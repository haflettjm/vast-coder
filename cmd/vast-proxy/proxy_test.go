package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// directUpstream stands in for the SSH pool: it dials a local test server.
type directUpstream struct {
	addr   string
	err    error
	status []tunnelStatus
}

func (d *directUpstream) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.addr)
}

func (d *directUpstream) Status() []tunnelStatus {
	if d.status != nil {
		return d.status
	}
	return []tunnelStatus{{Slot: 0, Up: true}}
}

type rig struct {
	proxy *httptest.Server
	srv   *server
	up    *directUpstream
	hits  atomic.Int32
}

func newRig(t *testing.T, cfg serverConfig, handler func(r *rig) http.Handler) *rig {
	t.Helper()
	g := &rig{}
	model := httptest.NewServer(handler(g))
	t.Cleanup(model.Close)
	g.up = &directUpstream{addr: model.Listener.Addr().String()}
	if cfg.queueWait == 0 {
		cfg.queueWait = 5 * time.Second
	}
	if cfg.maxBody == 0 {
		cfg.maxBody = 1 << 20
	}
	cfg.retryAfter = 5
	g.srv = newServer(cfg, g.up, "model.test")
	g.proxy = httptest.NewServer(g.srv)
	t.Cleanup(g.proxy.Close)
	return g
}

func post(t *testing.T, url, body string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	return http.DefaultClient.Do(req)
}

func TestForwardsRequestAndAuthorization(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 2, maxQueue: 4}, func(g *rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.hits.Add(1)
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Authorization") != "Bearer test-key" || r.Host != "model.test" {
				t.Errorf("headers lost: auth=%q host=%q", r.Header.Get("Authorization"), r.Host)
			}
			_, _ = w.Write([]byte("echo:" + string(b)))
		})
	})
	resp, err := post(t, g.proxy.URL, "hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "echo:hello" || g.hits.Load() != 1 {
		t.Fatalf("body=%q hits=%d", body, g.hits.Load())
	}
}

func TestConcurrencyNeverExceedsSlots(t *testing.T) {
	var running, peak atomic.Int32
	gate := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 2, maxQueue: 16}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-gate
			running.Add(-1)
			_, _ = w.Write([]byte("ok"))
		})
	})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := post(t, g.proxy.URL, "x"); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	waitFor(t, func() bool { in, q := g.srv.lim.stats(); return in == 2 && q == 4 })
	close(gate)
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak concurrent upstream requests = %d, want 2", peak.Load())
	}
}

func TestQueueFullAndWaitTimeoutReturn503WithRetryAfter(t *testing.T) {
	gate := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1, queueWait: 150 * time.Millisecond}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-gate })
	})
	defer close(gate)
	go func() { _, _ = post(t, g.proxy.URL, "holds the slot") }()
	waitFor(t, func() bool { in, _ := g.srv.lim.stats(); return in == 1 })
	queued := make(chan *http.Response, 1)
	go func() { r, _ := post(t, g.proxy.URL, "waits"); queued <- r }()
	waitFor(t, func() bool { _, q := g.srv.lim.stats(); return q == 1 })

	resp, err := post(t, g.proxy.URL, "overflow")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("overflow: status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	waited := <-queued // times out after queueWait
	if waited == nil || waited.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("queued request should time out with 503, got %v", waited)
	}
	_ = waited.Body.Close()
	if g.srv.counts.rejectedFull.Load() != 1 || g.srv.counts.rejectedTimeout.Load() != 1 {
		t.Fatalf("counts: full=%d timeout=%d", g.srv.counts.rejectedFull.Load(), g.srv.counts.rejectedTimeout.Load())
	}
}

func TestDisconnectedClientIsDroppedBeforeReachingUpstream(t *testing.T) {
	gate := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 4}, func(g *rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.hits.Add(1)
			<-gate
		})
	})
	go func() { _, _ = post(t, g.proxy.URL, "first") }()
	waitFor(t, func() bool { return g.hits.Load() == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.proxy.URL+"/v1/chat/completions", strings.NewReader("abandoned"))
	done := make(chan struct{})
	go func() { _, _ = http.DefaultClient.Do(req); close(done) }()
	waitFor(t, func() bool { _, q := g.srv.lim.stats(); return q == 1 })
	cancel() // the client goes away while queued
	<-done
	waitFor(t, func() bool { _, q := g.srv.lim.stats(); return q == 0 })
	close(gate)
	waitFor(t, func() bool { in, _ := g.srv.lim.stats(); return in == 0 })
	if g.hits.Load() != 1 {
		t.Fatalf("upstream saw %d requests; the abandoned one must never be sent", g.hits.Load())
	}
	if g.srv.counts.droppedClient.Load() != 1 {
		t.Fatalf("dropped_disconnected = %d, want 1", g.srv.counts.droppedClient.Load())
	}
}

