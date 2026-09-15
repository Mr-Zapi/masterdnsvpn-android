// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================
// Package client provides the core logic for the MasterDnsVPN client.
// This file (resolver_probe.go) exposes protocol-accurate resolver probing for
// the Android public-DNS scanner.
//
// Unlike a plain DNS A-query, a MasterDnsVPN probe validates that a resolver can
// actually reach the tunnel server and round-trip our protocol, rejecting
// random/junk answers.
//
// Scanning is masscan-style: a single sender emits probe packets at a constant,
// evenly spaced rate, while independent receiver goroutines match replies back
// to probes using the random code embedded in each probe payload. There is no
// per-batch barrier, so the stream never stalls waiting for timeouts.
// ==============================================================================
package client

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	DnsParser "masterdnsvpn-go/internal/dnsparser"
	Enums "masterdnsvpn-go/internal/enums"
)

// ResolverProbeResult is the outcome of probing a single resolver against the
// tunnel server using the MasterDnsVPN protocol.
type ResolverProbeResult struct {
	Resolver    string
	Domain      string
	OK          bool
	RTT         time.Duration
	UploadMTU   int
	DownloadMTU int
	Reason      string
}

// ResolverProbeOptions controls how resolvers are probed.
type ResolverProbeOptions struct {
	// Parallelism bounds how many resolvers run full MTU discovery at once.
	// Only used when FullMTU is set. Defaults to 8.
	Parallelism int
	// Timeout is how long a probe may wait for its reply before it is retried
	// or marked as no-response. Defaults to 2s.
	Timeout time.Duration
	// Retries is the total number of send attempts per resolver. Defaults to 1.
	Retries int
	// FullMTU runs upload+download MTU discovery on resolvers that pass the
	// quick round-trip check, which is slower but reports usable MTU sizes.
	FullMTU bool
	// MaxPacketsPerSecond is the sustained probe packet rate. Packets are spread
	// evenly across the second. Defaults to 500; values <= 0 use the default.
	// The effective minimum interval is 1ms (1000 pps).
	MaxPacketsPerSecond int
	// DrainTimeout is how long to keep collecting replies after the last packet
	// has been sent, before stopping the scan. Defaults to 5s.
	DrainTimeout time.Duration
	// OnPhase, when set, is called as the scan enters a phase:
	// "quick" (find live servers) then, if FullMTU, "mtu" (MTU on live servers).
	OnPhase func(phase string, total int)
	// OnMTUProgress, when set, is called after each live resolver finishes its
	// full MTU discovery: (done, total).
	OnMTUProgress func(done int, total int)
}

// defaultMaxPacketsPerSecond is the scanner's default packet rate.
const defaultMaxPacketsPerSecond = 500

// maxProbeResponseBytes is the largest DNS reply we are willing to read.
const maxProbeResponseBytes = 65535

// pendingProbe tracks an in-flight probe so replies can be matched by code.
type pendingProbe struct {
	index    int
	addr     *net.UDPAddr
	size     int
	attempts int
	sentAt   time.Time
}

// packetPacer releases evenly spaced pacing tokens so packets are spread across
// the second instead of being sent in a burst. It is safe for concurrent use.
type packetPacer struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func newPacketPacer(pps int) *packetPacer {
	if pps <= 0 {
		return nil
	}
	interval := time.Second / time.Duration(pps)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return &packetPacer{interval: interval}
}

func (p *packetPacer) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	waitFor := p.next.Sub(now)
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()

	if waitFor <= 0 {
		return nil
	}

	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// resolverScan holds the shared state of one scan run.
type resolverScan struct {
	client      *Client
	ctx         context.Context
	connections []Connection
	addrs       []*net.UDPAddr
	results     []ResolverProbeResult
	onResult    func(ResolverProbeResult)

	timeout   time.Duration
	retries   int
	useBase64 bool
	pacer     *packetPacer
	probeSize map[string]int

	onPhase       func(phase string, total int)
	onMTUProgress func(done int, total int)

	mu        sync.Mutex
	finalized []bool
	pending   map[uint32]*pendingProbe

	completed atomic.Int64
	done      chan struct{}
	stop      chan struct{}
	wg        sync.WaitGroup

	socks4 []*net.UDPConn
	socks6 []*net.UDPConn
	rr4    atomic.Uint32
	rr6    atomic.Uint32
}

