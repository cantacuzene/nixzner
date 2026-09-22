package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"hinfect/internal/hetzner"
)

func runCLI(env map[string]string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	var created atomic.Pointer[hetzner.Server]
	code := run(context.Background(), args, &stdout, &stderr, func(k string) string { return env[k] }, &created)
	return code, stdout.String(), stderr.String()
}

var fullArgs = []string{"create", "web1", "--flake", ".#web1", "--type", "cx32", "--location", "fsn1", "--ssh-key", "admin"}

func TestHelp(t *testing.T) {
	code, stdout, _ := runCLI(nil, "--help")
	if code != 0 || !strings.Contains(stdout, "create") {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
}

func TestCreateRequiresFlags(t *testing.T) {
	code, _, stderr := runCLI(nil, "create", "web1")
	if code != 1 || !strings.Contains(stderr, "required flag") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCreateRequiresName(t *testing.T) {
	code, _, _ := runCLI(nil, "create")
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
}

func TestCreateRejectsBadFlake(t *testing.T) {
	args := append(append([]string{}, fullArgs...), "--flake", "web1")
	code, _, stderr := runCLI(map[string]string{"HCLOUD_TOKEN": "x"}, args...)
	if code != 1 || !strings.Contains(stderr, "must have the form") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCreateRejectsReservedLabel(t *testing.T) {
	args := append(append([]string{}, fullArgs...), "--label", "managed-by=me")
	code, _, stderr := runCLI(map[string]string{"HCLOUD_TOKEN": "x"}, args...)
	if code != 1 || !strings.Contains(stderr, "reserved") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestCreateRequiresToken(t *testing.T) {
	code, _, stderr := runCLI(nil, fullArgs...)
	if code != 1 || !strings.Contains(stderr, "HCLOUD_TOKEN is not set") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
