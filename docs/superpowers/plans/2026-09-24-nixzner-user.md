# nixzner `--user` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `nixzner create` and `nixzner rebuild` accept `--user <name>`: the installed system gets that login user (holding the flake's root SSH keys, passwordless sudo, nix trusted), root SSH login is disabled, and nixzner connects as that user after install.

**Architecture:** A new `internal/flakewrap` package writes a wrapper flake to a temp dir that `extendModules` the user's host with a generated user module; every nix tool in the run gets the wrapper instead of the user's flake. `sshcheck` and `nixos.Remote` learn a login user (default root, `sudo` for non-root). `create` verifies as the user and records a `nixzner-user` label; `rebuild` reads that label to pick the target and login users and refuses a change of user.

**Tech Stack:** Go 1.26, cobra, hcloud-go v2, golang.org/x/crypto/ssh, Nix flakes (`extendModules`), nixos-anywhere.

**Spec:** `docs/superpowers/specs/2026-09-24-nixzner-user-design.md`

## Global Constraints

- Go module is `nixzner`; no new third-party dependencies.
- `--user` must match `^[a-z_][a-z0-9_-]{0,31}$` and must not be `root`.
- Label key is `nixzner-user`.
- Without `--user` (and, for rebuild, without a `nixzner-user` label) behaviour is unchanged: all existing tests keep passing unmodified except for signature updates called out below.
- Generated files are named `flake.nix` and `nixzner-user.nix`; the wrapper is referenced as `path:<dir>#<host>` where `<dir>` is a real path (symlinks resolved).
- `create` still uses root for wait-for-ssh and nixos-anywhere; only verify uses the user.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- `internal/sshcheck` has two known timing-flaky tests (`TestWaitForSSHTimesOutWithLastError`, `TestVerifyNixOSTimesOutOnMismatch`); a lone failure of either is not caused by this work — rerun.

## Refinement of the spec

The spec lists `sshcheck.Checker` gaining a `User` field. `create` needs root for wait-for-ssh and the user for verify from the same checker, so instead `VerifyNixOS` takes the user as a parameter (`""` means root) and `WaitForSSH` stays root-only.

## Review Focus

