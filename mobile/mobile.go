// ==============================================================================
// MasterDnsVPN - Android mobile binding
//
// This package is compiled into an Android library (mobile.aar) with gomobile.
// It exposes a tiny, gomobile-friendly API (only int/string/bool/error types)
// that the Android VpnService uses to:
//
//  1. Start the MasterDnsVPN client, which exposes a local SOCKS5 proxy on
//     127.0.0.1:<socksPort>.
//  2. Start an embedded tun2socks engine that reads the VpnService TUN file
//     descriptor and forwards every packet through that local SOCKS5 proxy.
//
// The result is a system-wide VPN on Android that carries all traffic through
// the DNS tunnel.
// ==============================================================================

package mobile

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"masterdnsvpn-go/internal/client"
	"masterdnsvpn-go/internal/config"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	socks5proxy "github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
)

var (
	mu      sync.Mutex
	running atomic.Bool
	// Fields below are guarded by mu. starting is true for the whole duration of
	// a Start call (including config bootstrap and the readiness wait) so Stop
	// can record stopRequested and Start can observe it before starting the
	// tun2socks engine.
	starting      bool
	stopRequested bool
	cancelFn      context.CancelFunc
	tunEngine     bool // whether the tun2socks engine has been started
	currentApp    *client.Client
)

// readyTimeout bounds how long Start waits for the client's local proxy listener
// to come up before giving up.
const readyTimeout = 120 * time.Second

// mobileSpeedOverrides returns client config values tuned for throughput on a
// phone. These do not change the wire protocol; they only adjust how the client
// uses it (fewer duplicate packets, larger per-packet payloads, more in flight).
func mobileSpeedOverrides() map[string]any {
	return map[string]any{
		// Do not send every packet twice: with one copy each packet is spread
		// across the resolver pool (multipath), which is the main throughput win.
		"PacketDuplicationCount": 1,
		// Allow more data per tunnel packet. Discovery still finds the largest
		// size the resolvers/server actually accept; these are upper bounds.
		// 150 matches the server's MAX_ALLOWED_CLIENT_UPLOAD_MTU; 4096 is its
		// MAX_ALLOWED_CLIENT_DOWNLOAD_MTU.
		"MaxUploadMTU":   150,
		"MaxDownloadMTU": 4096,
		// Reject resolvers that truncate large DNS answers. The session's
		// download MTU is the MINIMUM across active resolvers, so a single
		// low-MTU resolver otherwise drags the whole tunnel down to its size
		// (observed: 3603 found but only 653 selected). Raise/lower these if
		// the tunnel cannot find enough valid resolvers.
		"MinUploadMTU":   64,
		"MinDownloadMTU": 1024,
		// A fine MTU search finds a few more payload bytes per packet. The
		// extra overshoot probes only cost a little startup time.
		"MTUSearchTolerance": 8,
		// More queries in flight = more aggregate throughput on a high-RTT,
		// UDP request/response tunnel. Server caps are 255 workers / 20 batch /
		// 8000 ARQ window, so over-asking is clamped safely.
		"RX_TX_Workers":        12,
		"TunnelProcessWorkers": 12,
		"MaxPacketsPerBatch":   20,
		"ARQWindowSize":        8000,
		"ARQDataNackMaxGap":    255,
		// Raw binary payloads; base64 would inflate every packet by ~33%.
		"BaseEncodeData": false,
		// Discover resolvers on more parallel probes (faster startup only).
		"MTUTestParallelism": 32,
		// Bound the MTU scan of a huge scanned list to a spread sample; the
		// health loop validates the rest against the session MTU later.
		"MTUTestMaxResolvers": 128,
		// Drop low-MTU outliers so one weak resolver does not cap the tunnel.
		"MTUOptimizerAggressive": true,
		// Ask for faster loss recovery while keeping the retry pace sane.
		"ARQDataNackInitialDelaySeconds": 0.05,
		"ARQDataNackRepeatSeconds":       0.4,
		// Compress compressible payloads (falls back to raw when it would grow).
		"UploadCompressionType":   2,
		"DownloadCompressionType": 2,
		// Dedicated download pullers: keep extra empty poll requests in flight
		// on a dedicated share of the resolver pool so the server returns more
		// download fragments per RTT than the ACK-clocked flow alone. The pool
		// is split by direction: 25% of resolvers are reserved for download
		// pulling (downlink) and 75% carry upload/control traffic (uplink). The
		// two sets are disjoint. Set DOWNLINK to 0 to disable the split.
		"UplinkResolversPercent":   75,
		"DownlinkResolversPercent": 25,
		"DownloadPumpConcurrency":  8,
	}
}

// Protector is implemented on the Android side by the VpnService. Protect makes
// the socket with the given fd bypass the VPN tunnel (VpnService.protect), so
// direct DNS queries do not loop back into tun2socks. Returns true on success.
type Protector interface {
	Protect(fd int) bool
}

