package sshcheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const verifyCmd = "test -e /etc/NIXOS && readlink /run/current-system"

var fast = Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond}

func checker(signer ssh.Signer, port int) Checker {
	return Checker{Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Port: port, Backoff: fast}
}

func ok(string) (string, uint32) { return "", 0 }

func withTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestWaitForSSHSucceeds(t *testing.T) {
	key := newSigner(t)
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), ok)
	if err := checker(key, port).WaitForSSH(withTimeout(t, 5*time.Second), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSSHRetriesUntilServerAppears(t *testing.T) {
	key := newSigner(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		startServer(t, addr, key.PublicKey(), ok)
	}()
	if err := checker(key, port).WaitForSSH(withTimeout(t, 5*time.Second), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSSHTimesOutWithLastError(t *testing.T) {
	key := newSigner(t)
	other := newSigner(t)
	port := startServer(t, "127.0.0.1:0", other.PublicKey(), ok)
	err := checker(key, port).WaitForSSH(withTimeout(t, 300*time.Millisecond), "127.0.0.1")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("err = %v, want deadline exceeded with auth error", err)
	}
}

func TestVerifyNixOSMatches(t *testing.T) {
	key := newSigner(t)
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), func(cmd string) (string, uint32) {
		if cmd != verifyCmd {
			return "unexpected command", 127
		}
		return "/nix/store/abc-nixos-system-web1\n", 0
	})
	err := checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "", "127.0.0.1", "/nix/store/abc-nixos-system-web1")
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyNixOSRetriesWhileInstallerStillRunning(t *testing.T) {
	key := newSigner(t)
	var n atomic.Int32
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), func(string) (string, uint32) {
		if n.Add(1) < 3 {
			return "/nix/store/xyz-kexec-installer\n", 0
		}
		return "/nix/store/abc-nixos-system-web1\n", 0
	})
	err := checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "", "127.0.0.1", "/nix/store/abc-nixos-system-web1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() < 3 {
		t.Fatalf("checked %d times, want >= 3", n.Load())
	}
}

func TestVerifyNixOSTimesOutOnMismatch(t *testing.T) {
	key := newSigner(t)
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), func(string) (string, uint32) {
		return "/nix/store/wrong\n", 0
	})
	err := checker(key, port).VerifyNixOS(withTimeout(t, 300*time.Millisecond), "", "127.0.0.1", "/nix/store/right")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `"/nix/store/wrong"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyNixOSFailsWhenNotNixOS(t *testing.T) {
	key := newSigner(t)
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), func(string) (string, uint32) { return "", 1 })
	err := checker(key, port).VerifyNixOS(withTimeout(t, 300*time.Millisecond), "", "127.0.0.1", "/nix/store/right")
	if err == nil {
		t.Fatal("want error")
	}
}

func TestAuthMethodsFromIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, generateKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, closeFn, err := AuthMethods(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if len(auth) != 1 {
		t.Fatalf("got %d auth methods", len(auth))
	}
}

func TestAuthMethodsWithoutIdentityOrAgent(t *testing.T) {
	_, _, err := AuthMethods("", "")
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("err = %v", err)
	}
}

func generateKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func TestVerifyNixOSLogsInAsUser(t *testing.T) {
	key := newSigner(t)
	port := startServerAs(t, "127.0.0.1:0", "mp", key.PublicKey(), func(string) (string, uint32) {
		return "/nix/store/abc\n", 0
	})
	if err := checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "mp", "127.0.0.1", "/nix/store/abc"); err != nil {
		t.Fatal(err)
	}
	if err := checker(key, port).VerifyNixOS(withTimeout(t, 300*time.Millisecond), "", "127.0.0.1", "/nix/store/abc"); err == nil {
		t.Fatal("root login accepted by a server that only allows mp")
	}
}
