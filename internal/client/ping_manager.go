// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	Enums "masterdnsvpn-go/internal/enums"
)

// In a DNS tunnel the server can only hand data back as the answer to a query,
// and it returns exactly one queued packet per inbound request
// (see server: serveQueuedOrPong). So download throughput is governed by how
// many tunnel queries the client keeps outstanding, not by the server.
//
// The ping loop is that puller: in aggressive (warm) mode it tops the number of
// outstanding pulls up to the current target instead of emitting a single ping
// per tick. Outstanding count self-paces against RTT - it drains as answers
// arrive and is refilled on the next tick.
//
// The right target depends on the path (RTT and loss) and cannot be a constant:
// measured on one link, 96 and 256 both performed far worse than 192, and the
// optimum shifts on a lossy mobile link. So the target is steered at runtime by
// hill-climbing on the observed inbound packet rate: keep moving in the current
// direction while the rate improves, reverse when it degrades. No explicit loss
// signal is needed - a collapse from over-driving shows up as a falling rate.
const (
	// Wake coalescing window. Small enough not to cap the pull rate.
	pingWakeThrottle = time.Millisecond
	// Shortest timer re-arm. Bounds the pull rate ceiling, not 100ms as before.
	pingMinCheckInterval = time.Millisecond
	// If nothing came back for this long, assume the outstanding pulls are lost
	// and reset, otherwise a stall would latch inflight high and stop refills.
	pingInflightStaleAfter = 2 * time.Second

	// Controller cadence and step. Two seconds keeps the rate estimate above
	// the per-window noise of a mobile link; one second random-walked.
	pingCtlInterval = 2 * time.Second
	pingCtlStep     = 16
	// Consecutive degraded windows required before reversing direction.
	pingCtlBadWindowsToReverse = 2
	// Rate must improve by more than this to count as progress, so noise does
	// not keep pushing the target up into a collapse.
	pingCtlImproveRatio = 1.03
)

type PingManager struct {
	client                *Client
	lastPingSentAt        atomic.Int64
	lastPongReceivedAt    atomic.Int64
	lastNonPingSentAt     atomic.Int64
	lastNonPongReceivedAt atomic.Int64
	nextPingSeq           atomic.Uint32

	// inflight counts tunnel pulls we have enqueued but not yet seen answered.
	// Incremented when a ping is queued, decremented by any inbound packet
	// (every inbound packet is the answer to one outstanding query).
	inflight      atomic.Int64
	lastInboundAt atomic.Int64

	// Hill-climbing controller state (pingLoop goroutine only, except the
	// counter which any receiver touches).
	inboundCount atomic.Int64
	ctlTarget      int
	ctlDir         int
	ctlLastRate    float64
	ctlLastAt      time.Time
	ctlLastCount   int64
	ctlBadWindows  int

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	wakeCh     chan struct{}
	lastWokeAt atomic.Int64
}

func newPingManager(client *Client) *PingManager {
	now := time.Now().UnixNano()
	p := &PingManager{
		client: client,
		wakeCh: make(chan struct{}, 1),
	}
	p.lastPingSentAt.Store(now)
	p.lastPongReceivedAt.Store(now)
	p.lastNonPingSentAt.Store(now)
	p.lastNonPongReceivedAt.Store(now)
	p.lastWokeAt.Store(now)
	p.lastInboundAt.Store(now)
	return p
}

// Start starts the autonomous ping loop.
func (p *PingManager) Start(parentCtx context.Context) {
	p.Stop() // Ensure old one is stopped

	p.ctx, p.cancel = context.WithCancel(parentCtx)
	p.inflight.Store(0)
	p.inboundCount.Store(0)
	p.lastInboundAt.Store(time.Now().UnixNano())

	// Start from the configured target and climb from there.
	p.ctlTarget = p.client.cfg.PingInflightTarget
	p.ctlDir = 1
	p.ctlLastRate = 0
	p.ctlLastCount = 0
	p.ctlLastAt = time.Now()

	p.wg.Add(1)
	go p.pingLoop()
}

// Stop stops the ping loop.
func (p *PingManager) Stop() {
	if p.cancel != nil {
		p.cancel()
		p.wg.Wait()
		p.cancel = nil
	}
}

func (p *PingManager) NotifyPacket(packetType uint8, isInbound bool) {
	if p == nil {
		return
	}

	isPing := packetType == Enums.PACKET_PING
	isPong := packetType == Enums.PACKET_PONG

	now := time.Now().UnixNano()

	if isInbound {
		// Every inbound packet answers one outstanding query.
		p.lastInboundAt.Store(now)
		p.inboundCount.Add(1)
		if p.inflight.Add(-1) < 0 {
			p.inflight.Store(0)
		}

		if isPong {
			p.lastPongReceivedAt.Store(now)
		} else {
			p.lastNonPongReceivedAt.Store(now)
			p.wake(now)
		}
	} else {
		if isPing {
			p.lastPingSentAt.Store(now)
		} else {
			p.lastNonPingSentAt.Store(now)
			p.wake(now)
		}
	}
}