// makeDirectDNSResolver returns a resolver that forwards raw DNS queries to the
// given upstream servers (e.g. Yandex DNS) over a VPN-protected UDP socket,
// bypassing the tunnel. Servers are tried in order; the first answer wins.
func makeDirectDNSResolver(servers []string, protector Protector) func([]byte) ([]byte, bool) {
	dialer := &net.Dialer{
		Timeout: 4 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			if protector == nil {
				return nil
			}
			return c.Control(func(fd uintptr) {
				protector.Protect(int(fd))
			})
		},
	}

	return func(query []byte) ([]byte, bool) {
		for _, srv := range servers {
			conn, err := dialer.Dial("udp", srv)
			if err != nil {
				continue
			}
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			if _, err := conn.Write(query); err != nil {
				_ = conn.Close()
				continue
			}
			buf := make([]byte, 65535)
			n, err := conn.Read(buf)
			_ = conn.Close()
			if err != nil || n == 0 {
				continue
			}
			return buf[:n], true
		}
		return nil, false
	}
}

// parseDNSServers splits a comma/space/newline separated list into host:port
// entries, defaulting the port to 53.
func parseDNSServers(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(f); err != nil {
			f = net.JoinHostPort(f, "53")
		}
		out = append(out, f)
	}
	return out
}

