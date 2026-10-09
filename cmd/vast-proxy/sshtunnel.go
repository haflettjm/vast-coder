package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// endpoint mirrors .state/ssh_endpoint.json, which bin/endpoint maintains.
// host/port is the preferred route (the Vast SSH proxy); alt_* is the direct IP.
type endpoint struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	AltHost string `json:"alt_host"`
	AltPort int    `json:"alt_port"`
}

func (e endpoint) routes() []string {
	var r []string
	if e.Host != "" && e.Port > 0 {
		r = append(r, net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
	}
	if e.AltHost != "" && e.AltPort > 0 {
		r = append(r, net.JoinHostPort(e.AltHost, strconv.Itoa(e.AltPort)))
	}
	return r
}

type sshTunnel struct{ c *ssh.Client }

func (s sshTunnel) Dial(addr string) (net.Conn, error) { return s.c.Dial("tcp", addr) }
func (s sshTunnel) Wait() error                        { return s.c.Wait() }
func (s sshTunnel) Close() error                       { return s.c.Close() }
func (s sshTunnel) SendKeepalive() error {
	_, _, err := s.c.SendRequest("keepalive@openssh.com", true, nil)
	return err
}

type sshConfig struct {
	stateDir  string // holds ssh_endpoint.json and the pinned known_hosts file
	agentSock string
	keyFiles  []string
	userHosts string // the user's ~/.ssh/known_hosts, consulted read-only
	refresher func(ctx context.Context) error
	timeout   time.Duration
}

// newSSHConnector returns a connector that tries the preferred route, then the alternate.
func newSSHConnector(cfg sshConfig) connector {
	var refreshMu sync.Mutex
	var lastRefresh time.Time
	return func(ctx context.Context, slot int) (tunnel, error) {
		var lastErr error
		for attempt := 0; attempt < 2; attempt++ {
			ep, err := readEndpoint(filepath.Join(cfg.stateDir, "ssh_endpoint.json"))
			if err != nil {
				lastErr = err
			} else {
				var routeErrs []error
				for _, addr := range ep.routes() {
					t, err := dialSSH(ctx, addr, cfg)
					if err == nil {
						return t, nil
					}
					routeErrs = append(routeErrs, fmt.Errorf("%s: %w", addr, err))
				}
				// Keep every route's failure, not just the last, so a bad preferred route is visible.
				lastErr = errors.Join(routeErrs...)
			}
			// Every route failed. Ask discovery to refresh the cache, at most every two minutes.
			if attempt == 0 && cfg.refresher != nil {
				refreshMu.Lock()
				due := time.Since(lastRefresh) > 2*time.Minute
				if due {
					lastRefresh = time.Now()
				}
				refreshMu.Unlock()
				if !due {
					break
				}
				if err := cfg.refresher(ctx); err != nil {
					log.Printf("endpoint refresh failed: %v", err)
					break
				}
				continue
			}
			break
		}
		return nil, lastErr
	}
}

func readEndpoint(path string) (endpoint, error) {
	var ep endpoint
	data, err := os.ReadFile(path)
	if err != nil {
		return ep, fmt.Errorf("read endpoint cache: %w", err)
	}
	if err := json.Unmarshal(data, &ep); err != nil {
		return ep, fmt.Errorf("parse endpoint cache: %w", err)
	}
	if len(ep.routes()) == 0 {
		return ep, errors.New("endpoint cache has no usable route")
	}
	return ep, nil
}

// execRefresher runs bin/endpoint, which rediscovers the labelled instance and rewrites the cache.
func execRefresher(root string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, filepath.Join(root, "bin", "endpoint")).Run()
	}
}

func dialSSH(ctx context.Context, addr string, cfg sshConfig) (tunnel, error) {
	methods, closeAgent := authMethods(cfg)
	hostCB, err := hostKeyCallback(cfg)
	if err != nil {
		closeAgent()
		return nil, err
	}
	sc := &ssh.ClientConfig{User: "root", Auth: methods, HostKeyCallback: hostCB, Timeout: cfg.timeout}
	d := net.Dialer{Timeout: cfg.timeout, KeepAlive: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		closeAgent()
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, sc)
	if err != nil {
		_ = raw.Close()
		closeAgent()
		return nil, err
	}
	return sshTunnel{c: ssh.NewClient(conn, chans, reqs)}, nil
}

// authMethods offers the agent's keys and the key files through ONE publickey method.
// x/crypto/ssh tries each method name once, so two separate publickey methods would
// leave the key files unused whenever the agent answers but holds no usable key.
func authMethods(cfg sshConfig) ([]ssh.AuthMethod, func()) {
	closer := func() {}
	var agentSigners func() ([]ssh.Signer, error)
	if cfg.agentSock != "" {
		if conn, err := net.DialTimeout("unix", cfg.agentSock, 2*time.Second); err == nil {
			agentSigners = agent.NewClient(conn).Signers
			closer = func() { _ = conn.Close() }
		}
	}
	var fileSigners []ssh.Signer
	for _, f := range cfg.keyFiles {
		pem, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if s, err := ssh.ParsePrivateKey(pem); err == nil { // encrypted keys are skipped
			fileSigners = append(fileSigners, s)
		}
	}
	if agentSigners == nil && len(fileSigners) == 0 {
		return nil, closer
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		var all []ssh.Signer
		if agentSigners != nil {
			if s, err := agentSigners(); err == nil {
				all = append(all, s...)
			}
		}
		return append(all, fileSigners...), nil
	})}, closer
}

// hostKeyCallback verifies against the user's known_hosts first. A host unknown
// there is pinned on first use in the proxy's own file; a changed key always fails.
func hostKeyCallback(cfg sshConfig) (ssh.HostKeyCallback, error) {
	pinned := filepath.Join(cfg.stateDir, "proxy_known_hosts")
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(pinned, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		_ = f.Close()
	} else {
		return nil, err
	}
	var user ssh.HostKeyCallback
	if cfg.userHosts != "" {
		if _, err := os.Stat(cfg.userHosts); err == nil {
			if cb, err := knownhosts.New(cfg.userHosts); err == nil {
				user = cb
			}
		}
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		if user != nil {
			err := user(host, remote, key)
			if err == nil {
				return nil
			}
			var ke *knownhosts.KeyError
			if !errors.As(err, &ke) || len(ke.Want) > 0 {
				return err // a changed key, or an unreadable file: never trust
			}
		}
		own, err := knownhosts.New(pinned)
		if err != nil {
			return err
		}
		err = own(host, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) || len(ke.Want) > 0 {
			return err
		}
		f, err := os.OpenFile(pinned, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, knownhosts.Line([]string{host}, key)); err != nil {
			return err
		}
		log.Printf("pinned new host key for %s (%s)", host, ssh.FingerprintSHA256(key))
		return nil
	}, nil
}
