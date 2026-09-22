//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hinfect/internal/hetzner"
)

// Run with:
//
//	HCLOUD_TOKEN=... HINFECT_TEST_SSH_KEY=<hetzner key name> \
//	HINFECT_TEST_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" \
//	go test -tags integration -run TestCreateIntegration -timeout 45m -v ./cmd/hinfect/
//
// The matching private key must be loaded in ssh-agent. Costs a few cents.
func TestCreateIntegration(t *testing.T) {
	token := os.Getenv("HCLOUD_TOKEN")
	sshKey := os.Getenv("HINFECT_TEST_SSH_KEY")
	pubKey := os.Getenv("HINFECT_TEST_PUBKEY")
	if token == "" || sshKey == "" || pubKey == "" {
		t.Skip("set HCLOUD_TOKEN, HINFECT_TEST_SSH_KEY and HINFECT_TEST_PUBKEY")
	}
	serverType := envOr("HINFECT_TEST_TYPE", "cx23")
	location := envOr("HINFECT_TEST_LOCATION", "nbg1")
	host := envOr("HINFECT_TEST_HOST", "example")

	flakeDir := t.TempDir()
	if err := os.CopyFS(flakeDir, os.DirFS("../../examples/flake")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "authorized_keys"), []byte(pubKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var created atomic.Pointer[hetzner.Server]
	t.Cleanup(func() {
		if s := created.Load(); s != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := hetzner.New(token).DeleteServer(ctx, s.ID); err != nil {
				t.Errorf("deleting server %d: %v — delete it by hand", s.ID, err)
			}
		}
	})

	name := fmt.Sprintf("hinfect-it-%d", time.Now().Unix())
	var stdout bytes.Buffer
	code := run(context.Background(), []string{
		"create", name,
		"--flake", "path:" + flakeDir + "#" + host,
		"--type", serverType,
		"--location", location,
		"--ssh-key", sshKey,
		"--log-dir", t.TempDir(),
	}, &stdout, os.Stderr, os.Getenv, &created)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 4 || fields[0] != name || !strings.HasPrefix(fields[3], "/nix/store/") {
		t.Fatalf("stdout = %q, want '<name> <ipv4> <ipv6> <store-path>'", stdout.String())
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
