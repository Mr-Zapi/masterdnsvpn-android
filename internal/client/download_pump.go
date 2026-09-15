// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package client provides the core logic for the MasterDnsVPN client.
// This file (download_pump.go) implements the async downlink poll engine.
//
// The tunnel is strictly request/response: the server returns at most one
// queued packet per incoming query (serveQueuedOrPong on the server). A normal
// download is therefore ACK-clocked at roughly one fragment per RTT per active
// resolver. The pump breaks that cap by keeping several empty requests
// (PINGs) in flight on the downlink resolver partition. Each empty request
// makes the server dequeue one queued download fragment.
//
// Unlike a blocking per-resolver worker, this engine never opens a socket per
// resolver and never blocks on a read. It enqueues pre-built PING DNS queries
// onto the shared tunnel writer queue (the same N UDP sockets used for uploads)
// and lets the normal RX pipeline consume the responses. Resource usage is
// therefore bounded by a small, fixed number of goroutines regardless of how
// many resolvers are configured; only per-resolver in-flight counters scale
// with the resolver count.
// ==============================================================================
package client

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
	VpnProto "masterdnsvpn-go/internal/vpnproto"
)

const (
	// downloadPumpIdlePollInterval is how long a sender sleeps while no data
	// stream is active, so an idle tunnel does not keep the radio busy.
	downloadPumpIdlePollInterval = 20 * time.Millisecond
	// downloadPumpMinSendInterval spaces back-to-back polls to the same
	// resolver so a single resolver cannot be flooded.
	downloadPumpMinSendInterval = 3 * time.Millisecond
	// downloadPumpBackoffInterval is used when every resolver is at its
	// in-flight target (or the writer queue is full).
	downloadPumpBackoffInterval = 2 * time.Millisecond
	// downloadPumpResponseTimeout bounds how long an unanswered poll keeps
	// counting against a resolver's in-flight budget.
	downloadPumpResponseTimeout = 5 * time.Second
	// downloadPumpRefreshInterval is how often the downlink resolver set is
	// re-read from the balancer (follows MTU/partition changes).
	downloadPumpRefreshInterval = time.Second
	// downloadPumpMaintenanceInterval is how often stalled polls are reaped and
	// the per-resolver in-flight target is adapted.
	downloadPumpMaintenanceInterval = 250 * time.Millisecond
	// downloadPumpMaxInFlightPerResolver caps the adaptive in-flight ramp so a
	// single resolver cannot be flooded even on very healthy paths.
	downloadPumpMaxInFlightPerResolver = 32
	// downloadPumpMaxInFlightTotal bounds the total number of outstanding polls
	// across every downlink resolver, so a very large resolver pool cannot
	// create an unbounded number of in-flight queries.
	downloadPumpMaxInFlightTotal = 4096
	// downloadPumpDataResponseSize is the response size above which a reply is
	// treated as carrying a download fragment rather than a bare PONG.
	downloadPumpDataResponseSize = 300
)

// downlinkPump is the shared state for the async downlink poll engine.
type downlinkPump struct {
	client *Client

	mu            sync.Mutex
	states        map[string]*downlinkPumpState
	order         []string
	rr            int
	inflightTotal int
	refreshedAt   time.Time
}

type downlinkPumpState struct {
	conn         Connection
	addr         *net.UDPAddr
	label        string
	inflight     int
	target       int
	lastSent     time.Time
	lastResp     time.Time
	lastDataResp time.Time
	seq          uint32
}

