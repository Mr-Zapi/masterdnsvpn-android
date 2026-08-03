// ==============================================================================
// Direct-DNS proxy override for tun2socks.
//
// On some Android devices the MasterDnsVPN client's SOCKS5 UDP ASSOCIATE
// handshake does not complete (tun2socks reports "client handshake: unexpected
// EOF"), so DNS carried as UDP never resolves — even though TCP works fine.
//
// To make DNS deterministic we replace the tun2socks proxy with a thin wrapper:
//   - TCP  (DialContext) is delegated to the real SOCKS5 proxy (works).
//   - UDP  :53 (DialUDP)  is answered locally by forwarding the raw DNS query
//     straight to the configured upstreams (e.g. Yandex DNS) over a
//     VPN-protected socket, bypassing the SOCKS path entirely.
//   - Any other UDP is dropped (returns an error), so QUIC etc. fall back to TCP.
// ==============================================================================

package mobile

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
)

// dnsProxy wraps a base proxy (SOCKS5 for TCP) and answers UDP :53 directly.
type dnsProxy struct {
	base    proxy.Proxy
	resolve func(query []byte) ([]byte, bool)
}

func (p *dnsProxy) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	return p.base.DialContext(ctx, m)
}

func (p *dnsProxy) DialUDP(m *M.Metadata) (net.PacketConn, error) {
	if m == nil || m.DstPort != 53 {
		return nil, fmt.Errorf("non-DNS UDP dropped")
	}
	return newDirectDNSPacketConn(p.resolve), nil
}

// directDNSPacketConn implements net.PacketConn. tun2socks writes the DNS query
// via WriteTo and reads the answer via ReadFrom. We resolve each query through
// the direct resolver and hand the answer back, tagged with the address the app
// originally queried (so tun2socks' NAT matches the reply to the flow).
type directDNSPacketConn struct {
	resolve func(query []byte) ([]byte, bool)

	respCh chan dnsResponse
	closed chan struct{}

	mu           sync.Mutex
	readDeadline time.Time
}

type dnsResponse struct {
	data []byte
	addr net.Addr
}

func newDirectDNSPacketConn(resolve func(query []byte) ([]byte, bool)) *directDNSPacketConn {
	return &directDNSPacketConn{
		resolve: resolve,
		respCh:  make(chan dnsResponse, 8),
		closed:  make(chan struct{}),
	}
}

func (c *directDNSPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	query := append([]byte(nil), b...)
	go func() {
		resp, ok := c.resolve(query)
		if !ok || len(resp) == 0 {
			return
		}
		select {
		case c.respCh <- dnsResponse{data: resp, addr: addr}:
		case <-c.closed:
		}
	}()
	return len(b), nil
}

func (c *directDNSPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.mu.Lock()
	deadline := c.readDeadline
	c.mu.Unlock()

	var timeout <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return 0, nil, timeoutError{}
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case r := <-c.respCh:
		n := copy(b, r.data)
		return n, r.addr, nil
	case <-timeout:
		return 0, nil, timeoutError{}
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *directDNSPacketConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func (c *directDNSPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

func (c *directDNSPacketConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

func (c *directDNSPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return nil
}

func (c *directDNSPacketConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
