// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"masterdnsvpn-go/internal/config"
)

func bootstrapProbeTestClient(t *testing.T, resolvers string) *Client {
	t.Helper()

	dir := t.TempDir()
	resolversPath := filepath.Join(dir, "resolvers.txt")
	if err := os.WriteFile(resolversPath, []byte(resolvers), 0o600); err != nil {
		t.Fatalf("write resolvers: %v", err)
	}

	payload := base64.StdEncoding.EncodeToString([]byte(
		`{"DOMAINS":["test.example.com"],"DATA_ENCRYPTION_METHOD":0,"ENCRYPTION_KEY":"probe-test-key"}`,
	))

	cfg, err := config.LoadClientConfigFromJSONBase64WithOverrides(payload, config.ClientConfigOverrides{
		ResolversFilePath: &resolversPath,
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	app, err := BootstrapLoadedConfig(cfg, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return app
}

func TestProbeResolversClosedPortFails(t *testing.T) {
	app := bootstrapProbeTestClient(t, "127.0.0.1:1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := app.ProbeResolvers(ctx, ResolverProbeOptions{
		Parallelism: 1,
		Timeout:     300 * time.Millisecond,
		Retries:     1,
	}, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].OK {
		t.Fatalf("expected closed-port probe to fail, got %+v", results[0])
	}
	if results[0].Reason == "" {
		t.Fatalf("expected a failure reason, got %+v", results[0])
	}
	if results[0].Resolver != "127.0.0.1:1" {
		t.Fatalf("unexpected resolver label %q", results[0].Resolver)
	}
}

func TestProbeResolversInvokesCallback(t *testing.T) {
	app := bootstrapProbeTestClient(t, "127.0.0.1:1\n127.0.0.2:1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var callbacks int
	results := app.ProbeResolvers(ctx, ResolverProbeOptions{
		Parallelism: 2,
		Timeout:     200 * time.Millisecond,
		Retries:     1,
	}, func(ResolverProbeResult) {
		callbacks++
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if callbacks != 2 {
		t.Fatalf("expected 2 callbacks, got %d", callbacks)
	}
}

func TestProbeResolversEmptyReturnsEmpty(t *testing.T) {
	app := bootstrapProbeTestClient(t, "127.0.0.1:1\n")
	app.balancer.SetConnections(nil)

	results := app.ProbeResolvers(context.Background(), ResolverProbeOptions{}, nil)
	if len(results) != 0 {
		t.Fatalf("expected no results, got %d", len(results))
	}
}

func TestPacketPacerInterval(t *testing.T) {
	if p := newPacketPacer(0); p != nil {
		t.Fatal("expected nil pacer for non-positive rate")
	}

	p := newPacketPacer(500)
	if p == nil {
		t.Fatal("expected non-nil pacer")
	}
	if p.interval != 2*time.Millisecond {
		t.Fatalf("expected 2ms interval at 500 pps, got %s", p.interval)
	}

	fast := newPacketPacer(1_000_000)
	if fast == nil || fast.interval != time.Millisecond {
		t.Fatalf("expected interval clamped to 1ms, got %v", fast)
	}
}

func TestProbeResolversPacingCountsAllResolvers(t *testing.T) {
	app := bootstrapProbeTestClient(t, "127.0.0.1:1\n127.0.0.2:1\n127.0.0.3:1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := app.ProbeResolvers(ctx, ResolverProbeOptions{
		Parallelism:         2,
		Timeout:             200 * time.Millisecond,
		Retries:             1,
		MaxPacketsPerSecond: 500,
	}, nil)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Resolver == "" {
			t.Fatalf("result %d was not populated: %+v", i, r)
		}
	}
}