// startDownloadPumps launches the downlink poll engine if it is enabled in the
// config. It is called from StartAsyncRuntime (after the session is initialized)
// and tracked by asyncWG so StopAsyncRuntime waits for it.
func (c *Client) startDownloadPumps(ctx context.Context) {
	if c == nil || c.balancer == nil {
		return
	}

	if !c.downloadPumpEnabled() {
		return
	}

	pump := newDownlinkPump(c)
	pump.refresh(true)
	if len(pump.order) == 0 {
		return
	}

	// A handful of senders is enough: they only build PING packets and enqueue
	// them, they never block on I/O. Sending itself is done by the writer pool.
	senders := min(max(c.cfg.RX_TX_Workers, 2), 16)
	if senders > len(pump.order) {
		senders = len(pump.order)
	}
	if senders < 1 {
		senders = 1
	}

	c.downlinkPump.Store(pump)

	if c.log != nil {
		c.log.Infof(
			"<cyan>[PUMP]</cyan> Downlink pump enabled: <yellow>%d</yellow> resolver(s), <yellow>%d</yellow> sender(s), up to <yellow>%d</yellow> in-flight each",
			len(pump.order),
			senders,
			c.downloadPumpTarget(),
		)
	}

	for i := 0; i < senders; i++ {
		c.asyncWG.Add(1)
		go pump.sender(ctx)
	}
	c.asyncWG.Add(1)
	go pump.maintenance(ctx)
	c.asyncWG.Add(1)
	go c.runDownloadPumpReporter(ctx)
}

// noteDownlinkResponse is called from the RX path for every inbound tunnel
// datagram. It settles the in-flight budget of the sending downlink resolver.
// It is a no-op unless an async pump is installed and the sender is part of the
// downlink partition.
func (c *Client) noteDownlinkResponse(addr *net.UDPAddr, size int) {
	if c == nil || addr == nil {
		return
	}
	pump := c.downlinkPump.Load()
	if pump == nil {
		return
	}
	pump.noteResponse(addr, size)
}

// downloadPumpEnabled reports whether the pump should run. It is on when the
// directional resolver split reserves any downlink share, or when the legacy
// DOWNLOAD_PUMP_* settings are used.
func (c *Client) downloadPumpEnabled() bool {
	if c == nil {
		return false
	}
	return c.cfg.DownlinkResolversPercent > 0 ||
		c.cfg.DownloadPumpResolvers > 0 ||
		c.cfg.DownloadPumpResolversPercent > 0
}

// downloadPumpTarget is the configured per-resolver in-flight floor.
func (c *Client) downloadPumpTarget() int {
	if c == nil {
		return 1
	}
	target := c.cfg.DownloadPumpConcurrency
	if target < 1 {
		target = 1
	}
	if target > downloadPumpMaxInFlightPerResolver {
		target = downloadPumpMaxInFlightPerResolver
	}
	return target
}

// downloadPumpResolvers returns the resolvers the pump may poll. When the
// directional split is active this is exactly the downlink partition (disjoint
// from the uplink resolvers used for uploads). Otherwise it falls back to the
// legacy top-N selection over the whole active pool.
func (c *Client) downloadPumpResolvers() []Connection {
	if c == nil || c.balancer == nil {
		return nil
	}

	pool := c.balancer.DownloadConnections()
	if len(pool) == 0 {
		return nil
	}

	if c.cfg.DownlinkResolversPercent > 0 {
		return pool
	}

	n := c.effectiveDownloadPumpResolvers(len(pool))
	if n <= 0 {
		return nil
	}
	if n > len(pool) {
		n = len(pool)
	}
	// Legacy path: poll the resolvers with the best download capacity.
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].DownloadMTUBytes != pool[j].DownloadMTUBytes {
			return pool[i].DownloadMTUBytes > pool[j].DownloadMTUBytes
		}
		return pool[i].MTUResolveTime < pool[j].MTUResolveTime
	})
	return pool[:n]
}

// effectiveDownloadPumpResolvers returns how many resolvers to dedicate to
// download pumping in legacy mode. With a percentage configured, it is that
// share of the active pool (at least 1), optionally capped by
// DownloadPumpResolvers.
func (c *Client) effectiveDownloadPumpResolvers(active int) int {
	if c == nil || active <= 0 {
		return 0
	}

	absolute := c.cfg.DownloadPumpResolvers
	percent := c.cfg.DownloadPumpResolversPercent

	target := 0
	switch {
	case percent > 0:
		target = active * percent / 100
		if target < 1 {
			target = 1
		}
	case absolute > 0:
		target = absolute
	default:
		return 0
	}

	if absolute > 0 && target > absolute {
		target = absolute
	}
	if target > active {
		target = active
	}
	return target
}

func newDownlinkPump(c *Client) *downlinkPump {
	return &downlinkPump{
		client: c,
		states: make(map[string]*downlinkPumpState),
	}
}