- A relative flake (`.#web1`) with `--user`: inside the wrapper `.` would mean the temp dir — `Wrap` must refuse it (Task 2 test `TestWrapRejectsRelativeFlake`, Task 5 test `TestRunWithUserRejectsRelativeFlake`).
- A flake path containing `"`, `\` or `${`: must be escaped in the generated Nix (Task 2 test `TestNixStringEscapes`).
- `rebuild` without `--user` on a server labelled `nixzner-user=mp`: must log in and verify as `mp`, not root (Task 6 test `TestRunUsesUserFromLabel`).
- `rebuild --user alice` on a server labelled `mp`: refused before anything runs (Task 6 test `TestRunRefusesChangingUser`).
- A failed run must not leave the wrapper temp dir behind (Task 5 test `TestRunWithUserRemovesWrapperOnFailure`, Task 6 test `TestRunConvertsRootServerToUser` checks cleanup).

---

## File Structure

- Modify `internal/config/labels.go` — add `LabelUser`.
- Modify `internal/config/options.go` — `ValidateUser`, `CreateOptions.User`, validate it.
- Create `internal/flakewrap/flakewrap.go` — `UserModule`, `WrapperFlake`, `nixString`, `Wrap`.
- Create `internal/flakewrap/flakewrap_test.go`.
- Modify `internal/sshcheck/sshcheck.go` — user parameter on `VerifyNixOS`, plumbed through `poll`/`attempt`.
- Modify `internal/sshcheck/server_test.go` — `startServerAs` accepting one user.
- Modify `internal/nixos/nixos.go` — `Remote.User`, `login()`, `sudo()`.
- Modify `internal/create/create.go` — wrap, verify as user, `nixzner-user` label.
- Modify `internal/rebuild/rebuild.go` — target/login user resolution, wrap, label, refusal, hint.
- Modify `cmd/nixzner/main.go` — `--user` on both commands.
- Modify `cmd/nixzner/integration_test.go` — `NIXZNER_TEST_USER`.
- Modify `README.md`.

---

### Task 1: `config` — user validation and label

**Files:**
- Modify: `internal/config/labels.go:10-17`
- Modify: `internal/config/options.go`
- Test: `internal/config/options_test.go`

**Interfaces:**
- Produces: `config.LabelUser = "nixzner-user"`; `func config.ValidateUser(name string) error`; field `config.CreateOptions.User string`.

- [ ] **Step 1: Write the failing tests** — append to `internal/config/options_test.go`:

```go
func TestValidateUser(t *testing.T) {
	for _, ok := range []string{"mp", "_svc", "a-b_1", strings.Repeat("a", 32)} {
		if err := ValidateUser(ok); err != nil {
			t.Errorf("ValidateUser(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "root", "Mp", "1mp", "m p", "mp!", strings.Repeat("a", 33)} {
		if err := ValidateUser(bad); err == nil {
			t.Errorf("ValidateUser(%q) = nil, want error", bad)
		}
	}
	if err := ValidateUser("root"); err == nil || !strings.Contains(err.Error(), `must not be "root"`) {
		t.Errorf("root error = %v", err)
	}
}

func TestValidateChecksUser(t *testing.T) {
	o := validOpts()
	o.User = "Root!"
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "--user") {
		t.Fatalf("err = %v", err)
	}
	o.User = "mp"
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}
```

(Add `"strings"` to the test imports if missing.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/`
Expected: build failure `undefined: ValidateUser` / `o.User undefined`.

- [ ] **Step 3: Implement**

In `internal/config/labels.go`, add to the const block after `LabelStatus`:

```go
	LabelUser        = "nixzner-user"
```

In `internal/config/options.go`, add field to `CreateOptions` after `Identity`:

```go
	User       string // login user to create; empty keeps root login
```

Add after `ValidateServerName`:

```go
var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidateUser checks name is usable as the login user nixzner creates.
func ValidateUser(name string) error {
	if name == "root" {
		return errors.New(`--user must not be "root" (leave it out to keep root login)`)
	}
	if !userRe.MatchString(name) {
		return fmt.Errorf("--user %q must be a Linux user name (a-z, 0-9, _, -; starts with a letter or _; at most 32 chars)", name)
	}
	return nil
}
```

In `Validate`, after the server name check:

```go
	if o.User != "" {
		if err := ValidateUser(o.User); err != nil {
			errs = append(errs, err)
		}
	}
```

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal && go test ./internal/config/`
Expected: no gofmt output; `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): --user validation and nixzner-user label

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `flakewrap` — generate the wrapper flake

**Files:**
- Create: `internal/flakewrap/flakewrap.go`
- Test: `internal/flakewrap/flakewrap_test.go`

**Interfaces:**
- Consumes: `config.FlakeRef{URI, Host string}`.
- Produces: `func flakewrap.Wrap(f config.FlakeRef, user string) (config.FlakeRef, func(), error)` — with `user == ""` returns `f`, a no-op cleanup, nil. Otherwise returns `FlakeRef{URI: "path:<realdir>", Host: f.Host}` and a cleanup that removes `<realdir>`. Errors if `f.URI` starts with `.`. Also `func UserModule(user string) string`, `func WrapperFlake(targetURI, host string) string`.

- [ ] **Step 1: Write the failing tests** — `internal/flakewrap/flakewrap_test.go`:

```go
package flakewrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nixzner/internal/config"
)

func TestUserModule(t *testing.T) {
	m := UserModule("mp")
	for _, want := range []string{
		`users.users."mp" = {`,
		`isNormalUser = true;`,
		`openssh.authorizedKeys = { inherit (rootKeys) keys keyFiles; };`,
		`{ users = [ "mp" ]; commands = [ { command = "ALL"; options = [ "NOPASSWD" ]; } ]; }`,
		`nix.settings.trusted-users = [ "mp" ];`,
		`services.openssh.settings.PermitRootLogin = lib.mkForce "no";`,
		`assertion = userKeys.keys != [ ] || userKeys.keyFiles != [ ];`,
		`message = "nixzner --user: mp has no SSH keys (the flake gives none to mp or to root)";`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("module does not contain %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "@USER@") {
		t.Errorf("unreplaced placeholder:\n%s", m)
	}
}

func TestWrapperFlake(t *testing.T) {
	got := WrapperFlake("path:/src/infra", "web1")
	want := `{
  inputs.target.url = "path:/src/infra";
  outputs = { target, ... }: {
    nixosConfigurations."web1" =
      target.nixosConfigurations."web1".extendModules { modules = [ ./nixzner-user.nix ]; };
  };
}
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNixStringEscapes(t *testing.T) {
	if got, want := nixString(`a"b\c${d}`), `"a\"b\\c\${d}"`; got != want {
		t.Fatalf("nixString = %s, want %s", got, want)
	}
}

func TestWrapWithoutUserIsNoop(t *testing.T) {
	f := config.FlakeRef{URI: ".", Host: "web1"}
	got, cleanup, err := Wrap(f, "")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if got != f {
		t.Fatalf("got %+v, want %+v", got, f)
	}
}

func TestWrapWritesWrapperAndCleansUp(t *testing.T) {
	f := config.FlakeRef{URI: "path:/src/infra", Host: "web1"}
	got, cleanup, err := Wrap(f, "mp")
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := strings.CutPrefix(got.URI, "path:")
	if !ok || !filepath.IsAbs(dir) || got.Host != "web1" {
		t.Fatalf("wrapped = %+v", got)
	}
	if real, _ := filepath.EvalSymlinks(dir); real != dir {
		t.Errorf("wrapper dir %q is not a real path (%q)", dir, real)
	}
	flake, _ := os.ReadFile(filepath.Join(dir, "flake.nix"))
	module, _ := os.ReadFile(filepath.Join(dir, "nixzner-user.nix"))
	if string(flake) != WrapperFlake("path:/src/infra", "web1") || string(module) != UserModule("mp") {
		t.Errorf("files = %q / %q", flake, module)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("wrapper dir still exists after cleanup: %v", err)
	}
}

func TestWrapRejectsRelativeFlake(t *testing.T) {
	_, _, err := Wrap(config.FlakeRef{URI: ".", Host: "web1"}, "mp")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/flakewrap/`
Expected: build failure `undefined: UserModule` (and others).

- [ ] **Step 3: Implement** — `internal/flakewrap/flakewrap.go`:

```go
// Package flakewrap adds nixzner's login-user module on top of a flake's
// host without editing the flake, by generating a wrapper flake.
package flakewrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nixzner/internal/config"
)

const moduleTemplate = `{ config, lib, ... }:
let
  rootKeys = config.users.users.root.openssh.authorizedKeys;
  userKeys = config.users.users."@USER@".openssh.authorizedKeys;
in
{
  users.users."@USER@" = {
    isNormalUser = true;
    openssh.authorizedKeys = { inherit (rootKeys) keys keyFiles; };
  };
  security.sudo.extraRules = [
    { users = [ "@USER@" ]; commands = [ { command = "ALL"; options = [ "NOPASSWD" ]; } ]; }
  ];
  nix.settings.trusted-users = [ "@USER@" ];
  services.openssh.settings.PermitRootLogin = lib.mkForce "no";
  assertions = [
    {
      assertion = userKeys.keys != [ ] || userKeys.keyFiles != [ ];
      message = "nixzner --user: @USER@ has no SSH keys (the flake gives none to @USER@ or to root)";
    }
  ];
}
`

// UserModule is the NixOS module that creates user with the SSH keys the
// flake gives root, lets it sudo and use nix without a password, and
// disables root SSH login. user must pass config.ValidateUser.
func UserModule(user string) string {
	return strings.ReplaceAll(moduleTemplate, "@USER@", user)
}

// WrapperFlake is a flake whose nixosConfigurations.<host> is target's,
// extended with ./nixzner-user.nix.
func WrapperFlake(targetURI, host string) string {
	h := nixString(host)
	return fmt.Sprintf(`{
  inputs.target.url = %s;
  outputs = { target, ... }: {
    nixosConfigurations.%s =
      target.nixosConfigurations.%s.extendModules { modules = [ ./nixzner-user.nix ]; };
  };
}
`, nixString(targetURI), h, h)
}

// nixString quotes s as a Nix string literal.
func nixString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `${`, `\${`)
	return `"` + r.Replace(s) + `"`
}