func (p *PingManager) wake(now int64) {
	if now-p.lastWokeAt.Load() < int64(pingWakeThrottle) {
		return
	}
	p.lastWokeAt.Store(now)
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *PingManager) nextInterval(nowNano int64) time.Duration {
	lastNonPingSent := p.lastNonPingSentAt.Load()
	lastNonPongRecv := p.lastNonPongReceivedAt.Load()

	// Use fast int64 comparisons for intervals
	warmThresholdNano := int64(p.client.cfg.PingWarmThreshold())

	if nowNano-lastNonPingSent < warmThresholdNano || nowNano-lastNonPongRecv < warmThresholdNano {
		return p.client.cfg.PingAggressiveInterval()
	}

	idleSent := nowNano - lastNonPingSent
	idleRecv := nowNano - lastNonPongRecv
	minIdle := idleSent
	if idleRecv < minIdle {
		minIdle = idleRecv
	}

	coolThresholdNano := int64(p.client.cfg.PingCoolThreshold())
	coldThresholdNano := int64(p.client.cfg.PingColdThreshold())
	switch {
	case minIdle < coolThresholdNano:
		return p.client.cfg.PingLazyInterval()
	case minIdle < coldThresholdNano:
		return p.client.cfg.PingCooldownInterval()
	default:
		return p.client.cfg.PingColdInterval()
	}
}

// steerTarget hill-climbs the inflight target on the measured inbound rate.
// Called from pingLoop only.
func (p *PingManager) steerTarget(now time.Time) {
	cfg := p.client.cfg
	if !cfg.PingInflightAdaptive {
		p.ctlTarget = cfg.PingInflightTarget
		return
	}

	elapsed := now.Sub(p.ctlLastAt)
	if elapsed < pingCtlInterval {
		return
	}

	count := p.inboundCount.Load()
	rate := float64(count-p.ctlLastCount) / elapsed.Seconds()
	p.ctlLastCount = count
	p.ctlLastAt = now

	// First window only establishes a baseline.
	if p.ctlLastRate == 0 {
		p.ctlLastRate = rate
		return
	}

	// Too little traffic to judge: leave the target where it is.
	if rate < 1 && p.ctlLastRate < 1 {
		p.ctlLastRate = rate
		return
	}

	switch {
	case rate > p.ctlLastRate*pingCtlImproveRatio:
		// Still gaining - keep going the same way.
		p.ctlBadWindows = 0
	case rate*pingCtlImproveRatio < p.ctlLastRate:
		// One bad window on a mobile link is usually noise. Reverse only on a
		// sustained drop, otherwise the target random-walks off the peak.
		p.ctlBadWindows++
		if p.ctlBadWindows < pingCtlBadWindowsToReverse {
			p.ctlLastRate = rate
			return
		}
		p.ctlBadWindows = 0
		p.ctlDir = -p.ctlDir
	default:
		// Flat: we are at the plateau, stop pushing further out.
		p.ctlBadWindows = 0
		p.ctlLastRate = rate
		return
	}

	p.ctlTarget += p.ctlDir * pingCtlStep
	if p.ctlTarget >= cfg.PingInflightTarget {
		// Sit at the ceiling rather than bouncing off it: the configured value
		// is the best known target, so only a measured drop should pull us down.
		p.ctlTarget = cfg.PingInflightTarget
		p.ctlDir = 1
	}
	if p.ctlTarget < cfg.PingInflightMin {
		p.ctlTarget = cfg.PingInflightMin
		p.ctlDir = 1
	}
	p.ctlLastRate = rate
}

// queuePing enqueues one pull query on stream 0 and counts it as outstanding.
func (p *PingManager) queuePing() bool {
	if !p.client.SessionReady() {
		return false
	}

	payload, err := buildClientPingPayload()
	if err != nil {
		return false
	}

	// Use Stream 0 for pings
	p.client.streamsMu.RLock()
	s0 := p.client.active_streams[0]
	p.client.streamsMu.RUnlock()

	if s0 == nil {
		return false
	}

	s0.PushTXPacket(
		Enums.DefaultPacketPriority(Enums.PACKET_PING),
		Enums.PACKET_PING,
		p.nextPingSequence(),
		0,
		0,
		0,
		0,
		payload,
	)
	// Counted at enqueue time so a burst cannot be re-issued on the next tick
	// before the dispatcher has put these on the wire.
	p.inflight.Add(1)
	return true
}

func (p *PingManager) pingLoop() {
	defer p.wg.Done()

	p.client.log.Debugf("\U0001F3D3 <cyan>Ping Manager loop started</cyan>")
	timer := time.NewTimer(p.client.cfg.PingAggressiveInterval())
	defer timer.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wakeCh:
		case <-timer.C:
		}

		now := time.Now()
		nowNano := now.UnixNano()
		interval := p.nextInterval(nowNano)
		lastPing := p.lastPingSentAt.Load()

		// Only pull hard while warm; idle modes keep the original keepalive.
		target := 1
		if interval <= p.client.cfg.PingAggressiveInterval() {
			p.steerTarget(now)
			if p.ctlTarget > target {
				target = p.ctlTarget
			}
		}

		if nowNano-p.lastInboundAt.Load() > int64(pingInflightStaleAfter) {
			p.inflight.Store(0)
		}

		if target > 1 {
			for p.inflight.Load() < int64(target) {
				if !p.queuePing() {
					break
				}
			}
		} else if nowNano-lastPing >= int64(interval) {
			p.queuePing()
		}

		checkInterval := interval / 2
		if checkInterval < pingMinCheckInterval {
			checkInterval = pingMinCheckInterval
		}
		if checkInterval > 1*time.Second {
			checkInterval = 1 * time.Second
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(checkInterval)
	}
}

func (p *PingManager) nextPingSequence() uint16 {
	if p == nil {
		return 0
	}
	return uint16(p.nextPingSeq.Add(1))
}

func buildClientPingPayload() ([]byte, error) {
	// Pre-allocate the fixed size payload to avoid multiple allocations and appends
	payload := make([]byte, 7)
	payload[0] = 'P'
	payload[1] = 'O'
	payload[2] = ':'

	// Use rand.Read directly into the pre-allocated buffer starting at index 3
	if _, err := rand.Read(payload[3:]); err != nil {
		return nil, err
	}
	return payload, nil
}
