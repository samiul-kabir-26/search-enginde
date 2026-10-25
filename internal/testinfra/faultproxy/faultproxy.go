// Package faultproxy is a TCP proxy for chaos tests: it forwards to a target, refuses
// connections, accepts them and never answers (a hung endpoint), or delays every chunk.
package faultproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is how the proxy treats connections.
type Mode int32

// Modes.
const (
	Pass Mode = iota
	// Refuse closes every connection as soon as it is accepted.
	Refuse
	// Blackhole accepts connections and reads from them, but never forwards or answers.
	Blackhole
	// Delay forwards every chunk in both directions after the configured delay.
	Delay
)

// Proxy forwards host:port connections to a target with injected faults.
type Proxy struct {
	ln     net.Listener
	target string
	mode   atomic.Int32
	delay  atomic.Int64

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  chan struct{}
}

// Start listens on 127.0.0.1 and forwards to target.
func Start(target string) (*Proxy, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	p := &Proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
	go p.serve()
	return p, nil
}

// Addr is the proxy's host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Set switches the mode. Open connections are closed so clients reconnect into the new mode.
func (p *Proxy) Set(m Mode, delay time.Duration) {
	p.mode.Store(int32(m))
	p.delay.Store(int64(delay))
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
}

// Close stops the proxy and closes every connection.
func (p *Proxy) Close() error {
	close(p.done)
	p.Set(Pass, 0)
	if err := p.ln.Close(); err != nil {
		return fmt.Errorf("close listener: %w", err)
	}
	return nil
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return false
	default:
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			_ = c.Close()
			return
		}
		go p.handle(c)
	}
}

func (p *Proxy) handle(c net.Conn) {
	switch Mode(p.mode.Load()) {
	case Refuse:
		_ = c.Close()
		return
	case Blackhole:
		_, _ = io.Copy(io.Discard, c)
		return
	}
	var d net.Dialer
	up, err := d.DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	if !p.track(up) {
		_ = up.Close()
		_ = c.Close()
		return
	}
	go p.pipe(up, c)
	p.pipe(c, up)
}

func (p *Proxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if Mode(p.mode.Load()) == Delay {
				time.Sleep(time.Duration(p.delay.Load()))
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
