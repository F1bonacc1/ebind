package testutil

import (
	"context"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy relays TCP between NATS clients and a server, and can be frozen.
// Frozen, it keeps every socket open but drops all traffic both ways: a
// network partition the client has not noticed. nats.go sees no error (the
// connection stays "connected" until its ping interval runs out), so plain
// publishes report success, and only request/reply reveals the cut. Closing
// the client connection instead would make publishes fail at once, which is
// the easy case.
type Proxy struct {
	ln     net.Listener
	target string
	frozen atomic.Bool

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// StartProxy listens on a loopback port and relays to serverURL (for example
// embed.Node.ClientURL()). It is closed at test cleanup.
func StartProxy(t *testing.T, serverURL string) *Proxy {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("proxy: parse %q: %v", serverURL, err)
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy: listen: %v", err)
	}
	p := &Proxy{ln: ln, target: u.Host}
	go p.accept()
	t.Cleanup(p.Close)
	return p
}

// URL is the NATS URL clients connect to instead of the server's.
func (p *Proxy) URL() string { return "nats://" + p.ln.Addr().String() }

// Freeze starts dropping all traffic on every current and future connection.
func (p *Proxy) Freeze() { p.frozen.Store(true) }

// Close stops the proxy and closes every relayed connection.
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	_ = p.ln.Close()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

func (p *Proxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := new(net.Dialer).DialContext(context.Background(), "tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, server) {
			return
		}
		go p.relay(server, client)
		go p.relay(client, server)
	}
}

func (p *Proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	p.conns = append(p.conns, conns...)
	return true
}

// relay copies src to dst until either side fails, dropping what it reads
// while frozen.
func (p *Proxy) relay(dst, src net.Conn) {
	defer func() {
		_ = dst.Close()
		_ = src.Close()
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
