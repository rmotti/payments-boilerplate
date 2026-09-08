// Package chaos contains reusable infrastructure fault controls for the
// recovery and backlog suites. Tests decide when to change state explicitly;
// production code has no switch that enables these faults.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

type proxyMode int

const (
	modeForward proxyMode = iota
	modeRejectNew
	modeSilent
)

// TCPProxy forwards TCP connections to one upstream and can deterministically
// interrupt the transport without knowing its application protocol.
type TCPProxy struct {
	listener net.Listener
	upstream string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	mode    proxyMode
	changed chan struct{}
	conns   map[net.Conn]struct{}
}

// NewTCPProxy starts a forwarding proxy on an ephemeral loopback port.
func NewTCPProxy(parent context.Context, upstream string) (*TCPProxy, error) {
	listener, err := (&net.ListenConfig{}).Listen(parent, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for chaos proxy: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	proxy := &TCPProxy{
		listener: listener,
		upstream: upstream,
		ctx:      ctx,
		cancel:   cancel,
		changed:  make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
	}
	proxy.wg.Add(1)
	go proxy.accept()
	return proxy, nil
}

// Addr is the loopback address clients should use instead of the real server.
func (p *TCPProxy) Addr() string { return p.listener.Addr().String() }

// RejectNew makes newly accepted connections fail immediately. Connections
// that were already forwarding are not changed.
func (p *TCPProxy) RejectNew() { p.setMode(modeRejectNew) }

// Silence accepts new TCP connections but neither dials nor answers until
// Restore is called. This reaches handshake timeout paths that a closed port
// cannot exercise.
func (p *TCPProxy) Silence() { p.setMode(modeSilent) }

// Restore forwards new connections and releases silent accepted connections.
func (p *TCPProxy) Restore() { p.setMode(modeForward) }

// CloseExisting severs all currently tracked client and upstream sockets.
// New connections follow the currently selected mode.
func (p *TCPProxy) CloseExisting() {
	p.mu.Lock()
	connections := make([]net.Conn, 0, len(p.conns))
	for connection := range p.conns {
		connections = append(connections, connection)
	}
	p.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

// WaitForConnections waits until exactly want sockets are tracked. It gives
// fault tests synchronization and useful diagnostics without fixed sleeps.
func (p *TCPProxy) WaitForConnections(ctx context.Context, want int) error {
	for {
		p.mu.Lock()
		got := len(p.conns)
		changed := p.changed
		p.mu.Unlock()
		if got == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for proxy connections: have %d, want %d: %w", got, want, ctx.Err())
		case <-changed:
		}
	}
}

// Close stops accepting and closes every active socket.
func (p *TCPProxy) Close() error {
	p.cancel()
	err := p.listener.Close()
	p.CloseExisting()
	p.signalChange()
	p.wg.Wait()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close chaos proxy: %w", err)
	}
	return nil
}

func (p *TCPProxy) accept() {
	defer p.wg.Done()
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			if p.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		p.track(connection)
		p.wg.Add(1)
		go p.handle(connection)
	}
}

func (p *TCPProxy) handle(client net.Conn) {
	defer p.wg.Done()
	defer p.untrackAndClose(client)

	if !p.waitUntilForward(client) {
		return
	}

	upstream, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", p.upstream)
	if err != nil {
		return
	}
	p.track(upstream)
	defer p.untrackAndClose(upstream)

	done := make(chan struct{}, 2)
	copyStream := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		done <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	select {
	case <-done:
	case <-p.ctx.Done():
	}
}

func (p *TCPProxy) waitUntilForward(client net.Conn) bool {
	for {
		p.mu.Lock()
		mode := p.mode
		changed := p.changed
		p.mu.Unlock()
		switch mode {
		case modeForward:
			return true
		case modeRejectNew:
			return false
		case modeSilent:
			select {
			case <-p.ctx.Done():
				return false
			case <-changed:
			}
		default:
			_ = client.Close()
			return false
		}
	}
}

func (p *TCPProxy) setMode(mode proxyMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *TCPProxy) track(connection net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[connection] = struct{}{}
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *TCPProxy) untrackAndClose(connection net.Conn) {
	_ = connection.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, connection)
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *TCPProxy) signalChange() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.changed)
	p.changed = make(chan struct{})
}