// refresh rebuilds the downlink resolver set, preserving per-resolver in-flight
// state for resolvers that are still present. It is throttled unless force.
func (p *downlinkPump) refresh(force bool) {
	if p == nil || p.client == nil {
		return
	}

	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	if !force && !p.refreshedAt.IsZero() && now.Sub(p.refreshedAt) < downloadPumpRefreshInterval {
		return
	}
	p.refreshedAt = now

	resolvers := p.client.downloadPumpResolvers()
	target := p.client.downloadPumpTarget()

	next := make(map[string]*downlinkPumpState, len(resolvers))
	order := make([]string, 0, len(resolvers))
	inflightTotal := 0
	for _, conn := range resolvers {
		label := conn.ResolverLabel
		if label == "" {
			label = formatResolverEndpoint(conn.Resolver, conn.ResolverPort)
		}
		addr, err := p.client.getResolverUDPAddr(conn)
		if err != nil || addr == nil {
			continue
		}
		key := addr.String()
		state := p.states[key]
		if state == nil {
			state = &downlinkPumpState{target: target}
		}
		state.conn = conn
		state.addr = addr
		state.label = label
		if state.target < 1 {
			state.target = target
		}
		inflightTotal += state.inflight
		next[key] = state
		order = append(order, key)
	}

	p.states = next
	p.order = order
	p.inflightTotal = inflightTotal
	if p.rr >= len(p.order) {
		p.rr = 0
	}
}

// reserve picks the next eligible resolver and tentatively books one in-flight
// slot on it. The slot must be released with rollback on send failure or
// settled by noteResponse when a response arrives.
func (p *downlinkPump) reserve(now time.Time) *downlinkPumpState {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.inflightTotal >= downloadPumpMaxInFlightTotal {
		return nil
	}

	n := len(p.order)
	for i := 0; i < n; i++ {
		idx := (p.rr + i) % n
		state := p.states[p.order[idx]]
		if state == nil {
			continue
		}
		if state.inflight >= state.target {
			continue
		}
		if !state.lastSent.IsZero() && now.Sub(state.lastSent) < downloadPumpMinSendInterval {
			continue
		}
		state.inflight++
		state.lastSent = now
		p.inflightTotal++
		p.rr = (idx + 1) % n
		return state
	}
	return nil
}

func (p *downlinkPump) rollback(state *downlinkPumpState) {
	if p == nil || state == nil {
		return
	}
	p.mu.Lock()
	if state.inflight > 0 {
		state.inflight--
		if p.inflightTotal > 0 {
			p.inflightTotal--
		}
	}
	p.mu.Unlock()
}

// noteResponse settles an in-flight poll for the resolver that answered.
func (p *downlinkPump) noteResponse(addr *net.UDPAddr, size int) {
	if p == nil || addr == nil {
		return
	}

	key := addr.String()
	now := time.Now()

	p.mu.Lock()
	state := p.states[key]
	if state != nil {
		if state.inflight > 0 {
			state.inflight--
			if p.inflightTotal > 0 {
				p.inflightTotal--
			}
		}
		state.lastResp = now
		if size > downloadPumpDataResponseSize {
			state.lastDataResp = now
		}
	}
	p.mu.Unlock()

	if state == nil {
		return
	}

	client := p.client
	client.pumpResponses.Add(1)
	if size > downloadPumpDataResponseSize {
		client.pumpDataResponses.Add(1)
		client.pumpDataBytes.Add(int64(size))
	} else {
		client.pumpSmallResponse.Add(1)
	}
}

func (p *downlinkPump) sender(ctx context.Context) {
	defer p.client.asyncWG.Done()

	for {
		if ctx.Err() != nil {
			return
		}
		// Only pump while a real (non-control) stream is active.
		if !p.client.hasActiveDataStream() {
			if !sleepWithContext(ctx, downloadPumpIdlePollInterval) {
				return
			}
			continue
		}

		p.refresh(false)
		state := p.reserve(time.Now())
		if state == nil {
			if !sleepWithContext(ctx, downloadPumpBackoffInterval) {
				return
			}
			continue
		}

		if !p.send(ctx, state) {
			p.rollback(state)
			if !sleepWithContext(ctx, downloadPumpBackoffInterval) {
				return
			}
		}
	}
}

