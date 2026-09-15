package client

import (
	"testing"
	"time"

	"masterdnsvpn-go/internal/config"
)

func newTestDownlinkPump(t *testing.T, downlinkPercent int) (*downlinkPump, *Client) {
	t.Helper()

	cfg := config.ClientConfig{
		DownlinkResolversPercent: downlinkPercent,
		DownloadPumpConcurrency:  6,
		RX_TX_Workers:            4,
	}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	pump := newDownlinkPump(c)
	pump.refresh(true)
	if len(pump.order) == 0 {
		t.Fatal("expected the pump to have resolvers")
	}
	return pump, c
}

func TestDownlinkPumpResolversUseDownlinkPartition(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	// 25% of 4 active resolvers = 1 downlink resolver.
	if len(pump.order) != 1 {
		t.Fatalf("expected 1 downlink pump resolver, got %d", len(pump.order))
	}
}

func TestDownlinkPumpReserveAndResponseSettleInFlight(t *testing.T) {
	pump, c := newTestDownlinkPump(t, 25)

	state := pump.reserve(time.Now())
	if state == nil {
		t.Fatal("expected to reserve an in-flight slot")
	}
	if state.inflight != 1 {
		t.Fatalf("expected 1 in-flight poll, got %d", state.inflight)
	}

	before := c.pumpResponses.Load()
	pump.noteResponse(state.addr, 400)
	if c.pumpResponses.Load() != before+1 {
		t.Fatal("expected a response to be counted")
	}

	pump.mu.Lock()
	inflight := state.inflight
	pump.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("expected in-flight to settle to 0, got %d", inflight)
	}
}

func TestDownlinkPumpReserveHonorsTarget(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	pump.mu.Lock()
	for _, state := range pump.states {
		state.target = 1
	}
	pump.mu.Unlock()

	if pump.reserve(time.Now()) == nil {
		t.Fatal("expected the first reservation to succeed")
	}
	if got := pump.reserve(time.Now()); got != nil {
		t.Fatal("expected a second reservation to be rejected at target")
	}
}

func TestDownlinkPumpRollbackReleasesInFlight(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	state := pump.reserve(time.Now())
	if state == nil {
		t.Fatal("expected to reserve an in-flight slot")
	}
	pump.rollback(state)

	pump.mu.Lock()
	inflight := state.inflight
	pump.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("expected rollback to release the slot, got %d", inflight)
	}
}

func TestDownlinkPumpMaintainReapsStalledPolls(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	state := pump.reserve(time.Now())
	if state == nil {
		t.Fatal("expected to reserve an in-flight slot")
	}
	state.lastSent = time.Now().Add(-2 * downloadPumpResponseTimeout)

	pump.maintain(time.Now())

	pump.mu.Lock()
	inflight := state.inflight
	pump.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("expected stalled poll to be reaped, got in-flight %d", inflight)
	}
}

func TestDownlinkPumpMaintainRampsTarget(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	pump.mu.Lock()
	var state *downlinkPumpState
	for _, s := range pump.states {
		state = s
	}
	state.lastDataResp = time.Now()
	state.target = 2
	pump.mu.Unlock()

	pump.maintain(time.Now())

	pump.mu.Lock()
	target := state.target
	pump.mu.Unlock()
	if target != 3 {
		t.Fatalf("expected target to ramp from 2 to 3, got %d", target)
	}
}

func TestDownlinkPumpMaintainDoesNotRampOnPong(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	pump.mu.Lock()
	var state *downlinkPumpState
	for _, s := range pump.states {
		state = s
	}
	state.lastResp = time.Now()
	state.target = 2
	pump.mu.Unlock()

	pump.maintain(time.Now())

	pump.mu.Lock()
	target := state.target
	pump.mu.Unlock()
	if target != 2 {
		t.Fatalf("expected target to stay at 2 on PONG-only traffic, got %d", target)
	}
}

func TestDownlinkPumpMaintainDecaysToSingleIdlePoll(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	pump.mu.Lock()
	var state *downlinkPumpState
	for _, s := range pump.states {
		state = s
	}
	state.lastDataResp = time.Now().Add(-3 * downloadPumpResponseTimeout)
	state.target = 8
	pump.mu.Unlock()

	for i := 0; i < 12; i++ {
		pump.maintain(time.Now())
	}

	pump.mu.Lock()
	target := state.target
	pump.mu.Unlock()
	if target != 1 {
		t.Fatalf("expected idle target to decay to 1, got %d", target)
	}
}

func TestWriterQueueHasPumpHeadroom(t *testing.T) {
	c := &Client{}
	c.encodedTXChannel = make(chan writerTask, 100)

	if !c.writerQueueHasPumpHeadroom() {
		t.Fatal("expected headroom on an empty writer queue")
	}
	for i := 0; i < 80; i++ {
		c.encodedTXChannel <- writerTask{}
	}
	if c.writerQueueHasPumpHeadroom() {
		t.Fatal("expected no pump headroom when the writer queue is mostly full")
	}
}

func TestDownlinkPumpGlobalInFlightCap(t *testing.T) {
	pump, _ := newTestDownlinkPump(t, 25)

	pump.mu.Lock()
	pump.inflightTotal = downloadPumpMaxInFlightTotal
	pump.mu.Unlock()

	if got := pump.reserve(time.Now()); got != nil {
		t.Fatal("expected reservation to be rejected at the global in-flight cap")
	}
}
