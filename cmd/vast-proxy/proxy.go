package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// upstream is the set of SSH tunnels. The pool implements it; tests use fakes.
type upstream interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	Status() []tunnelStatus
}

type serverConfig struct {
	maxInflight int
	maxQueue    int
	queueWait   time.Duration
	maxBody     int64
	retryAfter  int // seconds advertised on 503
}

type counters struct {
	served, rejectedFull, rejectedTimeout, droppedClient, upstreamErrors atomic.Int64
}

type server struct {
	cfg    serverConfig
	lim    *limiter
	up     upstream
	rp     *httputil.ReverseProxy
	probe  *prober
	counts counters
}

func newServer(cfg serverConfig, up upstream, remoteHost string) *server {
	s := &server{cfg: cfg, lim: newLimiter(cfg.maxInflight, cfg.maxQueue), up: up}
	// New channel per request and no connection reuse. This also stops net/http from
	// ever re-sending a request on a different connection: a request is never replayed.
	tr := &http.Transport{DialContext: up.DialContext, DisableKeepAlives: true, DisableCompression: true}
	s.rp = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme, r.URL.Host, r.Host = "http", remoteHost, remoteHost
		},
		Transport:     tr,
		FlushInterval: -1, // stream server-sent events as they arrive
		ErrorHandler:  s.upstreamError,
	}
	s.probe = newProber(&http.Client{Transport: &http.Transport{DialContext: up.DialContext, DisableKeepAlives: true}}, remoteHost)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		s.serveHealth(w)
		return
	}
	// Only bodies that start inference need a slot. Reads such as /v1/models bypass the queue.
	if r.Method != http.MethodPost {
		s.rp.ServeHTTP(w, r)
		return
	}

	// Read the whole request locally first (loopback, so this is fast). Once the body is
	// consumed, net/http notices a client that went away and cancels r.Context(), so a
	// queued request from a dead client is dropped before it uses the slow upload path.
	buf, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "")
		}
		return
	}
	_ = r.Body.Close()

	qctx, cancel := context.WithTimeout(r.Context(), s.cfg.queueWait)
	release, err := s.lim.acquire(qctx)
	cancel()
	switch {
	case err == nil:
	case r.Context().Err() != nil:
		s.counts.droppedClient.Add(1)
		return // the client is gone; nothing to answer
	case errors.Is(err, errQueueFull):
		s.counts.rejectedFull.Add(1)
		s.busy(w, "queue is full")
		return
	default:
		s.counts.rejectedTimeout.Add(1)
		s.busy(w, "timed out waiting for a free slot")
		return
	}
	defer release()

	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	r.GetBody = nil
	s.counts.served.Add(1)
	s.rp.ServeHTTP(w, r)
}

func (s *server) busy(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusServiceUnavailable, msg, strconv.Itoa(s.cfg.retryAfter))
}

func (s *server) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil:
		return
	case errors.Is(err, errNoTunnel):
		s.busy(w, "ssh tunnel to the model server is down")
	default:
		s.counts.upstreamErrors.Add(1)
		writeError(w, http.StatusBadGateway, "upstream request failed; it was not retried", "")
	}
}

func writeError(w http.ResponseWriter, code int, msg, retryAfter string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "proxy_error"}})
}

// prober samples the model server's own /health over a tunnel, off the request path.
type prober struct {
	client *http.Client
	host   string
	mu     sync.Mutex
	code   int // 0 means the last probe failed
	at     time.Time
}

func newProber(client *http.Client, host string) *prober {
	return &prober{client: client, host: host}
}

func (p *prober) run(ctx context.Context, every, timeout time.Duration) {
	p.once(ctx, timeout)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.once(ctx, timeout)
		}
	}
}

func (p *prober) once(ctx context.Context, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code := 0
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.host+"/health", nil); err == nil {
		if resp, err := p.client.Do(req); err == nil {
			code = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}
	}
	p.mu.Lock()
	p.code, p.at = code, time.Now()
	p.mu.Unlock()
}

func (p *prober) last() (int, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.at
}

// serveHealth never touches a tunnel, so it answers even when every slot is busy
// or every tunnel is stalled behind a large upload.
func (s *server) serveHealth(w http.ResponseWriter) {
	tunnels := s.up.Status()
	up := 0
	for _, t := range tunnels {
		if t.Up {
			up++
		}
	}
	code, at := s.probe.last()
	fresh := !at.IsZero() && time.Since(at) < 45*time.Second
	status := "unreachable"
	switch {
	case up == 0:
	case !fresh || code == 0:
		status = "degraded"
	case code == http.StatusOK:
		status = "inference-ready"
	case code == http.StatusServiceUnavailable:
		status = "model-loading"
	default:
		status = "degraded"
	}
	inflight, queued := s.lim.stats()
	out := map[string]any{
		"status": status, "tunnels": tunnels, "tunnels_up": up,
		"inflight": inflight, "queued": queued, "max_inflight": s.cfg.maxInflight,
		"upstream_health_code": code, "probe_age_seconds": ageSeconds(at),
		"counts": map[string]int64{
			"served": s.counts.served.Load(), "rejected_queue_full": s.counts.rejectedFull.Load(),
			"rejected_wait_timeout": s.counts.rejectedTimeout.Load(),
			"dropped_disconnected":  s.counts.droppedClient.Load(), "upstream_errors": s.counts.upstreamErrors.Load(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if status == "inference-ready" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func ageSeconds(t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return time.Since(t).Seconds()
}
