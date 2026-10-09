// vast-proxy is a loopback OpenAI-compatible front end for the model server on the
// Vast rental. It keeps a small pool of SSH tunnels, queues inference requests so
// the server never gets more than it has slots for, drops requests whose client has
// gone away, and answers /health without touching a tunnel. It never replays a
// request that has already been sent upstream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()
	root := os.Getenv("VAST_CODER_ROOT")
	if root == "" {
		if exe, err := os.Executable(); err == nil {
			root = filepath.Dir(filepath.Dir(exe))
		}
	}
	listen := flag.String("listen", "127.0.0.1:8000", "loopback address Hermes connects to")
	remote := flag.String("remote", "127.0.0.1:8080", "model server address on the rental")
	conns := flag.Int("conns", 2, "independent SSH connections to keep open")
	inflight := flag.Int("max-inflight", 4, "concurrent inference requests; match the server's slot count")
	queue := flag.Int("max-queue", 64, "requests allowed to wait for a slot")
	wait := flag.Duration("queue-wait", 30*time.Second, "longest a request waits for a slot before a 503")
	maxBody := flag.Int64("max-body", 32<<20, "largest accepted request body in bytes")
	keepalive := flag.Duration("keepalive", 5*time.Second, "SSH keepalive interval (the rental drops silent clients after about 20 s)")
	probeEvery := flag.Duration("probe-interval", 10*time.Second, "how often to sample the server's own /health")
	stateDir := flag.String("state-dir", filepath.Join(root, ".state"), "directory with ssh_endpoint.json")
	agentSock := flag.String("agent", defaultAgent(home), "ssh-agent socket (1Password agent is used if present)")
	flag.Parse()

	if err := requireLoopback(*listen); err != nil {
		log.Fatal(err)
	}
	if *conns < 1 || *inflight < 1 || *queue < 0 {
		log.Fatal("conns and max-inflight must be at least 1, max-queue at least 0")
	}

	cfg := sshConfig{
		stateDir:  *stateDir,
		agentSock: *agentSock,
		keyFiles:  []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_ecdsa"), filepath.Join(home, ".ssh", "id_rsa")},
		userHosts: filepath.Join(home, ".ssh", "known_hosts"),
		refresher: execRefresher(root),
		timeout:   15 * time.Second,
	}
	p := newPool(*conns, *remote, newSSHConnector(cfg), *keepalive)
	srv := newServer(serverConfig{maxInflight: *inflight, maxQueue: *queue, queueWait: *wait,
		maxBody: *maxBody, retryAfter: 5}, p, *remote)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go p.run(ctx)
	go srv.probe.run(ctx, *probeEvery, 8*time.Second)

	httpServer := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	log.Printf("listening on %s, %d tunnels, %d slots, queue %d", *listen, *conns, *inflight, *queue)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// requireLoopback keeps the proxy off the network: it adds no authentication of its own.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to listen on %q: use a loopback IP such as 127.0.0.1", host)
	}
	return nil
}

func defaultAgent(home string) string {
	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" {
		return s
	}
	if p := filepath.Join(home, ".1password", "agent.sock"); fileExists(p) {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