// Start brings the tunnel up.
//
//	tunFd         : the file descriptor returned by VpnService.Builder.establish()
//	mtu           : MTU configured on the VpnService interface (e.g. 1500)
//	socksPort     : local port for the SOCKS5 proxy (e.g. 18000)
//	configB64     : the client configuration as a base64-encoded JSON string
//	                (the same value the desktop client accepts via -json_base64)
//	resolversText : the contents of client_resolvers.txt (resolver list)
//	directDNS     : optional comma/newline list of DNS servers (e.g.
//	                "77.88.8.8,77.88.8.1"). When non-empty, name resolution is
//	                done directly through these servers over a VPN-protected
//	                socket instead of through the DNS tunnel. Leave empty to
//	                resolve through the tunnel.
//	uplinkPercent : share (0..100) of active resolvers reserved for upload and
//	                control traffic. Negative means "use the config/default".
//	downlinkPercent: share (0..100) of active resolvers reserved for download
//	                pulling. Negative means "use the config/default". The two
//	                pools are disjoint (a resolver is never in both).
//	filesDir      : a writable directory (Context.getFilesDir().getAbsolutePath())
//	protector     : Android VpnService protector (may be nil); required for
//	                directDNS so the queries bypass the tunnel.
//
// It returns nil on success. On failure everything is torn down and a non-nil
// error is returned.
func Start(tunFd int, mtu int, socksPort int, configB64 string, resolversText string, directDNS string, uplinkPercent int, downlinkPercent int, filesDir string, protector Protector) error {
	mu.Lock()
	if running.Load() || starting {
		mu.Unlock()
		return fmt.Errorf("already running")
	}
	starting = true
	stopRequested = false
	mu.Unlock()

	// Do not hold mu across the (potentially long) readiness wait below, so
	// IsRunning() stays non-blocking and Stop() can cancel an in-progress start.
	defer func() {
		mu.Lock()
		starting = false
		mu.Unlock()
	}()

	// stopped reports whether Stop() was requested while we were starting.
	stopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stopRequested
	}

	if tunFd <= 0 {
		return fmt.Errorf("invalid tun fd: %d", tunFd)
	}
	if socksPort <= 0 || socksPort > 65535 {
		return fmt.Errorf("invalid socks port: %d", socksPort)
	}
	if mtu <= 0 {
		mtu = 1500
	}

	// Persist the resolver list to a file the client can read.
	resolversPath := filepath.Join(filesDir, "client_resolvers.txt")
	if err := os.WriteFile(resolversPath, []byte(resolversText), 0o600); err != nil {
		return fmt.Errorf("write resolvers file: %w", err)
	}

	// Force the client into a local SOCKS5 proxy on loopback, regardless of
	// whatever the shared config file happens to contain.
	overrides := config.ClientConfigOverrides{
		ResolversFilePath: &resolversPath,
		Values: map[string]any{
			"ProtocolType":    "SOCKS5",
			"ListenIP":        "127.0.0.1",
			"ListenPort":      socksPort,
			"LocalDNSEnabled": false,
		},
	}
	for key, value := range mobileSpeedOverrides() {
		overrides.Values[key] = value
	}

	// The UI-provided split overrides the built-in defaults when present.
	if uplinkPercent >= 0 && downlinkPercent >= 0 {
		overrides.Values["UplinkResolversPercent"] = uplinkPercent
		overrides.Values["DownlinkResolversPercent"] = downlinkPercent
	}

	cfg, err := config.LoadClientConfigFromJSONBase64WithOverrides(configB64, overrides)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	app, err := client.BootstrapLoadedConfig(cfg, "")
	if err != nil {
		return fmt.Errorf("bootstrap client: %w", err)
	}

	// Direct-DNS mode: resolve names straight through the given servers over a
	// VPN-protected socket, bypassing the DNS tunnel.
	dnsServers := parseDNSServers(directDNS)
	if len(dnsServers) > 0 {
		app.SetDirectDNSResolver(makeDirectDNSResolver(dnsServers, protector))
	}

	// Stop() may have been requested during the (uncancellable) bootstrap above.
	if stopped() {
		return fmt.Errorf("start cancelled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	mu.Lock()
	if stopRequested {
		mu.Unlock()
		cancel()
		return fmt.Errorf("start cancelled")
	}
	cancelFn = cancel
	currentApp = app
	mu.Unlock()

	// Run the DNS-tunnel client (owns the local SOCKS5 listener) in the
	// background. Run blocks until ctx is cancelled.
	runErrCh := make(chan error, 1)
	runDone := make(chan struct{})
	go func() {
		err := app.Run(ctx)
		runErrCh <- err
		close(runDone)
	}()

	// Do NOT start tun2socks until the client's local SOCKS5 listener is actually
	// accepting connections. With a large (or slow) resolver list the client
	// spends a long time MTU-testing before it opens the listener; forwarding
	// traffic during that window makes every TCP flow reset. Waiting here makes
	// the window invisible to apps.
	readyTimer := time.NewTimer(readyTimeout)
	defer readyTimer.Stop()

	ready := false
	select {
	case <-app.Ready():
		ready = true
	case <-runDone:
		// The client exited before it ever became ready (bad config, port in
		// use, MTU failure, ...). Fall through and report the error.
	case <-ctx.Done():
	case <-readyTimer.C:
	}

	if !ready {
		cancel()
		mu.Lock()
		cancelFn = nil
		currentApp = nil
		mu.Unlock()
		if err := drainRunError(runErrCh); err != nil {
			return fmt.Errorf("tunnel failed: %w", err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("start cancelled")
		}
		return fmt.Errorf("tunnel did not become ready")
	}

	// Start the tun2socks engine: read packets from the VpnService tun fd and
	// forward them to the local SOCKS5 proxy.
	key := &engine.Key{
		Device:                   fmt.Sprintf("fd://%d", tunFd),
		Proxy:                    fmt.Sprintf("socks5://127.0.0.1:%d", socksPort),
		MTU:                      mtu,
		LogLevel:                 "info",
		TCPModerateReceiveBuffer: true,
		TCPSendBufferSize:        "4m",
		TCPReceiveBufferSize:     "4m",
	}
	engine.Insert(key)
	engine.Start()

	// Stop() may have raced with us between the readiness check and the engine
	// start. If so, tear the engine back down instead of leaving it running
	// against a cancelled client.
	mu.Lock()
	cancelled := stopRequested || cancelFn == nil || ctx.Err() != nil
	if !cancelled {
		tunEngine = true
		running.Store(true)
	}
	mu.Unlock()

	if cancelled {
		engine.Stop()
		return fmt.Errorf("start cancelled")
	}

	// Override the tun2socks proxy so UDP :53 is resolved directly (see
	// dnsproxy.go) while TCP keeps going through the SOCKS5 proxy. This sidesteps
	// devices where the SOCKS5 UDP ASSOCIATE handshake fails. Must be set AFTER
	// engine.Start(), because Start() installs the key's proxy again.
	if len(dnsServers) > 0 {
		if base, err := socks5proxy.New(fmt.Sprintf("127.0.0.1:%d", socksPort), "", ""); err == nil {
			tunnel.T().SetProxy(&dnsProxy{
				base:    base,
				resolve: makeDirectDNSResolver(dnsServers, protector),
			})
		}
	}

	return nil
}

// drainRunError returns the client's Run error if the goroutine has already
// finished, without blocking.
func drainRunError(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	default:
		return nil
	}
}

// Stop tears the tunnel down. Safe to call when not running (including while a
// Start is still waiting, in which case it cancels that start).
func Stop() {
	mu.Lock()
	cancel := cancelFn
	cancelFn = nil
	engineOn := tunEngine
	tunEngine = false
	currentApp = nil
	// Record the stop so a Start that is still in its bootstrap/readiness phase
	// aborts instead of bringing the tunnel up after this call returns.
	if starting {
		stopRequested = true
	}
	wasActive := starting || engineOn || cancel != nil || running.Load()
	running.Store(false)
	mu.Unlock()

	if !wasActive {
		return
	}
	if engineOn {
		engine.Stop()
	}
	if cancel != nil {
		cancel()
	}
}

// IsRunning reports whether the tunnel is currently up. Lock-free so it is safe
// to call from the Android main thread while Start() is still working.
func IsRunning() bool {
	return running.Load()
}

// NotifyNetworkChanged tells the running client to rebuild its session, e.g.
// after the Android default network appears or changes. Safe to call when the
// tunnel is not running (it is then a no-op).
func NotifyNetworkChanged() {
	mu.Lock()
	app := currentApp
	mu.Unlock()
	if app != nil {
		app.NotifyNetworkChanged()
	}
}