// ProbeResolvers probes every resolver currently loaded in the balancer using
// the MasterDnsVPN protocol and returns one result per resolver.
//
// A single sender emits packets at MaxPacketsPerSecond (spread evenly across
// the second); receiver goroutines collect and validate the answers. onResult,
// when non-nil, is invoked once per resolver as it is finalized and must be
// safe for concurrent use.
func (c *Client) ProbeResolvers(ctx context.Context, opts ResolverProbeOptions, onResult func(ResolverProbeResult)) []ResolverProbeResult {
	if c == nil || c.balancer == nil {
		return nil
	}

	connections := c.balancer.AllConnections()
	results := make([]ResolverProbeResult, len(connections))
	if len(connections) == 0 {
		return results
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	retries := opts.Retries
	if retries < 1 {
		retries = 1
	}
	pps := opts.MaxPacketsPerSecond
	if pps <= 0 {
		pps = defaultMaxPacketsPerSecond
	}

	scan := &resolverScan{
		client:        c,
		ctx:           ctx,
		connections:   connections,
		addrs:         make([]*net.UDPAddr, len(connections)),
		results:       results,
		onResult:      onResult,
		timeout:       timeout,
		retries:       retries,
		useBase64:     c.cfg.BaseEncodeData,
		pacer:         newPacketPacer(pps),
		probeSize:     make(map[string]int),
		onPhase:       opts.OnPhase,
		onMTUProgress: opts.OnMTUProgress,
		finalized:     make([]bool, len(connections)),
		pending:       make(map[uint32]*pendingProbe, len(connections)),
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
	}

	// Resolve targets and finalize obviously invalid ones.
	for i, conn := range connections {
		addr, err := net.ResolveUDPAddr("udp", conn.ResolverLabel)
		if err != nil {
			scan.finalize(i, false, 0, 0, 0, "invalid-resolver")
			continue
		}
		scan.addrs[i] = addr
		if _, ok := scan.probeSize[conn.Domain]; !ok {
			scan.probeSize[conn.Domain] = c.quickProbeUploadSize(conn.Domain)
		}
	}

	scan.openSockets()
	if len(scan.socks4) == 0 && len(scan.socks6) == 0 {
		for i := range connections {
			scan.finalize(i, false, 0, 0, 0, "no-socket")
		}
		return results
	}

	// Pace any probe that goes through the shared MTU probe path (full MTU).
	if scan.pacer != nil {
		gate := scan.pacer.wait
		c.probeGate.Store(&gate)
		defer c.probeGate.Store(nil)
	}

	for _, sock := range scan.socks4 {
		scan.wg.Add(1)
		go scan.readLoop(sock)
	}
	for _, sock := range scan.socks6 {
		scan.wg.Add(1)
		go scan.readLoop(sock)
	}
	scan.wg.Add(1)
	go scan.timeoutLoop()

	if scan.onPhase != nil {
		scan.onPhase("quick", len(connections))
	}

scanLoop:
	for i := range connections {
		if ctx.Err() != nil {
			break
		}
		if scan.addrs[i] == nil {
			continue
		}
		if scan.pacer != nil {
			if err := scan.pacer.wait(ctx); err != nil {
				break scanLoop
			}
		}
		scan.send(i, 1)
	}

	// After the last packet is sent, keep collecting replies for up to
	// DrainTimeout, then stop. If every probe is already finalized, done closes
	// first and the scan ends immediately.
	drain := opts.DrainTimeout
	if drain <= 0 {
		drain = 5 * time.Second
	}
	drainTimer := time.NewTimer(drain)
	defer drainTimer.Stop()

	select {
	case <-scan.done:
	case <-ctx.Done():
	case <-drainTimer.C:
		// Grace period elapsed: stop accepting replies and finish.
		close(scan.stop)
	}

	for _, sock := range scan.socks4 {
		_ = sock.Close()
	}
	for _, sock := range scan.socks6 {
		_ = sock.Close()
	}
	scan.wg.Wait()

	// Anything still outstanding has no chance of a reply now.
	scan.finalizePending()

	if opts.FullMTU {
		scan.runFullMTUPhase(opts.Parallelism)
	}

	return results
}

// finalizePending marks every still-in-flight probe as no-response. Must only be
// called after all reader/timeout goroutines have stopped.
func (s *resolverScan) finalizePending() {
	s.mu.Lock()
	remaining := make([]*pendingProbe, 0, len(s.pending))
	for code, probe := range s.pending {
		delete(s.pending, code)
		remaining = append(remaining, probe)
	}
	s.mu.Unlock()

	for _, probe := range remaining {
		s.finalize(probe.index, false, 0, 0, 0, "no-response")
	}
}

func (s *resolverScan) openSockets() {
	for range 4 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
		if err != nil {
			break
		}
		_ = conn.SetReadBuffer(4 << 20)
		s.socks4 = append(s.socks4, conn)
	}
	for range 2 {
		conn, err := net.ListenUDP("udp6", &net.UDPAddr{Port: 0})
		if err != nil {
			break
		}
		_ = conn.SetReadBuffer(4 << 20)
		s.socks6 = append(s.socks6, conn)
	}
}

