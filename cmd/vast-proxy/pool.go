package main

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

var errNoTunnel = errors.New("no ssh tunnel is up")

// tunnel is one established SSH connection that can open channels to the model server.
type tunnel interface {
	Dial(addr string) (net.Conn, error)
	SendKeepalive() error
	Wait() error
	Close() error
}

// connector establishes a new tunnel. It is retried with backoff by the pool.
type connector func(ctx context.Context, slot int) (tunnel, error)

type tunnelStatus struct {
	Slot     int       `json:"slot"`
	Up       bool      `json:"up"`
	Inflight int       `json:"inflight"`
	Since    time.Time `json:"since,omitempty"`
	LastErr  string    `json:"last_error,omitempty"`
}

type member struct {
	slot     int
	mu       sync.Mutex
	t        tunnel
	inflight int
	since    time.Time
	lastErr  string
}

// pool keeps several independent SSH connections alive. Each is its own TCP stream
// with its own congestion window, so a stalled upload on one does not block the rest.
type pool struct {
	remote    string
	connect   connector
	keepalive time.Duration
	minWait   time.Duration
	maxWait   time.Duration
	members   []*member
}

func newPool(n int, remote string, connect connector, keepalive time.Duration) *pool {
	p := &pool{remote: remote, connect: connect, keepalive: keepalive,
		minWait: time.Second, maxWait: 30 * time.Second}
	for i := 0; i < n; i++ {
		p.members = append(p.members, &member{slot: i})
	}
	return p
}

// run keeps every member connected until ctx ends.
func (p *pool) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range p.members {
		wg.Add(1)
		go func(m *member) {
			defer wg.Done()
			p.maintain(ctx, m)
		}(m)
	}
	wg.Wait()
}

func (p *pool) maintain(ctx context.Context, m *member) {
	wait := p.minWait
	for ctx.Err() == nil {
		t, err := p.connect(ctx, m.slot)
		if err != nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			log.Printf("tunnel %d: connect failed: %v (retry in %s)", m.slot, err, wait)
			if !sleep(ctx, wait) {
				return
			}
			if wait *= 2; wait > p.maxWait {
				wait = p.maxWait
			}
			continue
		}
		wait = p.minWait
		m.mu.Lock()
		m.t, m.since, m.lastErr = t, time.Now(), ""
		m.mu.Unlock()
		log.Printf("tunnel %d: up", m.slot)

		reason := p.watch(ctx, t)

		m.mu.Lock()
		m.t, m.lastErr = nil, reason
		m.mu.Unlock()
		_ = t.Close()
		log.Printf("tunnel %d: down: %s", m.slot, reason)
		if !sleep(ctx, p.minWait) {
			return
		}
	}
}

// watch blocks until the tunnel ends or a keepalive fails, and returns why.
func (p *pool) watch(ctx context.Context, t tunnel) string {
	done := make(chan error, 1)
	go func() { done <- t.Wait() }()
	ticker := time.NewTicker(p.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "shutdown"
		case err := <-done:
			if err == nil {
				return "closed by remote"
			}
			return err.Error()
		case <-ticker.C:
			ok := make(chan error, 1)
			go func() { ok <- t.SendKeepalive() }()
			select {
			case err := <-ok:
				if err != nil {
					return "keepalive failed: " + err.Error()
				}
			case <-time.After(3 * p.keepalive):
				return "keepalive timed out"
			case <-ctx.Done():
				return "shutdown"
			}
		}
	}
}

// DialContext opens a channel to the model server on the least-loaded tunnel.
func (p *pool) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	m := p.pick()
	if m == nil {
		return nil, errNoTunnel
	}
	m.mu.Lock()
	t := m.t
	m.inflight++
	m.mu.Unlock()
	if t == nil {
		p.done(m)
		return nil, errNoTunnel
	}
	type result struct {
		c   net.Conn
		err error
	}
	res := make(chan result, 1)
	go func() {
		c, err := t.Dial(p.remote)
		res <- result{c, err}
	}()
	select {
	case r := <-res:
		if r.err != nil {
			p.done(m)
			return nil, r.err
		}
		return &countedConn{Conn: r.c, release: func() { p.done(m) }}, nil
	case <-ctx.Done():
		go func() { // do not leak a channel that opens after the caller left
			if r := <-res; r.err == nil {
				_ = r.c.Close()
			}
		}()
		p.done(m)
		return nil, ctx.Err()
	}
}

func (p *pool) pick() *member {
	var best *member
	bestLoad := 0
	for _, m := range p.members {
		m.mu.Lock()
		up, load := m.t != nil, m.inflight
		m.mu.Unlock()
		if up && (best == nil || load < bestLoad) {
			best, bestLoad = m, load
		}
	}
	return best
}

func (p *pool) done(m *member) {
	m.mu.Lock()
	m.inflight--
	m.mu.Unlock()
}

func (p *pool) Status() []tunnelStatus {
	out := make([]tunnelStatus, 0, len(p.members))
	for _, m := range p.members {
		m.mu.Lock()
		out = append(out, tunnelStatus{Slot: m.slot, Up: m.t != nil, Inflight: m.inflight,
			Since: m.since, LastErr: m.lastErr})
		m.mu.Unlock()
	}
	return out
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