// Wrap returns a flake reference with user's module added to f's host, and
// a cleanup that deletes the generated files. With no user it returns f.
// f.URI must not be relative: inside the wrapper "." would mean the
// wrapper's own directory.
func Wrap(f config.FlakeRef, user string) (config.FlakeRef, func(), error) {
	if user == "" {
		return f, func() {}, nil
	}
	if strings.HasPrefix(f.URI, ".") {
		return config.FlakeRef{}, nil, fmt.Errorf("flakewrap: flake %q must be resolved to an absolute path first", f.URI)
	}
	tmp, err := os.MkdirTemp("", "nixzner-wrap-*")
	if err != nil {
		return config.FlakeRef{}, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	dir, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		cleanup()
		return config.FlakeRef{}, nil, err
	}
	files := map[string]string{
		"flake.nix":        WrapperFlake(f.URI, f.Host),
		"nixzner-user.nix": UserModule(user),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			cleanup()
			return config.FlakeRef{}, nil, err
		}
	}
	return config.FlakeRef{URI: "path:" + dir, Host: f.Host}, cleanup, nil
}
```

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal && go vet ./internal/flakewrap && go test ./internal/flakewrap/`
Expected: `ok`.

- [ ] **Step 5: Check the generated flake with real Nix** (one-off, not a test). From the repo root:

```bash
export PATH="$HOME/.nix-profile/bin:/nix/var/nix/profiles/default/bin:$PATH"
D=$(mktemp -d) && D=$(cd "$D" && pwd -P) && cp -R examples/flake/. "$D"/ && cat ~/.ssh/id_ed25519.pub > "$D/authorized_keys"
W=$(mktemp -d) && W=$(cd "$W" && pwd -P)
mkdir -p .tmpgen && cat > .tmpgen/main.go <<'EOF'
package main

import (
	"os"
	"path/filepath"

	"nixzner/internal/flakewrap"
)

func main() {
	os.WriteFile(filepath.Join(os.Args[1], "flake.nix"), []byte(flakewrap.WrapperFlake("path:"+os.Args[2], "example")), 0o644)
	os.WriteFile(filepath.Join(os.Args[1], "nixzner-user.nix"), []byte(flakewrap.UserModule("mp")), 0o644)
}
EOF
go run ./.tmpgen "$W" "$D"; rm -rf .tmpgen
(cd "$W" && nix eval --raw .#nixosConfigurations.example.config.services.openssh.settings.PermitRootLogin && echo && nix eval --json .#nixosConfigurations.example.config.users.users.mp.openssh.authorizedKeys.keyFiles)
rm -rf "$D" "$W"
```

Expected: `no`, then a JSON list with one `/nix/store/…-source/authorized_keys` path.