func (s *resolverScan) sockFor(addr *net.UDPAddr) *net.UDPConn {
	if addr != nil && addr.IP.To4() != nil {
		if len(s.socks4) > 0 {
			idx := int(s.rr4.Add(1) % uint32(len(s.socks4)))
			return s.socks4[idx]
		}
	}
	if len(s.socks6) > 0 {
		idx := int(s.rr6.Add(1) % uint32(len(s.socks6)))
		return s.socks6[idx]
	}
	if len(s.socks4) > 0 {
		return s.socks4[0]
	}
	return nil
}

// send builds, records and transmits one probe for connections[index].
func (s *resolverScan) send(index int, attempts int) {
	conn := s.connections[index]
	addr := s.addrs[index]

	probeSize := s.probeSize[conn.Domain]
	if probeSize < minUploadMTUFloor {
		s.finalize(index, false, 0, 0, 0, "no-payload-capacity")
		return
	}

	payload, code, _, err := s.client.buildMTUProbePayload(probeSize)
	if err != nil {
		s.finalize(index, false, 0, 0, 0, "probe-build-failed")
		return
	}
	query, err := s.client.buildMTUProbeQuery(conn.Domain, Enums.PACKET_MTU_UP_REQ, payload)
	if err != nil {
		s.finalize(index, false, 0, 0, 0, "probe-build-failed")
		return
	}

	s.mu.Lock()
	s.pending[code] = &pendingProbe{
		index:    index,
		addr:     addr,
		size:     probeSize,
		attempts: attempts,
		sentAt:   time.Now(),
	}
	s.mu.Unlock()

	sock := s.sockFor(addr)
	if sock == nil {
		s.mu.Lock()
		delete(s.pending, code)
		s.mu.Unlock()
		s.finalize(index, false, 0, 0, 0, "no-socket")
		return
	}

	if _, err := sock.WriteToUDP(query, addr); err != nil {
		s.mu.Lock()
		delete(s.pending, code)
		s.mu.Unlock()
		s.finalize(index, false, 0, 0, 0, "send-failed")
	}
}

// readLoop continuously reads replies from one socket and validates them.
func (s *resolverScan) readLoop(sock *net.UDPConn) {
	defer s.wg.Done()

	buf := make([]byte, maxProbeResponseBytes)
	for {
		if s.ctx.Err() != nil {
			return
		}
		_ = sock.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := sock.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if s.ctx.Err() != nil {
				return
			}
			continue
		}

		packet, err := DnsParser.ExtractVPNResponse(buf[:n], s.useBase64)
		if err != nil {
			continue
		}
		if packet.PacketType != Enums.PACKET_MTU_UP_RES {
			continue
		}
		if len(packet.Payload) != 1+mtuProbeCodeLength+1 {
			continue
		}

		code := binary.BigEndian.Uint32(packet.Payload[:mtuProbeCodeLength])
		s.mu.Lock()
		probe, ok := s.pending[code]
		s.mu.Unlock()
		if !ok {
			continue
		}

		reportedSize := int(binary.BigEndian.Uint16(packet.Payload[mtuProbeCodeLength : mtuProbeCodeLength+2]))
		if reportedSize != probe.size {
			// Not the reply this probe is waiting for; leave it pending so the
			// timeout loop can still retry or expire it.
			continue
		}

		s.mu.Lock()
		delete(s.pending, code)
		s.mu.Unlock()
		s.finalize(probe.index, true, time.Since(probe.sentAt), 0, 0, "ok")
	}
}

