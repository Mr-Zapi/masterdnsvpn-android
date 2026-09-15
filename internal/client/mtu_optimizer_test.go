package client

import (
	"testing"

	"masterdnsvpn-go/internal/config"
)

func mtuOutlierConnections() []Connection {
	return []Connection{
		{Key: "a", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 2536},
		{Key: "b", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3592},
		{Key: "c", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3592},
		{Key: "d", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3592},
	}
}

func TestSelectMTUScanConnectionsSpreadsSample(t *testing.T) {
	cfg := config.ClientConfig{MTUTestMaxResolvers: 4, RX_TX_Workers: 4}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")

	all := c.balancer.AllConnections()
	sample := c.selectMTUScanConnections(all)
	if len(sample) != 4 {
		t.Fatalf("expected a 4-resolver sample, got %d", len(sample))
	}

	seen := make(map[string]struct{}, len(sample))
	for _, conn := range sample {
		if _, ok := seen[conn.Key]; ok {
			t.Fatalf("sample contains duplicate resolver %q", conn.Key)
		}
		seen[conn.Key] = struct{}{}
	}

	// Unlimited returns the whole pool.
	c.cfg.MTUTestMaxResolvers = 0
	if got := len(c.selectMTUScanConnections(all)); got != len(all) {
		t.Fatalf("expected the full pool when unlimited, got %d", got)
	}
}

func TestCapResolverPoolKeepsBestMTU(t *testing.T) {
	cfg := config.ClientConfig{ResolverPoolSize: 2, RX_TX_Workers: 4}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	valid := []Connection{
		{Key: "a", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 1024},
		{Key: "b", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3592},
		{Key: "c", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 2536},
		{Key: "d", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3400},
	}

	kept, _, down, _ := c.capResolverPool(valid, 122, 1024, 0)
	if len(kept) != 2 {
		t.Fatalf("expected pool capped to 2, got %d", len(kept))
	}
	if down != 3400 {
		t.Fatalf("expected session download MTU 3400 over the best 2, got %d", down)
	}
	keptKeys := map[string]bool{}
	for _, conn := range kept {
		keptKeys[conn.Key] = true
	}
	if !keptKeys["b"] || !keptKeys["d"] {
		t.Fatalf("expected the two highest-MTU resolvers b and d, got %+v", keptKeys)
	}
}

func TestCapResolverPoolDisabledKeepsAll(t *testing.T) {
	cfg := config.ClientConfig{ResolverPoolSize: 0, RX_TX_Workers: 4}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	valid := []Connection{
		{Key: "a", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 1024},
		{Key: "b", IsValid: true, UploadMTUBytes: 122, DownloadMTUBytes: 3592},
	}
	kept, _, _, _ := c.capResolverPool(valid, 122, 1024, 0)
	if len(kept) != 2 {
		t.Fatalf("expected no cap when disabled, got %d", len(kept))
	}
}

func TestOptimizeMTUResolversPoolCapBoundsDrops(t *testing.T) {
	cfg := config.ClientConfig{
		AutoRemoveLowMTUServers: true,
		MTUOptimizerAggressive:  true,
		ResolverPoolSize:        3,
		RX_TX_Workers:           4,
	}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	// The inner optimizer may drop outliers, but never below the pool size.
	valid, _, down, _ := c.optimizeMTUResolversInner(mtuOutlierConnections())
	if len(valid) != 3 {
		t.Fatalf("expected the inner optimizer to keep the pool floor of 3, got %d", len(valid))
	}
	if down != 3592 {
		t.Fatalf("expected the low outlier dropped, session MTU 3592, got %d", down)
	}
}

func TestOptimizeMTUResolversSmallPoolStillOptimizesMTU(t *testing.T) {
	// When fewer resolvers are available than the pool wants, MTU optimization
	// must still run (the pool size cannot be met anyway).
	cfg := config.ClientConfig{
		AutoRemoveLowMTUServers: true,
		MTUOptimizerAggressive:  true,
		ResolverPoolSize:        32,
		RX_TX_Workers:           4,
	}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	valid, _, down, _ := c.optimizeMTUResolvers(mtuOutlierConnections())
	if len(valid) != 3 {
		t.Fatalf("expected the low outlier to be dropped, got %d resolvers", len(valid))
	}
	if down != 3592 {
		t.Fatalf("expected session download MTU 3592, got %d", down)
	}
}

func TestOptimizeMTUResolversDefaultKeepsLowOutlier(t *testing.T) {
	cfg := config.ClientConfig{
		AutoRemoveLowMTUServers: true,
		MTUOptimizerAggressive:  false,
		RX_TX_Workers:           4,
	}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	valid, _, down, _ := c.optimizeMTUResolvers(mtuOutlierConnections())
	if len(valid) != 4 {
		t.Fatalf("expected all 4 resolvers kept by default, got %d", len(valid))
	}
	if down != 2536 {
		t.Fatalf("expected default session download MTU 2536, got %d", down)
	}
}

func TestOptimizeMTUResolversAggressiveDropsLowOutlier(t *testing.T) {
	cfg := config.ClientConfig{
		AutoRemoveLowMTUServers: true,
		MTUOptimizerAggressive:  true,
		RX_TX_Workers:           4,
	}
	c := buildTestClientWithResolvers(cfg, "a", "b", "c", "d")

	valid, up, down, _ := c.optimizeMTUResolvers(mtuOutlierConnections())
	if len(valid) != 3 {
		t.Fatalf("expected the low outlier to be dropped, got %d resolvers", len(valid))
	}
	if down != 3592 {
		t.Fatalf("expected aggressive session download MTU 3592, got %d", down)
	}
	if up != 122 {
		t.Fatalf("expected upload MTU to stay 122, got %d", up)
	}
	for _, conn := range valid {
		if conn.Key == "a" {
			t.Fatalf("low download MTU outlier %q should have been dropped", conn.Key)
		}
	}
}