// send builds one PING query and hands it to the shared writer queue. Returning
// false releases the reserved in-flight slot.
func (p *downlinkPump) send(ctx context.Context, state *downlinkPumpState) bool {
	client := p.client

	payload, err := buildClientPingPayload()
	if err != nil {
		return false
	}

	p.mu.Lock()
	state.seq++
	seq := state.seq
	p.mu.Unlock()

	query, err := client.buildTunnelTXTQueryRaw(state.conn.Domain, VpnProto.BuildOptions{
		SessionID:      client.sessionID,
		SessionCookie:  client.sessionCookie,
		PacketType:     Enums.PACKET_PING,
		StreamID:       0,
		SequenceNum:    uint16(seq),
		FragmentID:     0,
		TotalFragments: 1,
		Payload:        payload,
	})
	if err != nil {
		return false
	}

	task := writerTask{
		frames: []encodedOutboundDatagram{{
			addr:      state.addr,
			serverKey: state.conn.Key,
			packet:    query,
		}},
	}

	select {
	case client.encodedTXChannel <- task:
		client.pumpRequests.Add(1)
		return true
	case <-ctx.Done():
		return false
	default:
		// Writer queue is full; back off and retry.
		return false
	}
}

// maintenance reaps stalled polls and adapts the per-resolver in-flight target
// (AIMD): ramp up while responses keep flowing, back off toward the configured
// floor when they stop.
func (p *downlinkPump) maintenance(ctx context.Context) {
	defer p.client.asyncWG.Done()

	ticker := time.NewTicker(downloadPumpMaintenanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.refresh(false)
			p.maintain(now)
		}
	}
}

func (p *downlinkPump) maintain(now time.Time) {
	base := p.client.downloadPumpTarget()

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.states {
		// Reap stalled in-flight polls so a dead resolver does not permanently
		// consume its budget.
		if state.inflight > 0 && !state.lastSent.IsZero() && now.Sub(state.lastSent) > downloadPumpResponseTimeout {
			p.inflightTotal -= state.inflight
			if p.inflightTotal < 0 {
				p.inflightTotal = 0
			}
			state.inflight = 0
		}

		// Adapt on real download delivery, not on bare PONGs: ramping while
		// only PONGs arrive would waste uplink on an idle server queue.
		if state.lastDataResp.IsZero() {
			continue
		}

		age := now.Sub(state.lastDataResp)
		switch {
		case age <= downloadPumpResponseTimeout/2:
			if state.target < downloadPumpMaxInFlightPerResolver {
				state.target++
			}
		case age > downloadPumpResponseTimeout:
			// Drop to a single in-flight poll once downloads have been idle
			// for a while, so an idle-but-connected stream does not keep the
			// whole downlink pool busy chasing PONGs.
			floor := base
			if age > 2*downloadPumpResponseTimeout {
				floor = 1
			}
			if state.target > floor {
				state.target--
			}
		}
	}
}

// runDownloadPumpReporter logs rolling 5s pump counters so it is obvious whether
// polls are pulling data or just being answered with PONGs.
func (c *Client) runDownloadPumpReporter(ctx context.Context) {
	defer c.asyncWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req := c.pumpRequests.Swap(0)
			res := c.pumpResponses.Swap(0)
			data := c.pumpDataResponses.Swap(0)
			small := c.pumpSmallResponse.Swap(0)
			dataBytes := c.pumpDataBytes.Swap(0)
			if req == 0 && res == 0 {
				continue
			}
			if c.log != nil {
				c.log.Infof(
					"<cyan>[PUMP]</cyan> 5s: requests=<yellow>%d</yellow> responses=<yellow>%d</yellow> data=<green>%d</green> small=<yellow>%d</yellow> down=<cyan>%d KB/s</cyan>",
					req, res, data, small, dataBytes/5/1024,
				)
			}
		}
	}
}

func (c *Client) hasActiveDataStream() bool {
	c.streamsMu.RLock()
	defer c.streamsMu.RUnlock()
	for id, s := range c.active_streams {
		if id != 0 && s != nil {
			return true
		}
	}
	return false
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