// timeoutLoop expires probes that received no reply and retries them if allowed.
func (s *resolverScan) timeoutLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case <-s.stop:
			return
		case <-ticker.C:
		}

		now := time.Now()
		expired := make([]*pendingProbe, 0, 64)
		s.mu.Lock()
		for code, probe := range s.pending {
			if now.Sub(probe.sentAt) >= s.timeout {
				delete(s.pending, code)
				expired = append(expired, probe)
			}
		}
		s.mu.Unlock()

		for _, probe := range expired {
			if s.ctx.Err() != nil {
				return
			}
			if probe.attempts < s.retries {
				if s.pacer != nil {
					if err := s.pacer.wait(s.ctx); err != nil {
						return
					}
				}
				s.send(probe.index, probe.attempts+1)
				continue
			}
			s.finalize(probe.index, false, 0, 0, 0, "no-response")
		}
	}
}

// finalize records the first (and only) result for a resolver and notifies the
// caller. Later calls for the same index are ignored.
func (s *resolverScan) finalize(index int, ok bool, rtt time.Duration, up int, down int, reason string) {
	s.mu.Lock()
	if s.finalized[index] {
		s.mu.Unlock()
		return
	}
	s.finalized[index] = true
	conn := s.connections[index]
	result := ResolverProbeResult{
		Resolver:    conn.ResolverLabel,
		Domain:      conn.Domain,
		OK:          ok,
		RTT:         rtt,
		UploadMTU:   up,
		DownloadMTU: down,
		Reason:      reason,
	}
	s.results[index] = result
	s.mu.Unlock()

	if s.onResult != nil {
		s.onResult(result)
	}
	if s.completed.Add(1) == int64(len(s.connections)) {
		close(s.done)
	}
}

// runFullMTUPhase runs upload+download MTU discovery on resolvers that passed
// the quick round-trip check, updating their results in place.
func (s *resolverScan) runFullMTUPhase(parallelism int) {
	indices := make([]int, 0, len(s.results))
	for i := range s.results {
		if s.results[i].OK {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return
	}

	if s.onPhase != nil {
		s.onPhase("mtu", len(indices))
	}

	total := len(indices)
	var completed atomic.Int64

	workers := parallelism
	if workers < 1 {
		workers = 8
	}
	if workers > len(indices) {
		workers = len(indices)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if s.ctx.Err() != nil {
					return
				}
				s.client.probeFullMTU(s.ctx, &s.results[index])
				if s.onMTUProgress != nil {
					s.onMTUProgress(int(completed.Add(1)), total)
				}
			}
		}()
	}

	for _, index := range indices {
		select {
		case <-s.ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()
}

// probeFullMTU runs MTU discovery for one resolver using a dedicated socket.
func (c *Client) probeFullMTU(ctx context.Context, result *ResolverProbeResult) {
	transport, err := newUDPQueryTransport(result.Resolver)
	if err != nil {
		result.OK = false
		result.Reason = "dial-failed"
		return
	}
	defer transport.conn.Close()

	conn := Connection{
		Domain:        result.Domain,
		ResolverLabel: result.Resolver,
	}

	maxPayload := c.maxUploadMTUPayload(result.Domain)
	upOK, upBytes, _, upRTT, err := c.testUploadMTU(ctx, conn, transport, maxPayload)
	if err != nil || !upOK {
		result.OK = false
		result.Reason = "upload-mtu-failed"
		return
	}

	downOK, downBytes, downRTT, err := c.testDownloadMTU(ctx, conn, transport, upBytes)
	if err != nil || !downOK {
		result.OK = false
		result.Reason = "download-mtu-failed"
		return
	}

	result.UploadMTU = upBytes
	result.DownloadMTU = downBytes
	result.RTT = averageMTUProbeRTT(upRTT, downRTT)
}

// quickProbeUploadSize picks a small, always-safe payload size for the fast
// round-trip probe. It is capped by the domain's maximum encodable payload.
func (c *Client) quickProbeUploadSize(domain string) int {
	size := c.cfg.MinUploadMTU
	if size < minUploadMTUFloor {
		size = minUploadMTUFloor
	}
	if size > 96 {
		size = 96
	}

	maxPayload := c.maxUploadMTUPayload(domain)
	if maxPayload > 0 && size > maxPayload {
		size = maxPayload
	}
	return size
}
