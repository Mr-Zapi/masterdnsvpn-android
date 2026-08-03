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
	"syscall"
	"time"

	"masterdnsvpn-go/internal/client"
	"masterdnsvpn-go/internal/config"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	socks5proxy "github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
)

var (
	mu        sync.Mutex
	running   bool
	cancelFn  context.CancelFunc
	tunEngine bool // whether the tun2socks engine has been started
)

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
//	filesDir      : a writable directory (Context.getFilesDir().getAbsolutePath())
//	protector     : Android VpnService protector (may be nil); required for
//	                directDNS so the queries bypass the tunnel.
//
// It returns nil on success. On failure everything is torn down and a non-nil
// error is returned.
func Start(tunFd int, mtu int, socksPort int, configB64 string, resolversText string, directDNS string, filesDir string, protector Protector) error {
	mu.Lock()
	defer mu.Unlock()

	if running {
		return fmt.Errorf("already running")
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

	ctx, cancel := context.WithCancel(context.Background())

	// Run the DNS-tunnel client (owns the local SOCKS5 listener) in the
	// background. Run blocks until ctx is cancelled.
	go func() {
		_ = app.Run(ctx)
	}()

	// Start the tun2socks engine: read packets from the VpnService tun fd and
	// forward them to the local SOCKS5 proxy.
	key := &engine.Key{
		Device:   fmt.Sprintf("fd://%d", tunFd),
		Proxy:    fmt.Sprintf("socks5://127.0.0.1:%d", socksPort),
		MTU:      mtu,
		LogLevel: "info",
	}
	engine.Insert(key)
	engine.Start()

	// Override the tun2socks proxy so UDP :53 is resolved directly (see
	// dnsproxy.go) while TCP keeps going through the SOCKS5 proxy. This sidesteps
	// devices where the SOCKS5 UDP ASSOCIATE handshake fails.
	if len(dnsServers) > 0 {
		if base, err := socks5proxy.New(fmt.Sprintf("127.0.0.1:%d", socksPort), "", ""); err == nil {
			tunnel.T().SetProxy(&dnsProxy{
				base:    base,
				resolve: makeDirectDNSResolver(dnsServers, protector),
			})
		}
	}

	cancelFn = cancel
	tunEngine = true
	running = true
	return nil
}

// Stop tears the tunnel down. Safe to call when not running.
func Stop() {
	mu.Lock()
	defer mu.Unlock()

	if !running {
		return
	}
	if tunEngine {
		engine.Stop()
		tunEngine = false
	}
	if cancelFn != nil {
		cancelFn()
		cancelFn = nil
	}
	running = false
}

// IsRunning reports whether the tunnel is currently up.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}
