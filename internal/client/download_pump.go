// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package client provides the core logic for the MasterDnsVPN client.
// This file (download_pump.go) implements optional "download pullers".
//
// The tunnel is strictly request/response: the server returns at most one
// queued packet per incoming query (serveQueuedOrPong on the server). A normal
// download is therefore ACK-clocked at roughly one fragment per RTT per active
// resolver. The pump breaks that cap by keeping several extra empty requests
// (PINGs) in flight on a small number of dedicated resolvers. Each empty
// request makes the server dequeue one queued download fragment.
//
// Responses are fed straight into the normal packet pipeline
// (handleInboundPacket), so ARQ, stream reassembly, ACK generation and the
// balancer's resolver statistics all keep working unchanged. This does not
// change the wire protocol.
//
// Because the pump owns its own UDP sockets it never touches the dispatcher,
// planner or writer stages, so polling no longer competes with upstream data
// for the shared transmit pipeline.
// ==============================================================================
package client

import (
	"context"
	"net"
	"sort"
	"sync/atomic"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
	VpnProto "masterdnsvpn-go/internal/vpnproto"
)

const (
	// downloadPumpIdlePollInterval is how long a pump worker sleeps when no
	// data stream is active, so an idle tunnel does not keep the radio busy.
	downloadPumpIdlePollInterval = 20 * time.Millisecond
	// downloadPumpReadTimeout bounds a single poll. It also bounds how long
	// shutdown can wait on an in-flight poll.
	downloadPumpReadTimeout = 1500 * time.Millisecond
	// downloadPumpSelectInterval throttles re-sorting the active resolver set
	// while pumps are polling.
	downloadPumpSelectInterval = time.Second
)

type downloadPump struct {
	client *Client
	// slot identifies which of the N best resolvers this worker follows. The
	// worker re-reads its current target each poll so the dedicated download
	// resolvers follow MTU changes discovered after startup.
	slot int
	seq  atomic.Uint32
}

// startDownloadPumps spawns the dedicated pollers if enabled in the config.
// It is called from StartAsyncRuntime (after the session is initialized) and
// tracked by asyncWG so StopAsyncRuntime waits for them.
func (c *Client) startDownloadPumps(ctx context.Context) {
	if c == nil || c.balancer == nil {
		return
	}

	if c.cfg.DownloadPumpResolvers <= 0 && c.cfg.DownloadPumpResolversPercent <= 0 {
		return
	}
	concurrency := c.cfg.DownloadPumpConcurrency
	if concurrency < 1 {
		concurrency = 1
	}

	count := c.effectiveDownloadPumpResolvers(c.balancer.ActiveCount())
	if count <= 0 {
		return
	}

	conns := c.downloadPumpConnections(count, true)
	if len(conns) == 0 {
		return
	}
	if c.log != nil {
		c.log.Infof(
			"<cyan>[PUMP]</cyan> Download pump enabled: <yellow>%d</yellow> resolver(s) x <yellow>%d</yellow> in-flight requests",
			len(conns),
			concurrency,
		)
	}

	for i := range conns {
		p := &downloadPump{client: c, slot: i}
		for j := 0; j < concurrency; j++ {
			c.asyncWG.Add(1)
			go p.worker(ctx)
		}
	}
	c.asyncWG.Add(1)
	go c.runDownloadPumpReporter(ctx)
}

// effectiveDownloadPumpResolvers returns how many resolvers to dedicate to
// download pumping. With a percentage configured, it is that share of the
// active pool (at least 1), optionally capped by DownloadPumpResolvers.
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

// downloadPumpConnections picks the best active resolvers for pumping: highest
// download MTU first, then lowest resolve time. The result is cached briefly so
// the selection follows MTU updates without re-sorting on every poll.
func (c *Client) downloadPumpConnections(n int, forceRefresh bool) []Connection {
	if c == nil || c.balancer == nil || n <= 0 {
		return nil
	}

	now := time.Now()
	c.pumpSelectMu.Lock()
	if !forceRefresh && c.pumpSelect != nil && now.Sub(c.pumpSelectAt) < downloadPumpSelectInterval {
		selected := c.pumpSelect
		c.pumpSelectMu.Unlock()
		if n > len(selected) {
			n = len(selected)
		}
		return selected[:n]
	}
	c.pumpSelectMu.Unlock()

	active := c.balancer.ActiveConnections()
	if len(active) == 0 {
		c.pumpSelectMu.Lock()
		c.pumpSelect = nil
		c.pumpSelectAt = now
		c.pumpSelectMu.Unlock()
		return nil
	}

	sort.SliceStable(active, func(i, j int) bool {
		if active[i].DownloadMTUBytes != active[j].DownloadMTUBytes {
			return active[i].DownloadMTUBytes > active[j].DownloadMTUBytes
		}
		return active[i].MTUResolveTime < active[j].MTUResolveTime
	})

	if n > len(active) {
		n = len(active)
	}
	selected := active[:n]

	c.pumpSelectMu.Lock()
	c.pumpSelect = selected
	c.pumpSelectAt = now
	c.pumpSelectMu.Unlock()
	return selected
}