func TestFailedUpstreamRequestIsNeverReplayed(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 2, maxQueue: 4}, func(g *rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.hits.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close() // die without answering
		})
	})
	resp, err := post(t, g.proxy.URL, "side effects")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond) // give any hidden retry time to appear
	if g.hits.Load() != 1 {
		t.Fatalf("upstream received the request %d times; it must be sent once", g.hits.Load())
	}
}

func TestTunnelDownReturns503(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	})
	g.up.err = errNoTunnel
	resp, err := post(t, g.proxy.URL, "x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestStreamedResponsesAreFlushedAsTheyArrive(t *testing.T) {
	second := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-second
			_, _ = io.WriteString(w, "data: two\n\n")
		})
	})
	resp, err := post(t, g.proxy.URL, "stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(resp.Body).ReadString('\n'); line <- s }()
	select {
	case s := <-line:
		if s != "data: one\n" {
			t.Fatalf("first chunk %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event was held back until the response finished")
	}
	close(second)
}

func TestReadsBypassTheQueue(t *testing.T) {
	gate := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				<-gate
				return
			}
			_, _ = io.WriteString(w, `{"data":[]}`)
		})
	})
	defer close(gate)
	go func() { _, _ = post(t, g.proxy.URL, "slow") }()
	waitFor(t, func() bool { in, _ := g.srv.lim.stats(); return in == 1 })
	resp, err := http.Get(g.proxy.URL + "/v1/models")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("models read blocked behind inference: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestHealthAnswersWhileEverythingIsBusy(t *testing.T) {
	gate := make(chan struct{})
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-gate })
	})
	defer close(gate)
	go func() { _, _ = post(t, g.proxy.URL, "slow") }()
	waitFor(t, func() bool { in, _ := g.srv.lim.stats(); return in == 1 })

	check := func(want string, wantCode int) {
		t.Helper()
		start := time.Now()
		resp, err := http.Get(g.proxy.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Status   string `json:"status"`
			Inflight int    `json:"inflight"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if time.Since(start) > time.Second || out.Status != want || resp.StatusCode != wantCode {
			t.Fatalf("health: %q (%d) after %s, want %q (%d)", out.Status, resp.StatusCode, time.Since(start), want, wantCode)
		}
	}
	check("degraded", 503) // tunnels up but no probe result yet

	g.srv.probe.mu.Lock()
	g.srv.probe.code, g.srv.probe.at = 200, time.Now()
	g.srv.probe.mu.Unlock()
	check("inference-ready", 200)

	g.srv.probe.mu.Lock()
	g.srv.probe.code = 503
	g.srv.probe.mu.Unlock()
	check("model-loading", 503)

	g.up.status = []tunnelStatus{{Slot: 0, Up: false}}
	check("unreachable", 503)
}

func TestProberSamplesTheServerHealth(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1}, func(*rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
	})
	g.srv.probe.once(context.Background(), time.Second)
	if code, at := g.srv.probe.last(); code != 503 || at.IsZero() {
		t.Fatalf("probe recorded %d at %v", code, at)
	}
	g.up.err = errNoTunnel
	g.srv.probe.once(context.Background(), time.Second)
	if code, _ := g.srv.probe.last(); code != 0 {
		t.Fatalf("failed probe should record 0, got %d", code)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	g := newRig(t, serverConfig{maxInflight: 1, maxQueue: 1, maxBody: 16}, func(g *rig) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.hits.Add(1) })
	})
	resp, err := post(t, g.proxy.URL, strings.Repeat("x", 64))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || g.hits.Load() != 0 {
		t.Fatalf("status %d hits %d", resp.StatusCode, g.hits.Load())
	}
}

func TestOnlyLoopbackAddressesAreAccepted(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:8000": true, "[::1]:8000": true,
		"0.0.0.0:8000": false, "192.168.1.5:8000": false, "localhost:8000": false, ":8000": false} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("requireLoopback(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}