- [ ] **Step 6: Commit**

```bash
git add internal/flakewrap
git commit -m "feat(flakewrap): wrapper flake adding a login-user module

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `sshcheck` — verify as a given user

**Files:**
- Modify: `internal/sshcheck/sshcheck.go:37-110`
- Modify: `internal/sshcheck/server_test.go:30-57`
- Modify: `internal/sshcheck/sshcheck_test.go` (existing `VerifyNixOS` calls gain `""`)
- Modify: `internal/create/create.go:33-34,117`, `internal/create/create_test.go:104`
- Modify: `internal/rebuild/rebuild.go` (SSH interface + verify call), `internal/rebuild/rebuild_test.go` (fake)

**Interfaces:**
- Produces: `func (c Checker) VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error` (`user == ""` means root). `WaitForSSH(ctx, host)` unchanged (root). `create.SSH` and `rebuild.SSH` interfaces use the new `VerifyNixOS` signature.

- [ ] **Step 1: Write the failing tests**

In `internal/sshcheck/server_test.go`, replace `startServer` with:

```go
// startServer serves SSH on addr ("127.0.0.1:0" for any port), accepting
// only root with the authorized key, and returns the port it listens on.
func startServer(t *testing.T, addr string, authorized ssh.PublicKey, handle execHandler) int {
	t.Helper()
	return startServerAs(t, addr, "root", authorized, handle)
}

// startServerAs is startServer accepting only user.
func startServerAs(t *testing.T, addr, user string, authorized ssh.PublicKey, handle execHandler) int {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == user && bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	cfg.AddHostKey(newSigner(t))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(c, cfg, handle)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}
```

Append to `internal/sshcheck/sshcheck_test.go`:

```go
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
```

In every existing `VerifyNixOS(...)` call in `sshcheck_test.go`, insert `""` as the second argument, e.g.
`checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "", "127.0.0.1", "/nix/store/abc-nixos-system-web1")`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/sshcheck/`
Expected: build failure `too many arguments in call to checker(key, port).VerifyNixOS`.

- [ ] **Step 3: Implement** — in `internal/sshcheck/sshcheck.go`:

```go
// WaitForSSH polls until root can log in and run a command.
func (c Checker) WaitForSSH(ctx context.Context, host string) error {
	return c.poll(ctx, "root", host, func(client *ssh.Client) error {
		_, err := run(client, "true")
		return err
	})
}

// VerifyNixOS polls until user ("" means root) can log in to a host running
// NixOS with /run/current-system pointing at wantOutPath. Polling matters:
// right after install the kexec installer (also NixOS) may still be
// answering SSH.
func (c Checker) VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error {
	return c.poll(ctx, user, host, func(client *ssh.Client) error {
```

(body unchanged). Change `poll(ctx context.Context, host string, check …)` to `poll(ctx context.Context, user, host string, check …)` and its call to `c.attempt(ctx, user, host, check)`. Change `attempt` to `attempt(ctx context.Context, user, host string, check …)` and in its body, before building `cfg`:

```go
	if user == "" {
		user = "root"
	}
```

and set `User: user,` in the `ssh.ClientConfig`.

Update callers:
- `internal/create/create.go` interface: `VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error`; call: `d.SSH.VerifyNixOS(ctx, "", res.Server.IPv4, outPath)` (Task 5 replaces `""`).
- `internal/create/create_test.go` fake: `func (f *fakes) VerifyNixOS(ctx context.Context, user, host, want string) error` (body unchanged).
- `internal/rebuild/rebuild.go` interface likewise; call `d.SSH.VerifyNixOS(ctx, "", remote.Host, res.StorePath)` (Task 6 replaces `""`).
- `internal/rebuild/rebuild_test.go` fake: `func (f *fakes) VerifyNixOS(_ context.Context, user, host, want string) error { return f.record("verify %s %s as %s", host, want, cmp.Or(user, "root")) }` (add `"cmp"` import) and change the expected line in `TestRunRebuildsInOrder` to `"verify 203.0.113.10 " + storePath + " as root",`.

- [ ] **Step 4: Run tests**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: all `ok` (see Global Constraints about the two flaky sshcheck tests).

- [ ] **Step 5: Commit**

```bash
git add internal/sshcheck internal/create internal/rebuild
git commit -m "feat(sshcheck): VerifyNixOS logs in as a given user

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `nixos.Remote` — login user and sudo

**Files:**
- Modify: `internal/nixos/nixos.go` (the `Remote` block)
- Test: `internal/nixos/nixos_test.go`

**Interfaces:**
- Produces: field `nixos.Remote.User string` (`""` means root). For a non-root user the store URL is `ssh-ng://<user>@<host>?compress=true`, ssh target `<user>@<host>`, and switch commands are prefixed with `sudo `.

- [ ] **Step 1: Write the failing tests** — append to `internal/nixos/nixos_test.go`:

```go
func TestBuildRemoteAsUser(t *testing.T) {
	r := &fakeRunner{stdout: "/nix/store/abc\n"}
	if _, err := (Nix{Runner: r}).BuildRemote(context.Background(), flake(t, ".#web1"), Remote{Host: "h", User: "mp"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls[0].args, "ssh-ng://mp@h?compress=true") {
		t.Fatalf("args = %v, want store ssh-ng://mp@h", r.calls[0].args)
	}
}

func TestActivateAsUserUsesSudo(t *testing.T) {
	r := &fakeRunner{}
	if err := (Nix{Runner: r}).Activate(context.Background(), Remote{Host: "h", User: "mp"}, "/nix/store/abc", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "mp@h", "--",
		"sudo /nix/store/abc/bin/switch-to-configuration test"}
	if !slices.Equal(r.calls[0].args, want) {
		t.Fatalf("args = %v, want %v", r.calls[0].args, want)
	}
}

func TestMakeBootDefaultAsUserUsesSudo(t *testing.T) {
	r := &fakeRunner{}
	if err := (Nix{Runner: r}).MakeBootDefault(context.Background(), Remote{Host: "h", User: "mp"}, "/nix/store/abc", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := "sudo nix-env -p /nix/var/nix/profiles/system --set /nix/store/abc && sudo /nix/store/abc/bin/switch-to-configuration boot"
	if got := r.calls[0].args[len(r.calls[0].args)-1]; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/nixos/`
Expected: build failure `unknown field User in struct literal of type Remote`.

- [ ] **Step 3: Implement** — in `internal/nixos/nixos.go` replace the `Remote` struct and add helpers:

```go
// Remote is a running NixOS server reached over the system's ssh, so
// ~/.ssh/config and ~/.ssh/known_hosts apply.
type Remote struct {
	Host     string // IPv4 address
	User     string // login user; "" means root
	Identity string // optional private key file
}

func (r Remote) login() string {
	if r.User == "" {
		return "root"
	}
	return r.User
}

// sudo prefixes commands that need root when logged in as another user.
func (r Remote) sudo() string {
	if r.login() == "root" {
		return ""
	}
	return "sudo "
}
```

Then:
- In `BuildRemote`, the store argument becomes `"ssh-ng://"+r.login()+"@"+r.Host+"?compress=true"`.
- `Activate` command: `r.sudo()+outPath+"/bin/switch-to-configuration test"`.
- `MakeBootDefault` command: `r.sudo()+"nix-env -p /nix/var/nix/profiles/system --set "+outPath+" && "+r.sudo()+outPath+"/bin/switch-to-configuration boot"`.
- In `ssh`, the target becomes `r.login()+"@"+r.Host` and the error `fmt.Errorf("ssh %s@%s %q: %w", r.login(), r.Host, command, err)`.

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal && go test ./internal/nixos/ ./internal/rebuild/`
Expected: `ok` for both (existing root tests unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/nixos
git commit -m "feat(nixos): remote login user with sudo for switching

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `create --user`

**Files:**
- Modify: `internal/create/create.go`
- Test: `internal/create/create_test.go`

**Interfaces:**
- Consumes: `config.CreateOptions.User`, `config.LabelUser`, `flakewrap.Wrap`, `SSH.VerifyNixOS(ctx, user, host, want)`.
- Produces: with `opts.User != ""`: `Eval` and `Install` receive the wrapped flake; verify runs as `opts.User`; both label sets contain `nixzner-user=<user>`; the wrapper dir is removed when `Run` returns.

- [ ] **Step 1: Write the failing tests**

In `internal/create/create_test.go`, add fields to `fakes`: `evalFlake config.FlakeRef` and `verifyUser string`; in `Eval` add `f.evalFlake = fl`; in `VerifyNixOS` add `f.verifyUser = user`. Append:

```go
func userOpts(t *testing.T) config.CreateOptions {
	t.Helper()
	opts := testOpts(t)
	opts.User = "mp"
	opts.Flake = config.FlakeRef{URI: "path:" + t.TempDir(), Host: "web1"}
	return opts
}

