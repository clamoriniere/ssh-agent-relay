package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:17890":           true,
		"[::1]:17890":               true,
		"localhost:17890":           true,
		":17890":                    true,
		"0.0.0.0:17890":             false,
		"192.168.1.5:22":            false,
		"host.docker.internal:9999": false,
	}
	for addr, want := range cases {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestTokenFrame(t *testing.T) {
	if tokenFrame(nil) != nil {
		t.Fatal("tokenFrame(nil) should be nil")
	}
	got := tokenFrame([]byte("abc"))
	if string(got) != "abc\n" {
		t.Fatalf("tokenFrame = %q, want %q", got, "abc\n")
	}
}

func TestExpectToken(t *testing.T) {
	tok := []byte("s3cr3t-token")

	t.Run("match", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go func() { _, _ = a.Write(tokenFrame(tok)) }()
		if err := expectToken(b, tok); err != nil {
			t.Fatalf("expectToken: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go func() { _, _ = a.Write(tokenFrame([]byte("wrong"))) }()
		if err := expectToken(b, tok); err == nil {
			t.Fatal("expectToken: want error on mismatch, got nil")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		start := time.Now()
		if err := expectToken(b, tok); err == nil {
			t.Fatal("expectToken: want error on silent peer, got nil")
		}
		if elapsed := time.Since(start); elapsed > handshakeGrace+2*time.Second {
			t.Fatalf("expectToken blocked %s, want ~%s", elapsed, handshakeGrace)
		}
	})
}

func TestLoadToken(t *testing.T) {
	t.Setenv("SSH_AGENT_RELAY_TOKEN", "")

	if tok, err := loadToken(""); err != nil || tok != nil {
		t.Fatalf("loadToken(\"\") = %q, %v; want nil, nil", tok, err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "token")

	if err := os.WriteFile(f, []byte("  hello-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadToken(f)
	if err != nil || string(tok) != "hello-token" {
		t.Fatalf("loadToken(file) = %q, %v; want %q, nil", tok, err, "hello-token")
	}

	if err := os.WriteFile(f, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(f); err == nil {
		t.Fatal("loadToken(empty file): want error, got nil")
	}

	if _, err := loadToken(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("loadToken(missing file): want error, got nil")
	}

	t.Setenv("SSH_AGENT_RELAY_TOKEN", "  env-token ")
	tok, err = loadToken(f)
	if err != nil || string(tok) != "env-token" {
		t.Fatalf("loadToken with env = %q, %v; want %q, nil", tok, err, "env-token")
	}
}

func TestClearStaleSocket(t *testing.T) {
	dir := t.TempDir()

	if err := clearStaleSocket(filepath.Join(dir, "missing.sock")); err != nil {
		t.Fatalf("clearStaleSocket(missing) = %v, want nil", err)
	}

	reg := filepath.Join(dir, "regular")
	if err := os.WriteFile(reg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleSocket(reg); err == nil {
		t.Fatal("clearStaleSocket(regular file): want error, got nil")
	}

	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := clearStaleSocket(live); err == nil {
		t.Fatal("clearStaleSocket(live listener): want error, got nil")
	}
	ln.Close()
	if err := clearStaleSocket(live); err != nil {
		t.Fatalf("clearStaleSocket(stale socket) = %v, want nil", err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("stale socket not removed: stat err = %v", err)
	}
}

func TestSplice(t *testing.T) {
	c1a, c1b := net.Pipe()
	c2a, c2b := net.Pipe()

	go splice(c1b, c2a, 0)

	// c1a -> c1b -> (splice) -> c2a -> c2b
	go func() {
		_, _ = c1a.Write([]byte("ping"))
		_ = c1a.Close()
	}()

	buf := make([]byte, 4)
	_ = c2b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c2b.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("splice relayed %q, err %v; want %q", buf[:n], err, "ping")
	}
	c1b.Close()
	c2a.Close()
	c2b.Close()
}
