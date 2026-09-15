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