func TestRunWithUserInstallsWrappedFlakeAndVerifiesAsUser(t *testing.T) {
	f := &fakes{}
	opts := userOpts(t)
	if _, err := Run(context.Background(), opts, testDeps(t, f, &bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	wrapped := f.installArgs.Flake
	if wrapped == opts.Flake || !strings.HasPrefix(wrapped.URI, "path:") || wrapped.Host != "web1" {
		t.Errorf("installed flake = %+v, want a wrapper of %+v", wrapped, opts.Flake)
	}
	if f.evalFlake != wrapped {
		t.Errorf("evaluated %+v but installed %+v", f.evalFlake, wrapped)
	}
	if f.verifyUser != "mp" {
		t.Errorf("verified as %q, want mp", f.verifyUser)
	}
	if f.createReq.Labels["nixzner-user"] != "mp" || f.labels["nixzner-user"] != "mp" {
		t.Errorf("labels = %v / %v, want nixzner-user=mp", f.createReq.Labels, f.labels)
	}
	if _, err := os.Stat(strings.TrimPrefix(wrapped.URI, "path:")); !os.IsNotExist(err) {
		t.Errorf("wrapper dir left behind: %v", err)
	}
}

func TestRunWithUserRemovesWrapperOnFailure(t *testing.T) {
	f := &fakes{installErr: errBoom}
	opts := userOpts(t)
	if _, err := Run(context.Background(), opts, testDeps(t, f, &bytes.Buffer{})); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(strings.TrimPrefix(f.installArgs.Flake.URI, "path:")); !os.IsNotExist(err) {
		t.Errorf("wrapper dir left behind: %v", err)
	}
}

func TestRunWithUserRejectsRelativeFlake(t *testing.T) {
	f := &fakes{}
	opts := testOpts(t) // flake ".#web1"
	opts.User = "mp"
	if _, err := Run(context.Background(), opts, testDeps(t, f, &bytes.Buffer{})); err == nil {
		t.Fatal("want error")
	}
	if len(f.Calls()) != 0 {
		t.Errorf("calls = %v, want none", f.Calls())
	}
}
```

In `TestRunSuccess`, add after the install-args check:

```go
	if f.verifyUser != "" || f.labels["nixzner-user"] != "" {
		t.Errorf("without --user: verified as %q, labels %v", f.verifyUser, f.labels)
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/create/`
Expected: FAIL — installed flake equals the original, verify user empty, no label.

- [ ] **Step 3: Implement** — in `internal/create/create.go`, right after `opts.Validate()`:

```go
	flake, cleanupWrap, err := flakewrap.Wrap(opts.Flake, opts.User)
	if err != nil {
		return Result{}, err
	}
	defer cleanupWrap()
```

(The existing `logFile, err := runlog.Start(d.TempDir)` line stays as is: `:=` is still valid because `logFile` is new.)

Add a label helper before `req`:

```go
	labels := func(status string) map[string]string {
		l := config.SystemLabels(opts.Labels, opts.Flake, status)
		if opts.User != "" {
			l[config.LabelUser] = opts.User
		}
		return l
	}
```

and use `Labels: labels(config.StatusInstalling)` in `req` and `labels(config.StatusReady)` in finalize. Replace `d.Nix.Eval(ctx, opts.Flake)` with `d.Nix.Eval(ctx, flake)`, `nixos.InstallArgs{Flake: opts.Flake, …}` with `nixos.InstallArgs{Flake: flake, …}`, and the verify call with `d.SSH.VerifyNixOS(ctx, opts.User, res.Server.IPv4, outPath)`. Import `"nixzner/internal/flakewrap"`.

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal && go vet ./internal/create && go test ./internal/create/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/create
git commit -m "feat(create): --user installs a wrapped flake and verifies as the user

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `rebuild --user`

**Files:**
- Modify: `internal/rebuild/rebuild.go`
- Test: `internal/rebuild/rebuild_test.go`

**Interfaces:**
- Consumes: `config.ValidateUser`, `config.LabelUser`, `flakewrap.Wrap`, `nixos.Remote.User`, `SSH.VerifyNixOS(ctx, user, host, want)`.
- Produces: field `rebuild.Options.User string`. Target user = `opts.User` or the `nixzner-user` label; login user = the label (root if none); a differing `--user` and label is refused in preflight; finalize sets `nixzner-user` to the target.

- [ ] **Step 1: Write the failing tests**

In `internal/rebuild/rebuild_test.go`, add field `evalFlake config.FlakeRef` to `fakes` and set it in `Eval` (`f.evalFlake = fl`). Append:

```go
func TestRunConvertsRootServerToUser(t *testing.T) {
	opts, d, f, _ := setup(t)
	opts.User = "mp"
	opts.Flake = config.FlakeRef{URI: "path:" + t.TempDir(), Host: "web1"}
	if _, err := Run(context.Background(), opts, d); err != nil {
		t.Fatal(err)
	}
	if f.remotes[0].User != "" {
		t.Errorf("logged in as %q, want root (no label yet)", f.remotes[0].User)
	}
	if f.evalFlake == opts.Flake || !strings.HasPrefix(f.evalFlake.URI, "path:") {
		t.Errorf("evaluated %+v, want a wrapper", f.evalFlake)
	}
	if !slices.Contains(f.calls, "verify 203.0.113.10 "+storePath+" as mp") {
		t.Errorf("calls = %v, want verify as mp", f.calls)
	}
	if f.setLabels["nixzner-user"] != "mp" {
		t.Errorf("labels = %v", f.setLabels)
	}
	if _, err := os.Stat(strings.TrimPrefix(f.evalFlake.URI, "path:")); !os.IsNotExist(err) {
		t.Errorf("wrapper dir left behind: %v", err)
	}
}

func TestRunUsesUserFromLabel(t *testing.T) {
	opts, d, f, _ := setup(t)
	opts.Flake = config.FlakeRef{URI: "path:" + t.TempDir(), Host: "web1"}
	f.labels["nixzner-user"] = "mp"
	if _, err := Run(context.Background(), opts, d); err != nil {
		t.Fatal(err)
	}
	if f.remotes[0].User != "mp" {
		t.Errorf("logged in as %q, want mp", f.remotes[0].User)
	}
	if f.evalFlake == opts.Flake {
		t.Errorf("flake not wrapped for the labelled user")
	}
	if !slices.Contains(f.calls, "verify 203.0.113.10 "+storePath+" as mp") {
		t.Errorf("calls = %v", f.calls)
	}
	if f.setLabels["nixzner-user"] != "mp" {
		t.Errorf("labels = %v", f.setLabels)
	}
}

func TestRunRefusesChangingUser(t *testing.T) {
	opts, d, f, _ := setup(t)
	opts.User = "alice"
	f.labels["nixzner-user"] = "mp"
	_, err := Run(context.Background(), opts, d)
	if err == nil || !strings.Contains(err.Error(), "changing the user is not supported") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"find web1"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestRunActivationHintUsesLoginUser(t *testing.T) {
	opts, d, f, stderr := setup(t)
	opts.Flake = config.FlakeRef{URI: "path:" + t.TempDir(), Host: "web1"}
	f.labels["nixzner-user"] = "mp"
	f.fail["activate"] = errors.New("unit failed")
	if _, err := Run(context.Background(), opts, d); err == nil {
		t.Fatal("want error")
	}
	if want := "ssh mp@203.0.113.10 sudo /nix/var/nix/profiles/system/bin/switch-to-configuration test"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not mention %q", stderr.String(), want)
	}
}

func TestRunValidatesUser(t *testing.T) {
	opts, d, f, _ := setup(t)
	opts.User = "root"
	if _, err := Run(context.Background(), opts, d); err == nil {
		t.Fatal("want error")
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %v, want none", f.calls)
	}
}
```

In `TestRunRebuildsInOrder`, add: `if f.evalFlake != opts.Flake { t.Errorf("flake wrapped without a user: %+v", f.evalFlake) }` and `if _, ok := f.setLabels["nixzner-user"]; ok { t.Errorf("nixzner-user set without a user") }`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/rebuild/`
Expected: build failure `opts.User undefined`.

- [ ] **Step 3: Implement** — in `internal/rebuild/rebuild.go`:

Add to `Options` after `Identity`:

```go
	User     string // login user to set up; "" means the server's nixzner-user label, if any
```

In `validate`, after the name check:

```go
	if o.User != "" {
		if err := config.ValidateUser(o.User); err != nil {
			errs = append(errs, err)
		}
	}
```

In `Run`, add to the `var (...)` block: `flake config.FlakeRef`, `target string`, and before the var block `cleanupWrap := func() {}` with `defer func() { cleanupWrap() }()` right after it.

Replace the preflight step body after the managed-by check with:

```go
			labelUser := l[config.LabelUser]
			if opts.User != "" && labelUser != "" && opts.User != labelUser {
				return nil, fmt.Errorf("server %q was set up with --user %s; changing the user is not supported", opts.Name, labelUser)
			}
			target = cmp.Or(opts.User, labelUser)
			res.Server, labels = srv, l
			remote = nixos.Remote{Host: srv.IPv4, User: labelUser, Identity: opts.Identity}
			progress("    server %s (id %d) at %s", srv.Name, srv.ID, srv.IPv4)
			var cleanup func()
			if flake, cleanup, err = flakewrap.Wrap(opts.Flake, target); err != nil {
				return nil, err
			}
			cleanupWrap = cleanup
			res.StorePath, err = d.Nix.Eval(ctx, flake)
			return nil, err
```

In the build step use `d.Nix.BuildRemote(ctx, flake, remote, out)`. In the activate step replace the `state` assignment with:

```go
			login, sudo := "root", ""
			if remote.User != "" {
				login, sudo = remote.User, "sudo "
			}
			state = fmt.Sprintf("the previous system is still the boot default; to return to it now, run:\n    ssh %s@%s %s/nix/var/nix/profiles/system/bin/switch-to-configuration test", login, remote.Host, sudo)
```

In verify use `d.SSH.VerifyNixOS(ctx, target, remote.Host, res.StorePath)`. In finalize, after setting the flake label:

```go
			if target != "" {
				l[config.LabelUser] = target
			}
```

Imports: add `"cmp"` and `"nixzner/internal/flakewrap"`.

- [ ] **Step 4: Run tests**

Run: `gofmt -l internal && go vet ./internal/rebuild && go test ./internal/rebuild/`
Expected: `ok` (including the existing `TestRunActivationFailureExplainsRollback`, whose root hint is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/rebuild
git commit -m "feat(rebuild): --user, defaulting to the server's nixzner-user label

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: CLI flags, integration test option, README

**Files:**
- Modify: `cmd/nixzner/main.go` (`newCreateCmd`, `newRebuildCmd`)
- Modify: `cmd/nixzner/main_test.go`
- Modify: `cmd/nixzner/integration_test.go:57-65`
- Modify: `README.md`

**Interfaces:**
- Consumes: `config.CreateOptions.User`, `rebuild.Options.User`, `config.ValidateUser`.

- [ ] **Step 1: Write the failing tests** — append to `cmd/nixzner/main_test.go`:

```go
func TestCreateRejectsRootUser(t *testing.T) {
	args := append(append([]string{}, fullArgs...), "--user", "root")
	code, _, stderr := runCLI(map[string]string{"HCLOUD_TOKEN": "x"}, args...)
	if code != 1 || !strings.Contains(stderr, `--user must not be "root"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestRebuildRejectsInvalidUser(t *testing.T) {
	code, _, stderr := runCLI(map[string]string{"HCLOUD_TOKEN": "x"}, "rebuild", "web1", "--flake", ".#web1", "--user", "Bad User")
	if code != 1 || !strings.Contains(stderr, "must be a Linux user name") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/nixzner/ -run 'User'`
Expected: FAIL — `unknown flag: --user`.

- [ ] **Step 3: Implement** — in `cmd/nixzner/main.go`:

In `newCreateCmd` flags:

```go
	f.StringVar(&opts.User, "user", "", "create this login user with the flake's root SSH keys, and disable root SSH login")
```

and in its `RunE`, immediately after the label parsing succeeds:

```go
			if opts.User != "" {
				if err := config.ValidateUser(opts.User); err != nil {
					return err
				}
			}
```

In `newRebuildCmd` flags:

```go
	f.StringVar(&opts.User, "user", "", "login user (default: the server's nixzner-user label); on a root-only server, sets it up and disables root SSH login")
```

and in its `RunE`, immediately after `ResolveLocalFlake` succeeds, the same `ValidateUser` block.

In `cmd/nixzner/integration_test.go`, replace the inline args of the `run(...)` call with a slice:

```go
	args := []string{
		"create", name,
		"--flake", "path:" + flakeDir + "#" + host,
		"--type", serverType,
		"--location", location,
		"--ssh-key", sshKey,
		"--log-dir", t.TempDir(),
	}
	if u := os.Getenv("NIXZNER_TEST_USER"); u != "" {
		args = append(args, "--user", u)
	}
	code := run(context.Background(), args, &stdout, os.Stderr, os.Getenv, &created, nil)
```

In `README.md`, after the `nixzner rebuild` section's first paragraph, add:

```markdown
### A login user instead of root: `--user`

`nixzner create web1 ... --user mp` installs your flake plus a small module
that creates `mp` with the SSH keys your flake gives root, lets `mp` use
`sudo` and nix without a password, and sets `PermitRootLogin = "no"`. Your
flake is not edited; nixzner adds the module for the run. nixzner verifies
the install by logging in as `mp` and labels the server `nixzner-user=mp`.

`nixzner rebuild` reads that label, so later rebuilds log in as `mp` and
keep the module without repeating `--user`. `rebuild web1 --user mp` on a
server created without `--user` converts it. Changing to a different user
is refused. If neither root nor the user would get an SSH key, the build
fails before anything changes.
```

Add to the README Development section's override list: `` `NIXZNER_TEST_USER` (runs create with `--user`) ``.

- [ ] **Step 4: Run tests**

Run: `gofmt -l . && go vet ./... && go vet -tags integration ./cmd/nixzner && go test ./...`
Expected: all `ok`.

- [ ] **Step 5: Commit**

```bash
git add cmd README.md
git commit -m "feat(cli): --user on create and rebuild

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Real verification on `mithril`

**Files:** none (manual verification; record results in the final report).

- [ ] **Step 1: Build**

```bash
S=/private/tmp/claude-501/-Users-hugocantacuzene-Code-nixzner/7f0f63c3-19a1-43e9-b8ef-a450d4cafd9a/scratchpad
go build -o $S/nixzner ./cmd/nixzner
```

- [ ] **Step 2: Convert mithril** (its flake already defines `mp` and disables root login; root still works on the running system)

```bash
cd ~/Code/mithril && export PATH="$HOME/.nix-profile/bin:/nix/var/nix/profiles/default/bin:$PATH"
HCLOUD_TOKEN=$(tr -d '[:space:]' < ~/.config/nixzner/token) $S/nixzner rebuild mithril --flake .#mithril --user mp --log-dir $S/nixzner-logs
```

Expected: steps `preflight`, `build`, `activate`, `make boot default`, `verify`, `finalize`; exit 0.

- [ ] **Step 3: Check the result**

```bash
ssh -o BatchMode=yes mp@2.28.96.76 'whoami; sudo -n true && echo sudo-ok; readlink /run/current-system'
ssh -o BatchMode=yes -o ConnectTimeout=10 root@2.28.96.76 true; echo "root exit=$?"
T=$(tr -d '[:space:]' < ~/.config/nixzner/token); curl -sS -H "Authorization: Bearer $T" 'https://api.hetzner.cloud/v1/servers?name=mithril' | jq -c '.servers[0].labels'
```

Expected: `mp`, `sudo-ok`, the store path printed by step 2; root refused (`Permission denied`, exit 255); labels include `"nixzner-user":"mp"`.

- [ ] **Step 4: Rebuild again without `--user`** (label default)

```bash
cd ~/Code/mithril && HCLOUD_TOKEN=$(tr -d '[:space:]' < ~/.config/nixzner/token) $S/nixzner rebuild mithril --flake .#mithril --log-dir $S/nixzner-logs
```

Expected: exit 0 (logs in as `mp`).
