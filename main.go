// Command ssh-agent-relay bridges a host SSH agent into a container across a
// loopback TCP hop, for setups where the agent's unix socket cannot be bind
// mounted directly — notably rootless podman on macOS, where virtiofs does not
// pass AF_UNIX sockets through to the VM.
//
//	host:       ssh-agent-relay serve   --listen 127.0.0.1:17890 --agent "$SSH_AUTH_SOCK"
//	container:  ssh-agent-relay connect --socket /tmp/ssh-agent-relay.sock \
//	                                    --upstream host.containers.internal:17890
//	container:  export SSH_AUTH_SOCK=/tmp/ssh-agent-relay.sock
//
// An optional shared token (--token-file, or $SSH_AGENT_RELAY_TOKEN) gates the
// TCP port so other local processes on the host cannot reach the agent through
// it. With a hardware-backed agent (1Password, Secretive) every signature still
// needs an explicit approval on the host regardless of the token.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultListen   = "127.0.0.1:17890"
	defaultUpstream = "host.containers.internal:17890"
	defaultSocket   = "/tmp/ssh-agent-relay.sock"
	tokenReadLimit  = 512
	handshakeGrace  = 5 * time.Second
	idleCheckEvery  = 30 * time.Second
)

// Set by -ldflags at release time (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ssh-agent-relay: ")

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "connect":
		err = runConnect(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("ssh-agent-relay %s (%s) %s\n", version, commit, date)
		return
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ssh-agent-relay — bridge a host SSH agent into a container over loopback TCP.

  serve    run on the host:      TCP  -> $SSH_AUTH_SOCK
  connect  run in the container: unix -> host relay over TCP
  version  print version and exit

Run "ssh-agent-relay serve -h" or "ssh-agent-relay connect -h" for flags.
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("SSH_AGENT_RELAY_LISTEN", defaultListen), "TCP address to accept connections on")
	agent := fs.String("agent", os.Getenv("SSH_AUTH_SOCK"), "host SSH agent unix socket (defaults to $SSH_AUTH_SOCK)")
	tokenFile := fs.String("token-file", os.Getenv("SSH_AGENT_RELAY_TOKEN_FILE"), "file holding the shared token (or set $SSH_AGENT_RELAY_TOKEN)")
	allowRemote := fs.Bool("allow-remote", false, "allow a non-loopback --listen address")
	idle := fs.Duration("idle-timeout", 0, "drop a relayed connection after this idle period (0 = never)")
	idleExit := fs.Duration("idle-exit", 0, "exit after this long with no active connection (0 = run forever)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *agent == "" {
		return errors.New("serve: no --agent and $SSH_AUTH_SOCK is empty")
	}
	if err := checkSocket(*agent); err != nil {
		return fmt.Errorf("serve: --agent %s: %w", *agent, err)
	}
	if !*allowRemote && !isLoopback(*listen) {
		return fmt.Errorf("serve: --listen %s is not loopback; pass --allow-remote to override", *listen)
	}
	token, err := loadToken(*tokenFile)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	log.Printf("serve: %s -> agent %s (token=%t)", ln.Addr(), *agent, token != nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	act := &activity{lastZero: time.Now()}
	if *idleExit > 0 {
		ctx = withIdleExit(ctx, act, *idleExit)
	}

	return accept(ctx, ln, func(c net.Conn) {
		defer c.Close()
		act.inc()
		defer act.dec()
		peer := c.RemoteAddr()
		if token != nil {
			if err := expectToken(c, token); err != nil {
				log.Printf("serve: %s: %v", peer, err)
				return
			}
		}
		up, err := net.Dial("unix", *agent)
		if err != nil {
			log.Printf("serve: %s: dial agent: %v", peer, err)
			return
		}
		defer up.Close()
		log.Printf("serve: %s: open", peer)
		splice(c, up, *idle)
		log.Printf("serve: %s: close", peer)
	})
}

func runConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	socket := fs.String("socket", envOr("SSH_AGENT_RELAY_SOCKET", defaultSocket), "unix socket to create for SSH_AUTH_SOCK")
	upstream := fs.String("upstream", envOr("SSH_AGENT_RELAY_UPSTREAM", defaultUpstream), "host relay TCP address")
	tokenFile := fs.String("token-file", os.Getenv("SSH_AGENT_RELAY_TOKEN_FILE"), "file holding the shared token (or set $SSH_AGENT_RELAY_TOKEN)")
	replace := fs.Bool("replace", false, "remove a stale --socket before listening")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second, "upstream dial timeout")
	idle := fs.Duration("idle-timeout", 0, "drop a relayed connection after this idle period (0 = never)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token, err := loadToken(*tokenFile)
	if err != nil {
		return err
	}

	if *replace {
		if err := clearStaleSocket(*socket); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			log.Printf("connect: %s already in use — assuming a relay is running", *socket)
			return nil
		}
		return fmt.Errorf("connect: %w", err)
	}
	if err := os.Chmod(*socket, 0o600); err != nil {
		log.Printf("connect: chmod %s: %v", *socket, err)
	}
	log.Printf("connect: %s -> %s (token=%t)", *socket, *upstream, token != nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	frame := tokenFrame(token)
	return accept(ctx, ln, func(c net.Conn) {
		defer c.Close()
		up, err := net.DialTimeout("tcp", *upstream, *dialTimeout)
		if err != nil {
			log.Printf("connect: dial %s: %v", *upstream, err)
			return
		}
		defer up.Close()
		if frame != nil {
			if _, err := up.Write(frame); err != nil {
				log.Printf("connect: send token: %v", err)
				return
			}
		}
		splice(c, up, *idle)
	})
}

func accept(ctx context.Context, ln net.Listener, handle func(net.Conn)) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				log.Print("shutdown")
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go handle(c)
	}
}

// activity tracks how many relayed connections are open and, once that count
// returns to zero, when it last did so — enough for serve --idle-exit.
type activity struct {
	mu       sync.Mutex
	open     int
	lastZero time.Time
}

func (a *activity) inc() {
	a.mu.Lock()
	a.open++
	a.mu.Unlock()
}

func (a *activity) dec() {
	a.mu.Lock()
	a.open--
	if a.open == 0 {
		a.lastZero = time.Now()
	}
	a.mu.Unlock()
}

func (a *activity) idleFor(d time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.open == 0 && !a.lastZero.IsZero() && time.Since(a.lastZero) >= d
}

// withIdleExit returns a context that is cancelled when act reports no activity
// for d, or when parent is cancelled.
func withIdleExit(parent context.Context, act *activity, d time.Duration) context.Context {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		t := time.NewTicker(min(d, idleCheckEvery))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if act.idleFor(d) {
					log.Printf("serve: idle for %s, exiting", d)
					cancel()
					return
				}
			}
		}
	}()
	return ctx
}

// splice copies bytes in both directions until either side ends, honouring an
// optional idle timeout. Each direction half-closes its write side so the peer
// observes EOF.
func splice(a, b net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)
	go copyHalf(a, b, idle, done)
	go copyHalf(b, a, idle, done)
	<-done
	<-done
}

type closeWriter interface{ CloseWrite() error }

func copyHalf(dst, src net.Conn, idle time.Duration, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 32*1024)
	for {
		if idle > 0 {
			_ = src.SetReadDeadline(time.Now().Add(idle))
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if idle > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	if cw, ok := dst.(closeWriter); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.SetReadDeadline(time.Now())
	}
}

func loadToken(file string) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv("SSH_AGENT_RELAY_TOKEN")); v != "" {
		return []byte(v), nil
	}
	if file == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("token-file: %w", err)
	}
	tok := []byte(strings.TrimSpace(string(raw)))
	switch {
	case len(tok) == 0:
		return nil, fmt.Errorf("token-file %s: empty", file)
	case len(tok) >= tokenReadLimit:
		return nil, fmt.Errorf("token-file %s: token too long", file)
	}
	return tok, nil
}

func tokenFrame(token []byte) []byte {
	if token == nil {
		return nil
	}
	return append(append(make([]byte, 0, len(token)+1), token...), '\n')
}

func expectToken(c net.Conn, want []byte) error {
	_ = c.SetReadDeadline(time.Now().Add(handshakeGrace))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 0, len(want)+1)
	one := make([]byte, 1)
	for {
		n, err := c.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			buf = append(buf, one[0])
			if len(buf) >= tokenReadLimit {
				return errors.New("token handshake: too long")
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("token handshake: %w", err)
		}
	}
	if subtle.ConstantTimeCompare(buf, want) != 1 {
		return errors.New("token handshake: mismatch")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func checkSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return errors.New("not a socket")
	}
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clearStaleSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, derr := net.DialTimeout("unix", path, 200*time.Millisecond); derr == nil {
		_ = c.Close()
		return fmt.Errorf("%s has a live listener", path)
	}
	return os.Remove(path)
}
