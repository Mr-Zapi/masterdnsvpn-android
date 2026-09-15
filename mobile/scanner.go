// ==============================================================================
// MasterDnsVPN - Android public-DNS resolver scanner
//
// This file exposes a gomobile-friendly API that validates candidate DNS
// resolvers by speaking the real MasterDnsVPN protocol to the tunnel server
// (see internal/client/resolver_probe.go). A plain DNS query is not useful here
// because the tunnel domain does not resolve to an A record; only a protocol
// round-trip proves the resolver can actually reach the server and forward our
// packets.
// ==============================================================================

package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"masterdnsvpn-go/internal/client"
	"masterdnsvpn-go/internal/config"
)

var (
	scanMu       sync.Mutex
	scanCancel   context.CancelFunc
	scanActive   atomic.Bool
	scanTested   atomic.Int64
	scanOK       atomic.Int64
	scanPhase    atomic.Int32
	scanMtuDone  atomic.Int64
	scanMtuTotal atomic.Int64
)

type scanJSONResult struct {
	Resolver    string  `json:"resolver"`
	Domain      string  `json:"domain"`
	OK          bool    `json:"ok"`
	RTTMs       float64 `json:"rttMs"`
	UploadMTU   int     `json:"upMtu"`
	DownloadMTU int     `json:"downMtu"`
	Reason      string  `json:"reason"`
}

type scanJSONResponse struct {
	Tested    int              `json:"tested"`
	OK        int              `json:"ok"`
	Cancelled bool             `json:"cancelled"`
	Results   []scanJSONResult `json:"results"`
}

// TestResolvers probes candidate resolvers against the configured MasterDnsVPN
// server and returns a JSON report. It blocks until the scan finishes or is
// cancelled, so call it from a background thread.
//
//	filesDir      : app files directory (for the temporary resolver list)
//	configB64     : base64 JSON client config (needs DOMAINS + encryption)
//	candidatesText: newline/space separated resolver candidates ("ip" or "ip:port")
//	maxPps        : max probe packets per second, spread evenly (default 500)
//	timeoutSec    : per-attempt response timeout in seconds (default 2)
//	fullMtu       : run upload/download MTU discovery on resolvers that respond
//
// The returned JSON is: {"tested":N,"ok":M,"cancelled":bool,
// "results":[{"resolver":"1.2.3.4:53","domain":"...","ok":true,"rttMs":12,
// "upMtu":120,"downMtu":300,"reason":"ok"}]}
func TestResolvers(filesDir string, configB64 string, candidatesText string, maxPps int, timeoutSec float64, fullMtu bool) (string, error) {
	if !scanActive.CompareAndSwap(false, true) {
		return "", fmt.Errorf("a scan is already running")
	}
	defer scanActive.Store(false)

	if candidatesText == "" {
		return "", fmt.Errorf("no candidate resolvers provided")
	}
	if filesDir == "" {
		return "", fmt.Errorf("files directory is required")
	}

	scanTested.Store(0)
	scanOK.Store(0)
	scanPhase.Store(0)
	scanMtuDone.Store(0)
	scanMtuTotal.Store(0)

	resolversPath := filepath.Join(filesDir, "scan_resolvers.txt")
	if err := os.WriteFile(resolversPath, []byte(candidatesText), 0o600); err != nil {
		return "", fmt.Errorf("write scan resolvers: %w", err)
	}
	defer func() { _ = os.Remove(resolversPath) }()

	if maxPps < 1 {
		maxPps = 500
	}
	timeout := timeoutSec
	if timeout <= 0 {
		timeout = 2.0
	}
	// Full MTU discovery runs a binary search per resolver, so keep its
	// per-probe timeout short; the quick round-trip pass uses the drain timeout.
	mtuTimeout := timeout
	if mtuTimeout > 2.0 {
		mtuTimeout = 2.0
	}

	overrides := config.ClientConfigOverrides{
		ResolversFilePath: &resolversPath,
		Values: map[string]any{
			"ProtocolType":         "SOCKS5",
			"LocalDNSEnabled":      false,
			"LogLevel":             "ERROR",
			"MTUTestRetries":       1,
			"MTUTestTimeout":       mtuTimeout,
			"SaveMTUServersToFile": false,
		},
	}
	// Match the runtime MTU bounds so the scan reports the same payload sizes.
	for key, value := range mobileSpeedOverrides() {
		if key == "PacketDuplicationCount" {
			continue
		}
		overrides.Values[key] = value
	}

	cfg, err := config.LoadClientConfigFromJSONBase64WithOverrides(configB64, overrides)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}

	app, err := client.BootstrapLoadedConfig(cfg, "")
	if err != nil {
		return "", fmt.Errorf("bootstrap client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scanMu.Lock()
	scanCancel = cancel
	scanMu.Unlock()
	defer func() {
		scanMu.Lock()
		scanCancel = nil
		scanMu.Unlock()
		cancel()
	}()

	results := app.ProbeResolvers(ctx, client.ResolverProbeOptions{
		Timeout:             time.Duration(timeout * float64(time.Second)),
		Retries:             1,
		FullMTU:             fullMtu,
		MaxPacketsPerSecond: maxPps,
		OnPhase: func(phase string, total int) {
			if phase == "mtu" {
				scanPhase.Store(2)
				scanMtuTotal.Store(int64(total))
				scanMtuDone.Store(0)
			} else {
				scanPhase.Store(1)
			}
		},
		OnMTUProgress: func(done int, total int) {
			scanMtuDone.Store(int64(done))
			scanMtuTotal.Store(int64(total))
		},
	}, func(r client.ResolverProbeResult) {
		scanTested.Add(1)
		if r.OK {
			scanOK.Add(1)
		}
	})

	response := scanJSONResponse{
		Tested:    int(scanTested.Load()),
		OK:        int(scanOK.Load()),
		Cancelled: ctx.Err() != nil,
		Results:   make([]scanJSONResult, 0, len(results)),
	}

	for _, r := range results {
		if r.Resolver == "" {
			continue
		}
		response.Results = append(response.Results, scanJSONResult{
			Resolver:    r.Resolver,
			Domain:      r.Domain,
			OK:          r.OK,
			RTTMs:       float64(r.RTT.Microseconds()) / 1000.0,
			UploadMTU:   r.UploadMTU,
			DownloadMTU: r.DownloadMTU,
			Reason:      r.Reason,
		})
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		return "", fmt.Errorf("encode scan result: %w", err)
	}
	return string(encoded), nil
}

// ScanProgress returns the number of resolvers tested so far and how many of
// them responded successfully. It is safe to poll while TestResolvers runs.
func ScanProgress() string {
	return fmt.Sprintf(`{"tested":%d,"ok":%d,"running":%t,"phase":%d,"mtuDone":%d,"mtuTotal":%d}`,
		scanTested.Load(), scanOK.Load(), scanActive.Load(), scanPhase.Load(), scanMtuDone.Load(), scanMtuTotal.Load())
}

// CancelScan requests cancellation of the currently running scan, if any.
func CancelScan() {
	scanMu.Lock()
	cancel := scanCancel
	scanMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// IsScanning reports whether a resolver scan is currently in progress.
func IsScanning() bool {
	return scanActive.Load()
}
