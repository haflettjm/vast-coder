package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type fakeTunnel struct {
	mu        sync.Mutex
	dials     int
	keepalive error
	closed    chan struct{}
	once      sync.Once
}

func newFakeTunnel() *fakeTunnel { return &fakeTunnel{closed: make(chan struct{})} }

func (f *fakeTunnel) Dial(string) (net.Conn, error) {
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	a, _ := net.Pipe()
	return a, nil
}
func (f *fakeTunnel) SendKeepalive() error { f.mu.Lock(); defer f.mu.Unlock(); return f.keepalive }
func (f *fakeTunnel) Wait() error          { <-f.closed; return errors.New("connection lost") }
func (f *fakeTunnel) Close() error         { f.once.Do(func() { close(f.closed) }); return nil }
func (f *fakeTunnel) drop()                { _ = f.Close() }

func fastPool(n int, connect connector) *pool {
	p := newPool(n, "127.0.0.1:8080", connect, 10*time.Millisecond)
	p.minWait, p.maxWait = 5*time.Millisecond, 20*time.Millisecond
	return p
}

func upCount(p *pool) int {
	n := 0
	for _, s := range p.Status() {
		if s.Up {
			n++
		}
	}
	return n
}

func TestPoolSpreadsRequestsAcrossTunnels(t *testing.T) {
	tunnels := []*fakeTunnel{newFakeTunnel(), newFakeTunnel()}
	p := fastPool(2, func(_ context.Context, slot int) (tunnel, error) { return tunnels[slot], nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	waitFor(t, func() bool { return upCount(p) == 2 })

	var conns []net.Conn
	for i := 0; i < 4; i++ {
		c, err := p.DialContext(ctx, "tcp", "ignored")
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, s := range p.Status() {
		if s.Inflight != 2 {
			t.Fatalf("tunnel %d carries %d, want an even split of 2", s.Slot, s.Inflight)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
	for _, s := range p.Status() {
		if s.Inflight != 0 {
			t.Fatalf("tunnel %d leaked %d in-flight after close", s.Slot, s.Inflight)
		}
	}
}

func TestPoolKeepsWorkingWhenOneTunnelCannotConnect(t *testing.T) {
	good := newFakeTunnel()
	p := fastPool(2, func(_ context.Context, slot int) (tunnel, error) {
		if slot == 1 {
			return nil, errors.New("route unreachable")
		}
		return good, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	waitFor(t, func() bool { return upCount(p) == 1 })
	for i := 0; i < 3; i++ {
		c, err := p.DialContext(ctx, "tcp", "x")
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	if good.dials != 3 {
		t.Fatalf("healthy tunnel handled %d dials, want 3", good.dials)
	}
	waitFor(t, func() bool { return p.Status()[1].LastErr == "route unreachable" })
}

func TestPoolReconnectsAfterATunnelDrops(t *testing.T) {
	var mu sync.Mutex
	var made []*fakeTunnel
	p := fastPool(1, func(context.Context, int) (tunnel, error) {
		mu.Lock()
		defer mu.Unlock()
		f := newFakeTunnel()
		made = append(made, f)
		return f, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	waitFor(t, func() bool { return upCount(p) == 1 })
	mu.Lock()
	first := made[0]
	mu.Unlock()
	first.drop()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(made) == 2 && upCount(p) == 1 })
}

func TestFailedKeepaliveTakesTheTunnelOutOfRotation(t *testing.T) {
	f := newFakeTunnel()
	var mu sync.Mutex
	calls := 0
	p := fastPool(1, func(context.Context, int) (tunnel, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return f, nil
		}
		return newFakeTunnel(), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)
	waitFor(t, func() bool { return upCount(p) == 1 })
	f.mu.Lock()
	f.keepalive = errors.New("no reply")
	f.mu.Unlock()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 })
}

func TestDialWithNoTunnelFailsFast(t *testing.T) {
	p := fastPool(2, func(context.Context, int) (tunnel, error) { return nil, errors.New("down") })
	if _, err := p.DialContext(context.Background(), "tcp", "x"); !errors.Is(err, errNoTunnel) {
		t.Fatalf("got %v, want errNoTunnel", err)
	}
}

func TestEachRequestUsesItsOwnChannelAndNeverReusesAConnection(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	})
	tr, ok := g.srv.rp.Transport.(*http.Transport)
	if !ok || !tr.DisableKeepAlives {
		t.Fatal("keep-alives must stay disabled so net/http can never resend a request on a reused connection")
	}
}
