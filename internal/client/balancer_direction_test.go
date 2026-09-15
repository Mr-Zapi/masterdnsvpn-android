package client

import (
	"testing"

	Enums "masterdnsvpn-go/internal/enums"
)

func newDirectionalBalancer(t *testing.T, downlinkPercent int, downloadMTUs map[string]int) *Balancer {
	t.Helper()

	b := NewBalancer(BalancingRoundRobin, nil)
	keys := []string{"a", "b", "c", "d"}
	conns := make([]*Connection, 0, len(keys))
	for _, key := range keys {
		conns = append(conns, &Connection{Key: key, IsValid: true})
	}
	b.SetConnections(conns)
	b.SetDownlinkPercent(downlinkPercent)

	for _, key := range keys {
		_ = b.SetConnectionValidity(key, true)
		if mtu, ok := downloadMTUs[key]; ok {
			_ = b.SetConnectionMTU(key, 40, 60, mtu)
		}
	}
	return b
}

func TestBalancerDirectionalSplitIsDisjoint(t *testing.T) {
	b := newDirectionalBalancer(t, 25, map[string]int{"a": 100, "b": 200, "c": 300, "d": 400})

	down := b.DownloadConnections()
	up := b.UploadConnections()

	if len(down) != 1 {
		t.Fatalf("expected 1 downlink resolver, got %d", len(down))
	}
	if len(up) != 3 {
		t.Fatalf("expected 3 uplink resolvers, got %d", len(up))
	}
	if down[0].Key != "d" {
		t.Fatalf("expected highest download MTU resolver 'd' reserved for downlink, got %q", down[0].Key)
	}

	upKeys := make(map[string]bool, len(up))
	for _, conn := range up {
		upKeys[conn.Key] = true
	}
	for _, conn := range down {
		if upKeys[conn.Key] {
			t.Fatalf("resolver %q is present in both uplink and downlink pools", conn.Key)
		}
	}
}

func TestBalancerUploadSelectionExcludesDownlink(t *testing.T) {
	b := newDirectionalBalancer(t, 25, map[string]int{"a": 100, "b": 200, "c": 300, "d": 400})

	downKeys := make(map[string]bool)
	for _, conn := range b.DownloadConnections() {
		downKeys[conn.Key] = true
	}

	for i := 0; i < 40; i++ {
		best, ok := b.GetBestConnection()
		if !ok {
			t.Fatal("expected a best connection")
		}
		if downKeys[best.Key] {
			t.Fatalf("upload selection returned downlink resolver %q", best.Key)
		}

		targets, err := b.SelectTargets(Enums.PACKET_PING, 0, 2)
		if err != nil {
			t.Fatalf("SelectTargets failed: %v", err)
		}
		for _, target := range targets {
			if downKeys[target.Key] {
				t.Fatalf("upload target selection returned downlink resolver %q", target.Key)
			}
		}
	}
}

func TestBalancerPreferredDownlinkResolverIsRejectedForUpload(t *testing.T) {
	b := newDirectionalBalancer(t, 25, map[string]int{"a": 100, "b": 200, "c": 300, "d": 400})

	// Pin a stream route to the downlink resolver and make sure upload
	// selection refuses to keep using it.
	b.EnsureStream(7)
	b.mu.Lock()
	b.streamRoutes[7].PreferredResolverKey = "d"
	b.mu.Unlock()

	targets, err := b.SelectTargets(Enums.PACKET_STREAM_DATA, 7, 2)
	if err != nil {
		t.Fatalf("SelectTargets failed: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected at least one upload target")
	}
	for _, target := range targets {
		if target.Key == "d" {
			t.Fatal("preferred downlink resolver was used for upload")
		}
	}
}

func TestBalancerDirectionalSplitDisabledUsesWholePool(t *testing.T) {
	b := newDirectionalBalancer(t, 0, map[string]int{"a": 100, "b": 200, "c": 300, "d": 400})

	if got := len(b.UploadConnections()); got != 4 {
		t.Fatalf("expected all 4 resolvers to carry upload traffic, got %d", got)
	}
	// With the split disabled the download pool falls back to the whole active
	// pool, preserving legacy pump behaviour.
	if got := len(b.DownloadConnections()); got != 4 {
		t.Fatalf("expected download pool to fall back to all active resolvers, got %d", got)
	}
}

func TestBalancerDirectionalSplitKeepsAtLeastOneUplink(t *testing.T) {
	b := newDirectionalBalancer(t, 100, map[string]int{"a": 100, "b": 200, "c": 300, "d": 400})

	if got := len(b.UploadConnections()); got != 1 {
		t.Fatalf("expected exactly 1 uplink resolver when downlink is 100%%, got %d", got)
	}
	if got := len(b.DownloadConnections()); got != 3 {
		t.Fatalf("expected 3 downlink resolvers when downlink is 100%%, got %d", got)
	}
}