// downloadPumpConnectionAt returns the current slot-th best resolver, or false
// when the active set is smaller than the number of pump slots.
func (c *Client) downloadPumpConnectionAt(slot int) (Connection, bool) {
	if c == nil || c.balancer == nil {
		return Connection{}, false
	}
	count := c.effectiveDownloadPumpResolvers(c.balancer.ActiveCount())
	conns := c.downloadPumpConnections(count, false)
	if slot < 0 || slot >= len(conns) {
		return Connection{}, false
	}
	return conns[slot], true
}

func (p *downloadPump) worker(ctx context.Context) {
	defer p.client.asyncWG.Done()

	var (
		conn         *net.UDPConn
		connResolver string
		localAddr    string
		remoteAddr   *net.UDPAddr
	)
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	buf := make([]byte, 65535)

	for {
		if ctx.Err() != nil {
			return
		}
		// Only pump while a real (non-control) stream is active.
		if !p.client.hasActiveDataStream() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(downloadPumpIdlePollInterval):
			}
			continue
		}

		current, ok := p.client.downloadPumpConnectionAt(p.slot)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(downloadPumpIdlePollInterval):
			}
			continue
		}

		// Follow the MTU-driven selection: switch to the current best resolver
		// for this slot whenever it changes.
		if conn == nil || current.ResolverLabel != connResolver {
			if conn != nil {
				_ = conn.Close()
				conn = nil
			}
			dialed, err := dialUDPResolver(current.ResolverLabel)
			if err != nil {
				if p.client.log != nil {
					p.client.log.Debugf("<yellow>[PUMP]</yellow> dial %s failed: %v", current.ResolverLabel, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(downloadPumpIdlePollInterval):
				}
				continue
			}
			conn = dialed
			connResolver = current.ResolverLabel
			localAddr = ""
			if conn.LocalAddr() != nil {
				localAddr = conn.LocalAddr().String()
			}
			remoteAddr = nil
			if conn.RemoteAddr() != nil {
				remoteAddr, _ = conn.RemoteAddr().(*net.UDPAddr)
			}
		}

		payload, err := buildClientPingPayload()
		if err != nil {
			continue
		}
		query, err := p.client.buildTunnelTXTQueryRaw(current.Domain, VpnProto.BuildOptions{
			SessionID:      p.client.sessionID,
			SessionCookie:  p.client.sessionCookie,
			PacketType:     Enums.PACKET_PING,
			StreamID:       0,
			SequenceNum:    uint16(p.seq.Add(1)),
			FragmentID:     0,
			TotalFragments: 1,
			Payload:        payload,
		})
		if err != nil {
			continue
		}

		_ = conn.SetWriteDeadline(time.Now().Add(downloadPumpReadTimeout))
		if _, err := conn.Write(query); err != nil {
			// Force a fresh dial (and re-selection) next iteration.
			_ = conn.Close()
			conn = nil
			connResolver = ""
			continue
		}
		p.client.pumpRequests.Add(1)
		_ = conn.SetReadDeadline(time.Now().Add(downloadPumpReadTimeout))
		n, err := conn.Read(buf)
		if err != nil || n < 12 {
			continue
		}
		p.client.pumpResponses.Add(1)
		if n > 300 {
			p.client.pumpDataResponses.Add(1)
			p.client.pumpDataBytes.Add(int64(n))
		} else {
			p.client.pumpSmallResponse.Add(1)
		}
		// Drop straight into the normal RX/ARQ pipeline. handleInboundPacket
		// copies what it needs, so reusing buf is safe (same contract as the
		// normal processor worker).
		p.client.handleInboundPacket(buf[:n], remoteAddr, localAddr)
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
