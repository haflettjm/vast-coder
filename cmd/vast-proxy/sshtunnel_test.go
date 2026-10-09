package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startSSHServer is a minimal in-process sshd that only supports direct-tcpip
// forwarding and keepalive requests, which is all the proxy needs.
func startSSHServer(t *testing.T) (addr string, key ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
				if err != nil {
					return
				}
				go func() {
					for r := range reqs {
						if r.WantReply {
							_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
						}
					}
				}()
				for nc := range chans {
					if nc.ChannelType() != "direct-tcpip" {
						_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					var req struct {
						Host     string
						Port     uint32
						OrigHost string
						OrigPort uint32
					}
					_ = ssh.Unmarshal(nc.ExtraData(), &req)
					ch, chReqs, _ := nc.Accept()
					go ssh.DiscardRequests(chReqs)
					go func() {
						dst, err := net.Dial("tcp", net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port))))
						if err != nil {
							_ = ch.Close()
							return
						}
						go func() { _, _ = io.Copy(dst, ch); _ = dst.Close() }()
						_, _ = io.Copy(ch, dst)
						_ = ch.Close()
					}()
				}
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String(), signer.PublicKey()
}

func writeEndpoint(t *testing.T, dir string, ep map[string]any) {
	t.Helper()
	data, _ := json.Marshal(ep)
	if err := os.WriteFile(filepath.Join(dir, "ssh_endpoint.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	return h, port
}

func TestRealSSHTunnelCarriesHTTPAndPinsTheHostKey(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "through-ssh")
	}))
	defer model.Close()
	sshAddr, _ := startSSHServer(t)
	host, port := splitAddr(t, sshAddr)

	dir := t.TempDir()
	writeEndpoint(t, dir, map[string]any{"host": host, "port": port})
	cfg := sshConfig{stateDir: dir, timeout: 3 * time.Second}
	connect := newSSHConnector(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := connect(ctx, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tun.Close()
	if err := tun.SendKeepalive(); err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	conn, err := tun.Dial(model.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}}
	resp, err := client.Get("http://model.test/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-ssh" {
		t.Fatalf("body %q", body)
	}

	pinned, _ := os.ReadFile(filepath.Join(dir, "proxy_known_hosts"))
	if !strings.Contains(string(pinned), "ssh-ed25519") {
		t.Fatalf("host key was not pinned: %q", pinned)
	}
	// Second connect uses the pinned key without adding a duplicate line.
	tun2, err := connect(ctx, 1)
	if err != nil {
		t.Fatalf("reconnect with pinned key: %v", err)
	}
	_ = tun2.Close()
	again, _ := os.ReadFile(filepath.Join(dir, "proxy_known_hosts"))
	if strings.Count(string(again), "\n") != 1 {
		t.Fatalf("expected a single pinned line, got %q", again)
	}
}

func TestChangedHostKeyIsRefused(t *testing.T) {
	first, _ := startSSHServer(t)
	host, port := splitAddr(t, first)
	dir := t.TempDir()
	writeEndpoint(t, dir, map[string]any{"host": host, "port": port})
	cfg := sshConfig{stateDir: dir, timeout: 3 * time.Second}
	ctx := context.Background()
	tun, err := newSSHConnector(cfg)(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = tun.Close()

	// A different server (new key) now answers at the same host:port the proxy pinned.
	impostor, _ := startSSHServer(t)
	_, ipPort := splitAddr(t, impostor)
	pinnedBytes, _ := os.ReadFile(filepath.Join(dir, "proxy_known_hosts"))
	rewritten := strings.Replace(string(pinnedBytes), "["+host+"]:"+strconv.Itoa(port), "["+host+"]:"+strconv.Itoa(ipPort), 1)
	if err := os.WriteFile(filepath.Join(dir, "proxy_known_hosts"), []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEndpoint(t, dir, map[string]any{"host": host, "port": ipPort})
	if _, err := newSSHConnector(cfg)(ctx, 0); err == nil {
		t.Fatal("a host whose key differs from the pinned one must be refused")
	}
}

func TestConnectorFallsBackToAlternateRoute(t *testing.T) {
	sshAddr, _ := startSSHServer(t)
	host, port := splitAddr(t, sshAddr)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadHost, deadPort := splitAddr(t, dead.Addr().String())
	_ = dead.Close() // nothing listens on the preferred route

	dir := t.TempDir()
	writeEndpoint(t, dir, map[string]any{"host": deadHost, "port": deadPort, "alt_host": host, "alt_port": port})
	tun, err := newSSHConnector(sshConfig{stateDir: dir, timeout: time.Second})(context.Background(), 0)
	if err != nil {
		t.Fatalf("alternate route should have been used: %v", err)
	}
	_ = tun.Close()
}

func TestConnectorRefreshesEndpointOnceWhenAllRoutesFail(t *testing.T) {
	sshAddr, _ := startSSHServer(t)
	host, port := splitAddr(t, sshAddr)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadHost, deadPort := splitAddr(t, dead.Addr().String())
	_ = dead.Close()

	dir := t.TempDir()
	writeEndpoint(t, dir, map[string]any{"host": deadHost, "port": deadPort})
	refreshed := 0
	cfg := sshConfig{stateDir: dir, timeout: time.Second, refresher: func(context.Context) error {
		refreshed++
		writeEndpoint(t, dir, map[string]any{"host": host, "port": port}) // discovery found the new address
		return nil
	}}
	tun, err := newSSHConnector(cfg)(context.Background(), 0)
	if err != nil || refreshed != 1 {
		t.Fatalf("err=%v refreshed=%d, want success after exactly one refresh", err, refreshed)
	}
	_ = tun.Close()
}

func TestEndpointCacheValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := readEndpoint(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing cache accepted")
	}
	writeEndpoint(t, dir, map[string]any{"host": "", "port": 0})
	if _, err := readEndpoint(filepath.Join(dir, "ssh_endpoint.json")); err == nil {
		t.Error("cache with no route accepted")
	}
	writeEndpoint(t, dir, map[string]any{"host": "ssh6.vast.ai", "port": 11088, "alt_host": "203.0.113.7", "alt_port": 26655})
	ep, err := readEndpoint(filepath.Join(dir, "ssh_endpoint.json"))
	if err != nil || len(ep.routes()) != 2 || ep.routes()[0] != "ssh6.vast.ai:11088" {
		t.Errorf("routes %v err %v", ep.routes(), err)
	}
}
