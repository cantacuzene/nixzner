# hinfect `create` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `hinfect create`, a Go CLI that creates a Hetzner Cloud server, installs NixOS on it from a flake via nixos-anywhere, verifies the first boot, and deletes the server on any failure.

**Architecture:** A cobra CLI (`cmd/hinfect`) parses flags into `config.CreateOptions` and hands them to `create.Run`, which drives a `pipeline.Pipeline` of steps (preflight → create server → wait for SSH → install → verify → finalize). Steps call three narrow interfaces — `hetzner` (hcloud-go), `nixos` (nix/nixos-anywhere subprocesses), `sshcheck` (x/crypto/ssh) — so the orchestration is unit-tested with fakes. Cleanups registered by steps run in reverse order under a fresh context on failure or interrupt.

**Tech Stack:** Go ≥ 1.23, `github.com/hetznercloud/hcloud-go/v2`, `github.com/spf13/cobra`, `golang.org/x/crypto/ssh` (+ `ssh/agent`), `os/exec`.

**Spec:** `docs/superpowers/specs/2026-09-22-hinfect-create-design.md`

## Global Constraints

- Go module path: `hinfect`. Minimum Go 1.23 (uses `os.CopyFS`, `context.AfterFunc`, builtin `min`).
- Binary name: `hinfect`. Only command in v1: `create`.
- Hetzner token comes only from env `HCLOUD_TOKEN`.
- Labels always applied: `managed-by=hinfect`, `hinfect-flake=<sanitized flake>`, `hinfect-status=installing|ready`. User label keys `managed-by` or starting with `hinfect-` are rejected.
- Hetzner label rule: name/value ≤ 63 chars, `[a-zA-Z0-9._-]`, alphanumeric at both ends (value may be empty); key may have a DNS-subdomain prefix `prefix/name`.
- nixos-anywhere is always invoked with `--build-on remote` and `--ssh-option StrictHostKeyChecking=no --ssh-option UserKnownHostsFile=/dev/null`.
- Exit codes: `0` success, `1` failed (nothing left behind), `2` failed and cleanup failed.
- On success stdout gets exactly one line: `<name> <ipv4> <ipv6> <store-path>`. All progress goes to stderr.
- Failure logs saved as `<log-dir>/<name>-<UTC 20060102T150405Z>.log`, mode `0600`.
- Timeouts: overall `--timeout` default 30m; SSH wait 5m; verify 5m; cleanup budget 2m on a fresh context.
- Commit messages end with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.

### Deliberate refinements of the spec (read before starting)

1. `pipeline.Step` is `{Name, Run func(ctx) (Cleanup, error)}` — the cleanup is returned by `Run` (it needs the server ID only known after running), not declared up front.
2. `nixos.Eval` evaluates only `toplevel.outPath` (evaluating it forces the same evaluation as `drvPath`; drvPath was unused).
3. Verification polls: after install the kexec installer may still answer SSH (and has `/etc/NIXOS`), so `VerifyNixOS` retries until `/run/current-system` equals the expected path or its timeout expires.
4. Architecture comes from the Hetzner server type's `architecture` field rather than a `cax*` name heuristic.
5. `nixos` tests use an injected fake `Runner` instead of the `TestHelperProcess` pattern; one small real-exec test covers `ExecRunner`.
6. The integration test reuses `examples/flake/` (copied to a temp dir) instead of a separate `testdata/flake/`.
7. IPv6 is reported as the `::1` address of the server's /64.

---

## File Structure

```
go.mod, go.sum
.gitignore
README.md
cmd/hinfect/main.go              cobra root + create command, signal handling, exit codes
cmd/hinfect/main_test.go         flag/env error paths (no network)
cmd/hinfect/integration_test.go  //go:build integration — real Hetzner run
internal/config/flake.go         FlakeRef, ParseFlake, ToplevelAttr
internal/config/labels.go        label validation, sanitizing, ParseLabels, SystemLabels
internal/config/options.go       CreateOptions + Validate
internal/config/*_test.go
internal/pipeline/pipeline.go    Step, Cleanup, Pipeline.Run/Cleanup
internal/pipeline/pipeline_test.go
internal/nixos/nixos.go          Runner, ExecRunner, Nix.Eval/Install, InstallCommand, CheckTools
internal/nixos/nixos_test.go
internal/sshcheck/sshcheck.go    Checker.WaitForSSH/VerifyNixOS, AuthMethods
internal/sshcheck/server_test.go in-process SSH test server
internal/sshcheck/sshcheck_test.go
internal/hetzner/hetzner.go      Client over hcloud-go: Preflight/CreateServer/DeleteServer/SetLabels
internal/hetzner/hetzner_test.go httptest fake Hetzner API
internal/create/create.go        Deps, Run, ErrCleanupFailed, persistLog
internal/create/create_test.go   orchestration tests with fakes
examples/flake/{flake.nix,configuration.nix,disk-config.nix,authorized_keys}
```

---

### Task 1: Project scaffold and `config` package

**Files:**
- Create: `go.mod`, `.gitignore`
- Create: `internal/config/flake.go`, `internal/config/labels.go`, `internal/config/options.go`
- Test: `internal/config/flake_test.go`, `internal/config/labels_test.go`, `internal/config/options_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type FlakeRef struct { URI, Host string }`; `func ParseFlake(s string) (FlakeRef, error)`; `func (f FlakeRef) String() string`; `func (f FlakeRef) ToplevelAttr() string`
  - Constants `LabelManagedBy="managed-by"`, `ManagedByValue="hinfect"`, `LabelFlake="hinfect-flake"`, `LabelStatus="hinfect-status"`, `StatusInstalling="installing"`, `StatusReady="ready"`
  - `func ValidLabelKey(string) bool`, `func ValidLabelValue(string) bool`, `func SanitizeLabelValue(string) string`, `func ParseLabels([]string) (map[string]string, error)`, `func SystemLabels(user map[string]string, flake FlakeRef, status string) map[string]string`
  - `type CreateOptions struct { Name string; Flake FlakeRef; ServerType, Location, SSHKey, Identity, Image string; Firewalls, Networks []string; Labels map[string]string; LogDir string; Timeout time.Duration }`; `func (o CreateOptions) Validate() error`

- [ ] **Step 1: Install toolchain and init module**

Go is not installed on the dev machine yet.

```bash
brew install go
go version   # expect go1.23 or newer
cd /Users/hugocantacuzene/Code/hetznerInfector
go mod init hinfect
```

Create `.gitignore`:

```gitignore
/hinfect
/hinfect-logs/
```

- [ ] **Step 2: Write failing tests for flake parsing**

`internal/config/flake_test.go`:

```go
package config

import "testing"

func TestParseFlake(t *testing.T) {
	cases := []struct {
		in      string
		want    FlakeRef
		wantErr bool
	}{
		{in: ".#web1", want: FlakeRef{URI: ".", Host: "web1"}},
		{in: "github:org/infra#db-1", want: FlakeRef{URI: "github:org/infra", Host: "db-1"}},
		{in: "path:/tmp/x#example", want: FlakeRef{URI: "path:/tmp/x", Host: "example"}},
		{in: ".", wantErr: true},
		{in: "#web1", wantErr: true},
		{in: ".#", wantErr: true},
		{in: `.#we"b`, wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseFlake(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseFlake(%q): want error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFlake(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseFlake(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if got.String() != c.in {
			t.Errorf("String() = %q, want %q", got.String(), c.in)
		}
	}
}

func TestToplevelAttr(t *testing.T) {
	cases := map[string]string{
		".#web1":             ".#nixosConfigurations.web1.config.system.build.toplevel",
		".#web1.example.com": `.#nixosConfigurations."web1.example.com".config.system.build.toplevel`,
	}
	for in, want := range cases {
		f, err := ParseFlake(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.ToplevelAttr(); got != want {
			t.Errorf("ToplevelAttr(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: ParseFlake` / `undefined: FlakeRef`.

- [ ] **Step 4: Implement `flake.go`**

```go
package config

import (
	"fmt"
	"strings"
)

// FlakeRef is a flake reference of the form <uri>#<host>, e.g. ".#web1",
// where <host> names an entry in the flake's nixosConfigurations.
type FlakeRef struct {
	URI  string
	Host string
}

func ParseFlake(s string) (FlakeRef, error) {
	uri, host, ok := strings.Cut(s, "#")
	if !ok || uri == "" || host == "" {
		return FlakeRef{}, fmt.Errorf("flake %q must have the form <uri>#<host>, e.g. .#web1", s)
	}
	if strings.Contains(host, `"`) {
		return FlakeRef{}, fmt.Errorf("flake host %q must not contain quotes", host)
	}
	return FlakeRef{URI: uri, Host: host}, nil
}

func (f FlakeRef) String() string { return f.URI + "#" + f.Host }

// ToplevelAttr is the installable for the host's system closure.
func (f FlakeRef) ToplevelAttr() string {
	host := f.Host
	if strings.Contains(host, ".") {
		host = `"` + host + `"`
	}
	return f.URI + "#nixosConfigurations." + host + ".config.system.build.toplevel"
}
```

- [ ] **Step 5: Write failing tests for labels**

`internal/config/labels_test.go`:

```go
package config

import (
	"maps"
	"strings"
	"testing"
)

func TestValidLabelKey(t *testing.T) {
	cases := map[string]bool{
		"env":                   true,
		"example.com/team":      true,
		"a.b_c-d":               true,
		"":                      false,
		"-bad":                  false,
		"bad-":                  false,
		"has space":             false,
		"a/b/c":                 false,
		"UPPER.com/x":           false,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
	}
	for k, want := range cases {
		if got := ValidLabelKey(k); got != want {
			t.Errorf("ValidLabelKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestValidLabelValue(t *testing.T) {
	cases := map[string]bool{"": true, "prod": true, "v1.2_3-x": true, "has space": false, "-x": false, "a/b": false}
	for v, want := range cases {
		if got := ValidLabelValue(v); got != want {
			t.Errorf("ValidLabelValue(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := map[string]string{
		".#web1":                "web1",
		"github:org/infra#db-1": "github-org-infra-db-1",
		"path:/tmp/x#example":   "path-tmp-x-example",
	}
	for in, want := range cases {
		if got := SanitizeLabelValue(in); got != want {
			t.Errorf("SanitizeLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeLabelValue(strings.Repeat("a", 62) + "-#x")
	if len(long) > 63 || !ValidLabelValue(long) {
		t.Errorf("long value %q is not a valid label value", long)
	}
}

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels([]string{"env=prod", "example.com/team=infra", "empty="})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "prod", "example.com/team": "infra", "empty": ""}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	bad := map[string]string{
		"noequals":         "key=value",
		"bad key=x":        "invalid label key",
		"env=bad value":    "invalid label value",
		"managed-by=me":    "reserved",
		"hinfect-status=x": "reserved",
	}
	for in, wantMsg := range bad {
		_, err := ParseLabels([]string{in})
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("ParseLabels(%q) error = %v, want containing %q", in, err, wantMsg)
		}
	}
}

func TestSystemLabels(t *testing.T) {
	user := map[string]string{"env": "prod"}
	flake, _ := ParseFlake(".#web1")
	got := SystemLabels(user, flake, StatusInstalling)
	want := map[string]string{
		"env":            "prod",
		"managed-by":     "hinfect",
		"hinfect-flake":  "web1",
		"hinfect-status": "installing",
	}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if len(user) != 1 {
		t.Errorf("SystemLabels mutated its input: %v", user)
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: ValidLabelKey` etc.

- [ ] **Step 7: Implement `labels.go`**

```go
package config

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
)

const (
	LabelManagedBy   = "managed-by"
	ManagedByValue   = "hinfect"
	LabelFlake       = "hinfect-flake"
	LabelStatus      = "hinfect-status"
	StatusInstalling = "installing"
	StatusReady      = "ready"
)

var (
	labelNameRe       = regexp.MustCompile(`^[a-zA-Z0-9]([-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?$`)
	labelPrefixRe     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	invalidValueChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
)

// ValidLabelKey reports whether k is a valid Hetzner label key: a name,
// optionally preceded by a DNS-subdomain prefix and "/".
func ValidLabelKey(k string) bool {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		return labelNameRe.MatchString(k)
	}
	return len(prefix) <= 253 && labelPrefixRe.MatchString(prefix) && labelNameRe.MatchString(name)
}

func ValidLabelValue(v string) bool { return v == "" || labelNameRe.MatchString(v) }

// SanitizeLabelValue turns an arbitrary string into a valid label value.
func SanitizeLabelValue(s string) string {
	s = invalidValueChars.ReplaceAllString(s, "-")
	s = strings.TrimFunc(s, notAlnum)
	if len(s) > 63 {
		s = strings.TrimFunc(s[:63], notAlnum)
	}
	return s
}

func notAlnum(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
}

// ParseLabels parses repeated --label key=value flags.
func ParseLabels(pairs []string) (map[string]string, error) {
	labels := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("label %q must have the form key=value", p)
		case !ValidLabelKey(k):
			return nil, fmt.Errorf("invalid label key %q", k)
		case !ValidLabelValue(v):
			return nil, fmt.Errorf("invalid label value %q for key %q", v, k)
		case k == LabelManagedBy || strings.HasPrefix(k, "hinfect-"):
			return nil, fmt.Errorf("label key %q is reserved for hinfect", k)
		}
		labels[k] = v
	}
	return labels, nil
}

// SystemLabels returns a copy of user with the labels hinfect always sets.
func SystemLabels(user map[string]string, flake FlakeRef, status string) map[string]string {
	labels := maps.Clone(user)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManagedBy] = ManagedByValue
	labels[LabelFlake] = SanitizeLabelValue(flake.String())
	labels[LabelStatus] = status
	return labels
}
```

- [ ] **Step 8: Write failing tests for options**

`internal/config/options_test.go`:

```go
package config

import (
	"strings"
	"testing"
	"time"
)

func validOpts() CreateOptions {
	return CreateOptions{
		Name:       "web1",
		Flake:      FlakeRef{URI: ".", Host: "web1"},
		ServerType: "cx32",
		Location:   "fsn1",
		SSHKey:     "admin",
		Image:      "debian-12",
		LogDir:     "./hinfect-logs",
		Timeout:    30 * time.Minute,
	}
}

func TestValidateAcceptsValidOptions(t *testing.T) {
	if err := validOpts().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*CreateOptions)
		wantMsg string
	}{
		"uppercase name":   {func(o *CreateOptions) { o.Name = "Web1" }, "server name"},
		"empty name":       {func(o *CreateOptions) { o.Name = "" }, "server name"},
		"missing flake":    {func(o *CreateOptions) { o.Flake = FlakeRef{} }, "--flake"},
		"missing type":     {func(o *CreateOptions) { o.ServerType = "" }, "--type"},
		"missing location": {func(o *CreateOptions) { o.Location = "" }, "--location"},
		"missing ssh key":  {func(o *CreateOptions) { o.SSHKey = "" }, "--ssh-key"},
		"missing image":    {func(o *CreateOptions) { o.Image = "" }, "--image"},
		"missing log dir":  {func(o *CreateOptions) { o.LogDir = "" }, "--log-dir"},
		"zero timeout":     {func(o *CreateOptions) { o.Timeout = 0 }, "--timeout"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := validOpts()
			c.mutate(&o)
			err := o.Validate()
			if err == nil || !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantMsg)
			}
		})
	}
}
```

- [ ] **Step 9: Run to verify failure**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: CreateOptions`.

- [ ] **Step 10: Implement `options.go`**

```go
package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// CreateOptions is everything `hinfect create` needs, already parsed.
type CreateOptions struct {
	Name       string
	Flake      FlakeRef
	ServerType string
	Location   string
	SSHKey     string
	Identity   string // private key file; empty means use ssh-agent
	Image      string
	Firewalls  []string
	Networks   []string
	Labels     map[string]string
	LogDir     string
	Timeout    time.Duration
}

var serverNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate checks the options without any I/O and reports every problem.
func (o CreateOptions) Validate() error {
	var errs []error
	if !serverNameRe.MatchString(o.Name) {
		errs = append(errs, fmt.Errorf("server name %q must be a lowercase hostname label (a-z, 0-9, -; at most 63 chars)", o.Name))
	}
	if o.Flake.Host == "" {
		errs = append(errs, errors.New("--flake is required"))
	}
	for flag, v := range map[string]string{
		"--type": o.ServerType, "--location": o.Location, "--ssh-key": o.SSHKey,
		"--image": o.Image, "--log-dir": o.LogDir,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", flag))
		}
	}
	if o.Timeout <= 0 {
		errs = append(errs, errors.New("--timeout must be positive"))
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 11: Run tests**

Run: `go test ./internal/config/ && go vet ./...`
Expected: PASS, no vet output.

- [ ] **Step 12: Commit**

```bash
git add go.mod .gitignore internal/config
git commit -m "feat(config): flake refs, label rules, create options

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: `pipeline` package

**Files:**
- Create: `internal/pipeline/pipeline.go`
- Test: `internal/pipeline/pipeline_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Cleanup func(ctx context.Context) error`
  - `type Step struct { Name string; Run func(ctx context.Context) (Cleanup, error) }`
  - `type Pipeline struct { Progress func(format string, args ...any) }` with `func (p *Pipeline) Run(ctx context.Context, steps []Step) error` and `func (p *Pipeline) Cleanup(ctx context.Context) error`
  - Error from `Run` is `fmt.Errorf("%s: %w", step.Name, err)` (so `errors.Is` works on the original).

- [ ] **Step 1: Write failing tests**

`internal/pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type recorder struct{ events []string }

func (r *recorder) step(name string, runErr error, withCleanup bool, cleanupErr error) Step {
	return Step{Name: name, Run: func(ctx context.Context) (Cleanup, error) {
		r.events = append(r.events, "run "+name)
		if !withCleanup {
			return nil, runErr
		}
		return func(ctx context.Context) error {
			r.events = append(r.events, "undo "+name)
			return cleanupErr
		}, runErr
	}}
}

func TestRunExecutesStepsInOrder(t *testing.T) {
	r := &recorder{}
	p := &Pipeline{}
	err := p.Run(context.Background(), []Step{r.step("a", nil, true, nil), r.step("b", nil, false, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"run a", "run b"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestRunStopsAtFailingStepAndWrapsError(t *testing.T) {
	boom := errors.New("boom")
	r := &recorder{}
	p := &Pipeline{}
	err := p.Run(context.Background(), []Step{r.step("a", nil, true, nil), r.step("b", boom, false, nil), r.step("c", nil, false, nil)})
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "b: ") {
		t.Fatalf("err = %v, want wrapped boom prefixed with step name", err)
	}
	if want := []string{"run a", "run b"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestCleanupRunsInReverseIncludingFailingStep(t *testing.T) {
	r := &recorder{}
	p := &Pipeline{}
	_ = p.Run(context.Background(), []Step{
		r.step("a", nil, true, nil),
		r.step("b", nil, false, nil),
		r.step("c", errors.New("half done"), true, nil),
	})
	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"run a", "run b", "run c", "undo c", "undo a"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestCleanupContinuesPastErrorsAndJoinsThem(t *testing.T) {
	e1, e2 := errors.New("e1"), errors.New("e2")
	r := &recorder{}
	p := &Pipeline{}
	_ = p.Run(context.Background(), []Step{r.step("a", nil, true, e1), r.step("b", nil, true, e2)})
	err := p.Cleanup(context.Background())
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("err = %v, want both e1 and e2", err)
	}
	if want := []string{"run a", "run b", "undo b", "undo a"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestRunStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{}
	cancelling := Step{Name: "cancel", Run: func(context.Context) (Cleanup, error) {
		r.events = append(r.events, "run cancel")
		cancel()
		return nil, nil
	}}
	p := &Pipeline{}
	err := p.Run(ctx, []Step{cancelling, r.step("never", nil, false, nil)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if slices.Contains(r.events, "run never") {
		t.Fatal("step after cancellation ran")
	}
}

func TestProgressIsReported(t *testing.T) {
	var lines []string
	p := &Pipeline{Progress: func(f string, a ...any) { lines = append(lines, f) }}
	r := &recorder{}
	_ = p.Run(context.Background(), []Step{r.step("a", nil, true, nil)})
	_ = p.Cleanup(context.Background())
	if len(lines) != 2 {
		t.Fatalf("progress lines = %v, want 2", lines)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/pipeline/`
Expected: FAIL — `undefined: Step`.

- [ ] **Step 3: Implement**

`internal/pipeline/pipeline.go`:

```go
// Package pipeline runs ordered steps and undoes them in reverse on failure.
package pipeline

import (
	"context"
	"errors"
	"fmt"
)

// Cleanup undoes a step's effect. It receives a context independent of the
// one the step ran under, so it still works after cancellation.
type Cleanup func(ctx context.Context) error

type Step struct {
	Name string
	// Run performs the step. A non-nil Cleanup is registered even when Run
	// also returns an error (e.g. the server was created but never started).
	Run func(ctx context.Context) (Cleanup, error)
}

type Pipeline struct {
	Progress func(format string, args ...any)

	cleanups []namedCleanup
}

type namedCleanup struct {
	name string
	fn   Cleanup
}

// Run executes steps in order and stops at the first error or cancellation.
// It does not clean up; call Cleanup for that.
func (p *Pipeline) Run(ctx context.Context, steps []Step) error {
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("before %s: %w", s.Name, err)
		}
		p.progress("==> %s", s.Name)
		cleanup, err := s.Run(ctx)
		if cleanup != nil {
			p.cleanups = append(p.cleanups, namedCleanup{s.Name, cleanup})
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	return nil
}

// Cleanup runs registered cleanups in reverse order, continuing past
// failures, and returns every failure joined.
func (p *Pipeline) Cleanup(ctx context.Context) error {
	var errs []error
	for i := len(p.cleanups) - 1; i >= 0; i-- {
		c := p.cleanups[i]
		p.progress("<== undo %s", c.name)
		if err := c.fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("undo %s: %w", c.name, err))
		}
	}
	p.cleanups = nil
	return errors.Join(errs...)
}

func (p *Pipeline) progress(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(format, args...)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/pipeline/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline
git commit -m "feat(pipeline): ordered steps with reverse cleanup

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: `nixos` package

**Files:**
- Create: `internal/nixos/nixos.go`
- Test: `internal/nixos/nixos_test.go`

**Interfaces:**
- Consumes: `config.FlakeRef` (`String()`, `ToplevelAttr()`).
- Produces:
  - `type Runner interface { Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error }`; `type ExecRunner struct{}`
  - `type Nix struct { Runner Runner }` with `func (n Nix) Eval(ctx context.Context, f config.FlakeRef) (outPath string, err error)` and `func (n Nix) Install(ctx context.Context, a InstallArgs, log io.Writer) error`
  - `type InstallArgs struct { Flake config.FlakeRef; Host string; Identity string }`; `func InstallCommand(a InstallArgs) (name string, args []string)`
  - `func CheckTools(lookPath func(string) (string, error)) error`

- [ ] **Step 1: Write failing tests**

`internal/nixos/nixos_test.go`:

```go
package nixos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"hinfect/internal/config"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls  []call
	stdout string
	stderr string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, call{name, args})
	io.WriteString(stdout, f.stdout)
	io.WriteString(stderr, f.stderr)
	return f.err
}

func flake(t *testing.T, s string) config.FlakeRef {
	t.Helper()
	f, err := config.ParseFlake(s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestEvalReturnsOutPath(t *testing.T) {
	r := &fakeRunner{stdout: "/nix/store/abc-nixos-system-web1\n"}
	got, err := Nix{Runner: r}.Eval(context.Background(), flake(t, ".#web1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/nix/store/abc-nixos-system-web1" {
		t.Fatalf("outPath = %q", got)
	}
	want := call{"nix", []string{
		"--extra-experimental-features", "nix-command flakes",
		"eval", "--raw", ".#nixosConfigurations.web1.config.system.build.toplevel.outPath",
	}}
	if len(r.calls) != 1 || r.calls[0].name != want.name || !slices.Equal(r.calls[0].args, want.args) {
		t.Fatalf("calls = %+v, want %+v", r.calls, want)
	}
}

func TestEvalIncludesNixStderrOnFailure(t *testing.T) {
	r := &fakeRunner{stderr: "error: attribute 'web1' missing", err: errors.New("exit status 1")}
	_, err := Nix{Runner: r}.Eval(context.Background(), flake(t, ".#web1"))
	if err == nil || !strings.Contains(err.Error(), "attribute 'web1' missing") {
		t.Fatalf("err = %v, want nix stderr included", err)
	}
}

func TestEvalRejectsNonStorePath(t *testing.T) {
	r := &fakeRunner{stdout: "not-a-path"}
	if _, err := (Nix{Runner: r}).Eval(context.Background(), flake(t, ".#web1")); err == nil {
		t.Fatal("want error for non-store-path output")
	}
}

func TestInstallCommand(t *testing.T) {
	name, args := InstallCommand(InstallArgs{Flake: flake(t, ".#web1"), Host: "203.0.113.10", Identity: "/home/me/.ssh/id_ed25519"})
	want := []string{
		"--flake", ".#web1",
		"--build-on", "remote",
		"--target-host", "root@203.0.113.10",
		"--ssh-option", "StrictHostKeyChecking=no",
		"--ssh-option", "UserKnownHostsFile=/dev/null",
		"-i", "/home/me/.ssh/id_ed25519",
	}
	if name != "nixos-anywhere" || !slices.Equal(args, want) {
		t.Fatalf("got %s %q\nwant nixos-anywhere %q", name, args, want)
	}

	_, args = InstallCommand(InstallArgs{Flake: flake(t, ".#web1"), Host: "203.0.113.10"})
	if slices.Contains(args, "-i") {
		t.Fatalf("args %q contain -i without identity", args)
	}
}

func TestInstallStreamsOutputToLog(t *testing.T) {
	r := &fakeRunner{stdout: "copying...\n", stderr: "warning\n"}
	var log bytes.Buffer
	err := Nix{Runner: r}.Install(context.Background(), InstallArgs{Flake: flake(t, ".#web1"), Host: "h"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if log.String() != "copying...\nwarning\n" {
		t.Fatalf("log = %q", log.String())
	}
}

func TestInstallWrapsError(t *testing.T) {
	boom := errors.New("exit status 1")
	r := &fakeRunner{err: boom}
	err := Nix{Runner: r}.Install(context.Background(), InstallArgs{Flake: flake(t, ".#web1"), Host: "h"}, io.Discard)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "nixos-anywhere") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckTools(t *testing.T) {
	missing := func(name string) (string, error) {
		if name == "nixos-anywhere" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	err := CheckTools(missing)
	if err == nil || !strings.Contains(err.Error(), "nixos-anywhere not found on PATH") {
		t.Fatalf("err = %v", err)
	}
	all := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	if err := CheckTools(all); err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerRunsRealCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	err := ExecRunner{}.Run(context.Background(), "sh", []string{"-c", "echo out; echo err >&2"}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/nixos/`
Expected: FAIL — `undefined: Nix`.

- [ ] **Step 3: Implement**

`internal/nixos/nixos.go`:

```go
// Package nixos runs nix and nixos-anywhere as subprocesses.
package nixos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"hinfect/internal/config"
)

type Runner interface {
	Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

// ExecRunner runs real processes. On cancellation it sends SIGINT so
// nixos-anywhere can tidy up, and kills the process 10s later.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

var nixFlags = []string{"--extra-experimental-features", "nix-command flakes"}

type Nix struct {
	Runner Runner
}

// Eval returns the store path the host's system closure will have. It only
// evaluates, so it works on machines that cannot build Linux derivations.
func (n Nix) Eval(ctx context.Context, f config.FlakeRef) (string, error) {
	attr := f.ToplevelAttr() + ".outPath"
	var out, errOut bytes.Buffer
	args := append(append([]string{}, nixFlags...), "eval", "--raw", attr)
	if err := n.Runner.Run(ctx, "nix", args, &out, &errOut); err != nil {
		return "", fmt.Errorf("nix eval %s: %w\n%s", attr, err, strings.TrimSpace(errOut.String()))
	}
	path := strings.TrimSpace(out.String())
	if !strings.HasPrefix(path, "/nix/store/") {
		return "", fmt.Errorf("nix eval %s: unexpected output %q", attr, path)
	}
	return path, nil
}

type InstallArgs struct {
	Flake    config.FlakeRef
	Host     string // IPv4 address of the target
	Identity string // optional private key file
}

func InstallCommand(a InstallArgs) (string, []string) {
	args := []string{
		"--flake", a.Flake.String(),
		"--build-on", "remote",
		"--target-host", "root@" + a.Host,
		// The target is minutes old and its host key changes after kexec.
		"--ssh-option", "StrictHostKeyChecking=no",
		"--ssh-option", "UserKnownHostsFile=/dev/null",
	}
	if a.Identity != "" {
		args = append(args, "-i", a.Identity)
	}
	return "nixos-anywhere", args
}

// Install runs nixos-anywhere, sending both of its output streams to log.
func (n Nix) Install(ctx context.Context, a InstallArgs, log io.Writer) error {
	name, args := InstallCommand(a)
	if err := n.Runner.Run(ctx, name, args, log, log); err != nil {
		return fmt.Errorf("nixos-anywhere: %w", err)
	}
	return nil
}

// CheckTools verifies that nix and nixos-anywhere are installed.
func CheckTools(lookPath func(string) (string, error)) error {
	var errs []error
	for _, tool := range []string{"nix", "nixos-anywhere"} {
		if _, err := lookPath(tool); err != nil {
			errs = append(errs, fmt.Errorf("%s not found on PATH", tool))
		}
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/nixos/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/nixos
git commit -m "feat(nixos): nix eval and nixos-anywhere wrapper

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: `sshcheck` package

**Files:**
- Create: `internal/sshcheck/sshcheck.go`
- Test: `internal/sshcheck/server_test.go` (test SSH server helper), `internal/sshcheck/sshcheck_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Backoff struct { Initial, Max time.Duration }`; `var DefaultBackoff = Backoff{Initial: 2 * time.Second, Max: 15 * time.Second}`
  - `type Checker struct { Auth []ssh.AuthMethod; Port int; Backoff Backoff; Progress func(format string, args ...any) }`
  - `func (c Checker) WaitForSSH(ctx context.Context, host string) error`
  - `func (c Checker) VerifyNixOS(ctx context.Context, host, wantOutPath string) error`
  - `func AuthMethods(identity, agentSock string) ([]ssh.AuthMethod, func() error, error)`
  - Both check methods poll until success or `ctx` ends; callers bound them with `context.WithTimeout`.

- [ ] **Step 1: Add dependency**

```bash
go get golang.org/x/crypto/ssh golang.org/x/crypto/ssh/agent
```

- [ ] **Step 2: Write the test SSH server helper**

`internal/sshcheck/server_test.go`:

```go
package sshcheck

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

// execHandler returns the stdout and exit status for an exec'd command.
type execHandler func(cmd string) (string, uint32)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startServer serves SSH on addr ("127.0.0.1:0" for any port), accepting
// only the authorized key, and returns the port it listens on.
func startServer(t *testing.T, addr string, authorized ssh.PublicKey, handle execHandler) int {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), authorized.Marshal()) {
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

func serveConn(c net.Conn, cfg *ssh.ServerConfig, handle execHandler) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var p struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &p)
				req.Reply(true, nil)
				out, status := handle(p.Command)
				io.WriteString(ch, out)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}
```

- [ ] **Step 3: Write failing tests**

`internal/sshcheck/sshcheck_test.go`:

```go
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
	err := checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "127.0.0.1", "/nix/store/abc-nixos-system-web1")
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
	err := checker(key, port).VerifyNixOS(withTimeout(t, 5*time.Second), "127.0.0.1", "/nix/store/abc-nixos-system-web1")
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
	err := checker(key, port).VerifyNixOS(withTimeout(t, 300*time.Millisecond), "127.0.0.1", "/nix/store/right")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `"/nix/store/wrong"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyNixOSFailsWhenNotNixOS(t *testing.T) {
	key := newSigner(t)
	port := startServer(t, "127.0.0.1:0", key.PublicKey(), func(string) (string, uint32) { return "", 1 })
	err := checker(key, port).VerifyNixOS(withTimeout(t, 300*time.Millisecond), "127.0.0.1", "/nix/store/right")
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
```

- [ ] **Step 4: Run to verify failure**

Run: `go test ./internal/sshcheck/`
Expected: FAIL — `undefined: Checker`.

- [ ] **Step 5: Implement**

`internal/sshcheck/sshcheck.go`:

```go
// Package sshcheck polls a freshly installed server over SSH.
package sshcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Backoff struct {
	Initial, Max time.Duration
}

var DefaultBackoff = Backoff{Initial: 2 * time.Second, Max: 15 * time.Second}

// attemptTimeout bounds one connect + command round trip.
const attemptTimeout = 30 * time.Second

// Checker connects as root. Host keys are not checked: the server was
// created seconds earlier by this run and its host key changes during
// install (accepted trust-on-first-use trade-off, see the design spec).
type Checker struct {
	Auth     []ssh.AuthMethod
	Port     int // 0 means 22
	Backoff  Backoff
	Progress func(format string, args ...any)
}

// WaitForSSH polls until root can log in and run a command.
func (c Checker) WaitForSSH(ctx context.Context, host string) error {
	return c.poll(ctx, host, func(client *ssh.Client) error {
		_, err := run(client, "true")
		return err
	})
}

// VerifyNixOS polls until the host runs NixOS with /run/current-system
// pointing at wantOutPath. Polling matters: right after install the kexec
// installer (also NixOS) may still be answering SSH.
func (c Checker) VerifyNixOS(ctx context.Context, host, wantOutPath string) error {
	return c.poll(ctx, host, func(client *ssh.Client) error {
		out, err := run(client, "test -e /etc/NIXOS && readlink /run/current-system")
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(out); got != wantOutPath {
			return fmt.Errorf("running system is %q, want %q", got, wantOutPath)
		}
		return nil
	})
}

func (c Checker) poll(ctx context.Context, host string, check func(*ssh.Client) error) error {
	delay := c.Backoff.Initial
	for {
		err := c.attempt(ctx, host, check)
		if err == nil {
			return nil
		}
		c.progress("    %s not ready: %v (retrying in %s)", host, err, delay)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
		delay = min(delay*2, c.Backoff.Max)
	}
}

func (c Checker) attempt(ctx context.Context, host string, check func(*ssh.Client) error) error {
	port := c.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(attemptTimeout))
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            c.Auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	return check(client)
}

func run(client *ssh.Client, cmd string) (string, error) {
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	out, err := s.Output(cmd)
	if err != nil {
		return "", fmt.Errorf("%q: %w", cmd, err)
	}
	return string(out), nil
}

func (c Checker) progress(format string, args ...any) {
	if c.Progress != nil {
		c.Progress(format, args...)
	}
}

// AuthMethods loads the private key at identity, or, when identity is
// empty, uses the ssh-agent listening on agentSock. Call the returned
// func to release the agent connection.
func AuthMethods(identity, agentSock string) ([]ssh.AuthMethod, func() error, error) {
	if identity != "" {
		data, err := os.ReadFile(identity)
		if err != nil {
			return nil, nil, fmt.Errorf("reading identity: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, nil, fmt.Errorf("%s is passphrase-protected: add it to ssh-agent and omit --identity", identity)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parsing identity %s: %w", identity, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, func() error { return nil }, nil
	}
	if agentSock == "" {
		return nil, nil, errors.New("no --identity given and SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", agentSock)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to ssh-agent: %w", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(conn).Signers)}, conn.Close, nil
}
```

- [ ] **Step 6: Run tests**

Run: `go test -race ./internal/sshcheck/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/sshcheck
git commit -m "feat(sshcheck): wait for SSH and verify NixOS boot

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: `hetzner` package

**Files:**
- Create: `internal/hetzner/hetzner.go`
- Test: `internal/hetzner/hetzner_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Server struct { ID int64; Name, IPv4, IPv6 string }`
  - `type CreateRequest struct { Name, ServerType, Location, Image, SSHKey string; Firewalls, Networks []string; Labels map[string]string }`
  - `func New(token string, opts ...hcloud.ClientOption) *Client`
  - `func (c *Client) Preflight(ctx context.Context, r CreateRequest) error`
  - `func (c *Client) CreateServer(ctx context.Context, r CreateRequest) (Server, error)` — returns a `Server` with non-zero `ID` whenever the server exists, even alongside an error.
  - `func (c *Client) DeleteServer(ctx context.Context, id int64) error` — not-found is success.
  - `func (c *Client) SetLabels(ctx context.Context, id int64, labels map[string]string) error` — replaces all labels.

- [ ] **Step 1: Add dependency**

```bash
go get github.com/hetznercloud/hcloud-go/v2
```

- [ ] **Step 2: Write failing tests with a fake API**

`internal/hetzner/hetzner_test.go`:

```go
package hetzner

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type fakeAPI struct {
	mu              sync.Mutex
	serverTypes     map[string]string // name -> architecture
	locations       map[string]bool
	images          map[string]bool // "name/arch"
	sshKeys         map[string]bool
	firewalls       map[string]bool
	networks        map[string]bool
	existingServers map[string]bool
	actionStatus    string
	createBody      map[string]any
	updateBody      map[string]any
	deleted         []string
	imageQueries    []string
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func newFakeAPI(t *testing.T) (*Client, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{
		serverTypes:     map[string]string{"cx32": "x86", "cax11": "arm"},
		locations:       set("fsn1"),
		images:          set("debian-12/x86", "debian-12/arm"),
		sshKeys:         set("admin"),
		firewalls:       set("web"),
		networks:        set("internal"),
		existingServers: map[string]bool{},
		actionStatus:    "success",
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := New("test-token",
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}),
	)
	return c, f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func named(ok bool, id int, name string) []any {
	if !ok {
		return nil
	}
	return []any{map[string]any{"id": id, "name": name}}
}

func serverJSON(id int, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "initializing",
		"public_net": map[string]any{
			"ipv4": map[string]any{"id": 1, "ip": "203.0.113.10"},
			"ipv6": map[string]any{"id": 2, "ip": "2001:db8:1:2::/64"},
		},
		"labels": map[string]any{},
	}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	name := q.Get("name")
	get := r.Method == http.MethodGet
	switch {
	case get && r.URL.Path == "/server_types":
		var items []any
		if arch, ok := f.serverTypes[name]; ok {
			items = []any{map[string]any{"id": 1, "name": name, "architecture": arch}}
		}
		writeJSON(w, 200, map[string]any{"server_types": items})
	case get && r.URL.Path == "/locations":
		writeJSON(w, 200, map[string]any{"locations": named(f.locations[name], 2, name)})
	case get && r.URL.Path == "/images":
		key := name + "/" + q.Get("architecture")
		f.imageQueries = append(f.imageQueries, key)
		var items []any
		if f.images[key] {
			items = []any{map[string]any{"id": 10, "name": name, "type": "system", "architecture": q.Get("architecture")}}
		}
		writeJSON(w, 200, map[string]any{"images": items})
	case get && r.URL.Path == "/ssh_keys":
		writeJSON(w, 200, map[string]any{"ssh_keys": named(f.sshKeys[name], 5, name)})
	case get && r.URL.Path == "/firewalls":
		writeJSON(w, 200, map[string]any{"firewalls": named(f.firewalls[name], 7, name)})
	case get && r.URL.Path == "/networks":
		writeJSON(w, 200, map[string]any{"networks": named(f.networks[name], 8, name)})
	case get && r.URL.Path == "/servers":
		var items []any
		if f.existingServers[name] {
			items = []any{serverJSON(99, name)}
		}
		writeJSON(w, 200, map[string]any{"servers": items})
	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		_ = json.NewDecoder(r.Body).Decode(&f.createBody)
		writeJSON(w, 201, map[string]any{
			"server":       serverJSON(42, f.createBody["name"].(string)),
			"action":       map[string]any{"id": 100, "status": "running", "command": "create_server"},
			"next_actions": []any{map[string]any{"id": 101, "status": "running", "command": "start_server"}},
		})
	case get && r.URL.Path == "/actions":
		var actions []any
		for _, id := range q["id"] {
			n, _ := strconv.Atoi(id)
			a := map[string]any{"id": n, "status": f.actionStatus, "command": "x"}
			if f.actionStatus == "error" {
				a["error"] = map[string]any{"code": "action_failed", "message": "boom"}
			}
			actions = append(actions, a)
		}
		writeJSON(w, 200, map[string]any{"actions": actions})
	case r.Method == http.MethodDelete && r.URL.Path == "/servers/42":
		f.deleted = append(f.deleted, "42")
		writeJSON(w, 200, map[string]any{"action": map[string]any{"id": 102, "status": "running", "command": "delete_server"}})
	case r.Method == http.MethodPut && r.URL.Path == "/servers/42":
		_ = json.NewDecoder(r.Body).Decode(&f.updateBody)
		writeJSON(w, 200, map[string]any{"server": serverJSON(42, "web1")})
	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": r.Method + " " + r.URL.String()}})
	}
}

func validRequest() CreateRequest {
	return CreateRequest{
		Name: "web1", ServerType: "cx32", Location: "fsn1", Image: "debian-12", SSHKey: "admin",
		Firewalls: []string{"web"}, Networks: []string{"internal"},
		Labels: map[string]string{"managed-by": "hinfect"},
	}
}

func TestPreflightOK(t *testing.T) {
	c, _ := newFakeAPI(t)
	if err := c.Preflight(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightReportsEveryProblem(t *testing.T) {
	c, f := newFakeAPI(t)
	f.existingServers["web1"] = true
	r := validRequest()
	r.ServerType = "nope"
	r.Firewalls = []string{"web", "missing-fw"}
	err := c.Preflight(context.Background(), r)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`server type "nope" not found`, `firewall "missing-fw" not found`, `server "web1" already exists`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPreflightLooksUpImageForServerTypeArchitecture(t *testing.T) {
	c, f := newFakeAPI(t)
	r := validRequest()
	r.ServerType = "cax11"
	if err := c.Preflight(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.imageQueries, "debian-12/arm") {
		t.Fatalf("image queries = %v, want debian-12/arm", f.imageQueries)
	}
}

func TestCreateServer(t *testing.T) {
	c, f := newFakeAPI(t)
	srv, err := c.CreateServer(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := Server{ID: 42, Name: "web1", IPv4: "203.0.113.10", IPv6: "2001:db8:1:2::1"}
	if srv != want {
		t.Fatalf("server = %+v, want %+v", srv, want)
	}
	b := f.createBody
	if b["name"] != "web1" || b["start_after_create"] != true {
		t.Errorf("create body = %v", b)
	}
	labels := map[string]string{}
	for k, v := range b["labels"].(map[string]any) {
		labels[k] = v.(string)
	}
	if !maps.Equal(labels, map[string]string{"managed-by": "hinfect"}) {
		t.Errorf("labels = %v", labels)
	}
	for _, field := range []string{"ssh_keys", "firewalls", "networks"} {
		if items, _ := b[field].([]any); len(items) != 1 {
			t.Errorf("%s = %v, want one entry", field, b[field])
		}
	}
}

func TestCreateServerReturnsServerWhenStartFails(t *testing.T) {
	c, f := newFakeAPI(t)
	f.actionStatus = "error"
	srv, err := c.CreateServer(context.Background(), validRequest())
	if err == nil {
		t.Fatal("want error")
	}
	if srv.ID != 42 {
		t.Fatalf("server ID = %d, want 42 so the caller can delete it", srv.ID)
	}
}

func TestDeleteServer(t *testing.T) {
	c, f := newFakeAPI(t)
	if err := c.DeleteServer(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.deleted, []string{"42"}) {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestDeleteServerNotFoundIsNotAnError(t *testing.T) {
	c, _ := newFakeAPI(t)
	if err := c.DeleteServer(context.Background(), 404); err != nil {
		t.Fatal(err)
	}
}

func TestSetLabels(t *testing.T) {
	c, f := newFakeAPI(t)
	if err := c.SetLabels(context.Background(), 42, map[string]string{"hinfect-status": "ready"}); err != nil {
		t.Fatal(err)
	}
	got := f.updateBody["labels"].(map[string]any)
	if got["hinfect-status"] != "ready" {
		t.Fatalf("update body = %v", f.updateBody)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/hetzner/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Implement**

`internal/hetzner/hetzner.go`:

```go
// Package hetzner is the slice of the Hetzner Cloud API hinfect needs.
package hetzner

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type Server struct {
	ID   int64
	Name string
	IPv4 string
	IPv6 string // the ::1 address of the server's /64
}

type CreateRequest struct {
	Name       string
	ServerType string
	Location   string
	Image      string // bootstrap image name, resolved for the server type's architecture
	SSHKey     string
	Firewalls  []string
	Networks   []string
	Labels     map[string]string
}

type Client struct {
	hc *hcloud.Client
}

func New(token string, opts ...hcloud.ClientOption) *Client {
	opts = append([]hcloud.ClientOption{hcloud.WithToken(token), hcloud.WithApplication("hinfect", "dev")}, opts...)
	return &Client{hc: hcloud.NewClient(opts...)}
}

type resolved struct {
	serverType *hcloud.ServerType
	location   *hcloud.Location
	image      *hcloud.Image
	sshKey     *hcloud.SSHKey
	firewalls  []*hcloud.Firewall
	networks   []*hcloud.Network
}

// resolve looks up every named resource and reports all missing ones.
func (c *Client) resolve(ctx context.Context, r CreateRequest) (*resolved, error) {
	var errs []error
	missing := func(kind, name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("looking up %s %q: %w", kind, name, err))
		} else {
			errs = append(errs, fmt.Errorf("%s %q not found", kind, name))
		}
	}
	res := &resolved{}
	var err error
	if res.serverType, _, err = c.hc.ServerType.GetByName(ctx, r.ServerType); res.serverType == nil {
		missing("server type", r.ServerType, err)
	}
	if res.location, _, err = c.hc.Location.GetByName(ctx, r.Location); res.location == nil {
		missing("location", r.Location, err)
	}
	if res.serverType != nil {
		arch := res.serverType.Architecture
		if res.image, _, err = c.hc.Image.GetForArchitecture(ctx, r.Image, arch); res.image == nil {
			missing(fmt.Sprintf("%s image", arch), r.Image, err)
		}
	}
	if res.sshKey, _, err = c.hc.SSHKey.GetByName(ctx, r.SSHKey); res.sshKey == nil {
		missing("ssh key", r.SSHKey, err)
	}
	for _, name := range r.Firewalls {
		fw, _, err := c.hc.Firewall.GetByName(ctx, name)
		if fw == nil {
			missing("firewall", name, err)
			continue
		}
		res.firewalls = append(res.firewalls, fw)
	}
	for _, name := range r.Networks {
		n, _, err := c.hc.Network.GetByName(ctx, name)
		if n == nil {
			missing("network", name, err)
			continue
		}
		res.networks = append(res.networks, n)
	}
	return res, errors.Join(errs...)
}

// Preflight checks that every referenced resource exists and the name is free.
func (c *Client) Preflight(ctx context.Context, r CreateRequest) error {
	_, err := c.resolve(ctx, r)
	existing, _, lookupErr := c.hc.Server.GetByName(ctx, r.Name)
	switch {
	case lookupErr != nil:
		err = errors.Join(err, fmt.Errorf("looking up server %q: %w", r.Name, lookupErr))
	case existing != nil:
		err = errors.Join(err, fmt.Errorf("server %q already exists (id %d)", r.Name, existing.ID))
	}
	return err
}

// CreateServer creates and starts the server. Whenever the server exists
// the returned Server has its ID set, even if an error is also returned.
func (c *Client) CreateServer(ctx context.Context, r CreateRequest) (Server, error) {
	res, err := c.resolve(ctx, r)
	if err != nil {
		return Server{}, err
	}
	opts := hcloud.ServerCreateOpts{
		Name:             r.Name,
		ServerType:       res.serverType,
		Image:            res.image,
		Location:         res.location,
		SSHKeys:          []*hcloud.SSHKey{res.sshKey},
		Networks:         res.networks,
		Labels:           r.Labels,
		StartAfterCreate: hcloud.Ptr(true),
	}
	for _, fw := range res.firewalls {
		opts.Firewalls = append(opts.Firewalls, &hcloud.ServerCreateFirewall{Firewall: *fw})
	}
	result, _, err := c.hc.Server.Create(ctx, opts)
	if err != nil {
		return Server{}, fmt.Errorf("creating server: %w", err)
	}
	srv := toServer(result.Server)
	actions := slices.DeleteFunc(append([]*hcloud.Action{result.Action}, result.NextActions...),
		func(a *hcloud.Action) bool { return a == nil })
	if err := c.hc.Action.WaitFor(ctx, actions...); err != nil {
		return srv, fmt.Errorf("waiting for server %d to start: %w", srv.ID, err)
	}
	return srv, nil
}

func toServer(s *hcloud.Server) Server {
	out := Server{ID: s.ID, Name: s.Name}
	if ip := s.PublicNet.IPv4.IP; ip != nil {
		out.IPv4 = ip.String()
	}
	if n := s.PublicNet.IPv6.Network; n != nil {
		ip := slices.Clone(n.IP.To16())
		ip[15] |= 1
		out.IPv6 = ip.String()
	}
	return out
}

// DeleteServer deletes the server and waits for it to be gone. A server
// that no longer exists counts as deleted.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	res, _, err := c.hc.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	if res.Action != nil {
		if err := c.hc.Action.WaitFor(ctx, res.Action); err != nil {
			return fmt.Errorf("waiting for server %d deletion: %w", id, err)
		}
	}
	return nil
}

// SetLabels replaces all of the server's labels.
func (c *Client) SetLabels(ctx context.Context, id int64, labels map[string]string) error {
	if _, _, err := c.hc.Server.Update(ctx, &hcloud.Server{ID: id}, hcloud.ServerUpdateOpts{Labels: labels}); err != nil {
		return fmt.Errorf("setting labels on server %d: %w", id, err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test -race ./internal/hetzner/`
Expected: PASS.

If a test fails because hcloud-go sends a request the fake doesn't route (the default case returns 404 with `METHOD URL` in the message), add that route to `fakeAPI.serve` with the minimal JSON hcloud-go needs, rather than changing `hetzner.go` to suit the fake.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/hetzner
git commit -m "feat(hetzner): preflight, create, delete, label servers

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: `create` orchestrator

**Files:**
- Create: `internal/create/create.go`
- Test: `internal/create/create_test.go`

**Interfaces:**
- Consumes: `config.CreateOptions`, `config.SystemLabels`, `config.StatusInstalling/StatusReady`; `pipeline.Pipeline/Step/Cleanup`; `hetzner.Server`, `hetzner.CreateRequest`; `nixos.InstallArgs`.
- Produces:
  - Interfaces `Hetzner` (Preflight, CreateServer, DeleteServer, SetLabels — signatures exactly as in Task 5), `Nix` (`Eval(ctx, config.FlakeRef) (string, error)`, `Install(ctx, nixos.InstallArgs, io.Writer) error`), `SSH` (`WaitForSSH(ctx, host string) error`, `VerifyNixOS(ctx, host, wantOutPath string) error`)
  - `type Deps struct { Hetzner Hetzner; Nix Nix; SSH SSH; CheckTools func() error; Stderr io.Writer; Now func() time.Time; TempDir string; SSHWait, VerifyWait, CleanupTimeout time.Duration; OnServerCreated func(hetzner.Server) }`
  - `type Result struct { Server hetzner.Server; StorePath string }`
  - `var ErrCleanupFailed = errors.New("cleanup failed")`
  - `func Run(ctx context.Context, opts config.CreateOptions, d Deps) (Result, error)`

- [ ] **Step 1: Write failing tests**

`internal/create/create_test.go`:

```go
package create

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hinfect/internal/config"
	"hinfect/internal/hetzner"
	"hinfect/internal/nixos"
)

const storePath = "/nix/store/abc-nixos-system-web1"

var errBoom = errors.New("boom")

type fakes struct {
	mu                                        sync.Mutex
	calls                                     []string
	preflightErr, evalErr, createErr, waitErr error
	installErr, verifyErr, deleteErr          error
	createNoServer                            bool
	installStarted                            chan struct{}
	blockInstall                              bool
	labels                                    map[string]string
	createReq                                 hetzner.CreateRequest
	installArgs                               nixos.InstallArgs
	verifiedPath                              string
}

func (f *fakes) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakes) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakes) Preflight(ctx context.Context, r hetzner.CreateRequest) error {
	f.record("preflight")
	return f.preflightErr
}

func (f *fakes) CreateServer(ctx context.Context, r hetzner.CreateRequest) (hetzner.Server, error) {
	f.record("create")
	f.createReq = r
	if f.createNoServer {
		return hetzner.Server{}, f.createErr
	}
	return hetzner.Server{ID: 42, Name: r.Name, IPv4: "203.0.113.10", IPv6: "2001:db8::1"}, f.createErr
}

func (f *fakes) DeleteServer(ctx context.Context, id int64) error {
	f.record(fmt.Sprintf("delete %d", id))
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.deleteErr
}

func (f *fakes) SetLabels(ctx context.Context, id int64, labels map[string]string) error {
	f.record("labels")
	f.labels = labels
	return nil
}

func (f *fakes) Eval(ctx context.Context, fl config.FlakeRef) (string, error) {
	f.record("eval")
	return storePath, f.evalErr
}

func (f *fakes) Install(ctx context.Context, a nixos.InstallArgs, log io.Writer) error {
	f.record("install")
	f.installArgs = a
	fmt.Fprintln(log, "nixos-anywhere output")
	if f.installStarted != nil {
		close(f.installStarted)
	}
	if f.blockInstall {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.installErr
}

func (f *fakes) WaitForSSH(ctx context.Context, host string) error {
	f.record("wait ssh")
	return f.waitErr
}

func (f *fakes) VerifyNixOS(ctx context.Context, host, want string) error {
	f.record("verify")
	f.verifiedPath = want
	return f.verifyErr
}

func testOpts(t *testing.T) config.CreateOptions {
	t.Helper()
	flake, err := config.ParseFlake(".#web1")
	if err != nil {
		t.Fatal(err)
	}
	return config.CreateOptions{
		Name: "web1", Flake: flake, ServerType: "cx32", Location: "fsn1", SSHKey: "admin",
		Image: "debian-12", LogDir: filepath.Join(t.TempDir(), "logs"), Timeout: time.Minute,
		Labels: map[string]string{"env": "prod"},
	}
}

func testDeps(t *testing.T, f *fakes, stderr io.Writer) Deps {
	return Deps{
		Hetzner: f, Nix: f, SSH: f,
		CheckTools:     func() error { return nil },
		Stderr:         stderr,
		Now:            func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) },
		TempDir:        t.TempDir(),
		SSHWait:        time.Second,
		VerifyWait:     time.Second,
		CleanupTimeout: time.Second,
	}
}

func TestRunSuccess(t *testing.T) {
	f := &fakes{}
	opts := testOpts(t)
	var stderr bytes.Buffer
	res, err := Run(context.Background(), opts, testDeps(t, f, &stderr))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "eval", "create", "wait ssh", "install", "verify", "labels"}
	if got := f.Calls(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if res.Server.ID != 42 || res.StorePath != storePath {
		t.Fatalf("result = %+v", res)
	}
	if f.createReq.Labels["hinfect-status"] != "installing" || f.createReq.Labels["env"] != "prod" {
		t.Errorf("create labels = %v", f.createReq.Labels)
	}
	if f.labels["hinfect-status"] != "ready" || f.labels["env"] != "prod" {
		t.Errorf("final labels = %v", f.labels)
	}
	if f.installArgs.Host != "203.0.113.10" || f.installArgs.Flake != opts.Flake {
		t.Errorf("install args = %+v", f.installArgs)
	}
	if f.verifiedPath != storePath {
		t.Errorf("verified %q, want %q", f.verifiedPath, storePath)
	}
	if _, err := os.Stat(opts.LogDir); !os.IsNotExist(err) {
		t.Errorf("log dir created on success: %v", err)
	}
}

func TestRunInvalidOptionsTouchesNothing(t *testing.T) {
	f := &fakes{}
	opts := testOpts(t)
	opts.Name = "Bad Name"
	if _, err := Run(context.Background(), opts, testDeps(t, f, io.Discard)); err == nil {
		t.Fatal("want error")
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("calls = %v, want none", f.Calls())
	}
}

func TestRunMissingToolsCreatesNothing(t *testing.T) {
	f := &fakes{}
	d := testDeps(t, f, io.Discard)
	d.CheckTools = func() error { return errBoom }
	if _, err := Run(context.Background(), testOpts(t), d); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("calls = %v, want none", f.Calls())
	}
}

func TestRunFailureBeforeServerExistsDoesNotDelete(t *testing.T) {
	for name, f := range map[string]*fakes{
		"preflight":             {preflightErr: errBoom},
		"eval":                  {evalErr: errBoom},
		"create without server": {createErr: errBoom, createNoServer: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), testOpts(t), testDeps(t, f, io.Discard))
			if !errors.Is(err, errBoom) || errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("err = %v", err)
			}
			for _, c := range f.Calls() {
				if strings.HasPrefix(c, "delete") {
					t.Fatalf("calls = %v, want no delete", f.Calls())
				}
			}
		})
	}
}

func TestRunDeletesServerAndSavesLogOnFailure(t *testing.T) {
	cases := map[string]*fakes{
		"create with server": {createErr: errBoom},
		"wait ssh":           {waitErr: errBoom},
		"install":            {installErr: errBoom},
		"verify":             {verifyErr: errBoom},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			opts := testOpts(t)
			var stderr bytes.Buffer
			_, err := Run(context.Background(), opts, testDeps(t, f, &stderr))
			if !errors.Is(err, errBoom) || errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("err = %v", err)
			}
			calls := f.Calls()
			if calls[len(calls)-1] != "delete 42" {
				t.Fatalf("calls = %v, want delete 42 last", calls)
			}
			logPath := filepath.Join(opts.LogDir, "web1-20260922T100000Z.log")
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("log not saved: %v", err)
			}
			if !strings.Contains(string(data), "==> create server") || !strings.Contains(string(data), "boom") {
				t.Errorf("log content = %q", data)
			}
			if !strings.Contains(stderr.String(), logPath) {
				t.Errorf("stderr does not mention log path: %q", stderr.String())
			}
		})
	}
}

func TestRunLogContainsInstallOutput(t *testing.T) {
	f := &fakes{installErr: errBoom}
	opts := testOpts(t)
	_, _ = Run(context.Background(), opts, testDeps(t, f, io.Discard))
	data, _ := os.ReadFile(filepath.Join(opts.LogDir, "web1-20260922T100000Z.log"))
	if !strings.Contains(string(data), "nixos-anywhere output") {
		t.Fatalf("log = %q", data)
	}
}

func TestRunCleanupFailure(t *testing.T) {
	f := &fakes{installErr: errBoom, deleteErr: errors.New("api down")}
	var stderr bytes.Buffer
	_, err := Run(context.Background(), testOpts(t), testDeps(t, f, &stderr))
	if !errors.Is(err, ErrCleanupFailed) || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want ErrCleanupFailed wrapping the original error", err)
	}
	if !strings.Contains(stderr.String(), "hcloud server delete 42") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCancelledDuringInstallStillDeletes(t *testing.T) {
	f := &fakes{blockInstall: true, installStarted: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.installStarted
		cancel()
	}()
	_, err := Run(ctx, testOpts(t), testDeps(t, f, io.Discard))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	calls := f.Calls()
	if calls[len(calls)-1] != "delete 42" {
		t.Fatalf("calls = %v, want delete 42 despite cancellation", calls)
	}
}

func TestRunReportsCreatedServer(t *testing.T) {
	f := &fakes{installErr: errBoom}
	d := testDeps(t, f, io.Discard)
	var got hetzner.Server
	d.OnServerCreated = func(s hetzner.Server) { got = s }
	_, _ = Run(context.Background(), testOpts(t), d)
	if got.ID != 42 {
		t.Fatalf("OnServerCreated got %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/create/`
Expected: FAIL — `undefined: Run`.

- [ ] **Step 3: Implement**

`internal/create/create.go`:

```go
// Package create implements `hinfect create`: provision a Hetzner server,
// install NixOS with nixos-anywhere, verify the boot, and delete the server
// if anything fails.
package create

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"hinfect/internal/config"
	"hinfect/internal/hetzner"
	"hinfect/internal/nixos"
	"hinfect/internal/pipeline"
)

type Hetzner interface {
	Preflight(ctx context.Context, r hetzner.CreateRequest) error
	CreateServer(ctx context.Context, r hetzner.CreateRequest) (hetzner.Server, error)
	DeleteServer(ctx context.Context, id int64) error
	SetLabels(ctx context.Context, id int64, labels map[string]string) error
}

type Nix interface {
	Eval(ctx context.Context, f config.FlakeRef) (string, error)
	Install(ctx context.Context, a nixos.InstallArgs, log io.Writer) error
}

type SSH interface {
	WaitForSSH(ctx context.Context, host string) error
	VerifyNixOS(ctx context.Context, host, wantOutPath string) error
}

type Deps struct {
	Hetzner        Hetzner
	Nix            Nix
	SSH            SSH
	CheckTools     func() error
	Stderr         io.Writer
	Now            func() time.Time
	TempDir        string // where the in-progress log lives; "" means os.TempDir()
	SSHWait        time.Duration
	VerifyWait     time.Duration
	CleanupTimeout time.Duration
	// OnServerCreated is called as soon as the server exists, so the caller
	// can name it if the process is force-killed.
	OnServerCreated func(hetzner.Server)
}

type Result struct {
	Server    hetzner.Server
	StorePath string
}

// ErrCleanupFailed means the run failed and the server may still exist.
var ErrCleanupFailed = errors.New("cleanup failed")

func Run(ctx context.Context, opts config.CreateOptions, d Deps) (Result, error) {
	if err := opts.Validate(); err != nil {
		return Result{}, err
	}
	logFile, err := os.CreateTemp(d.TempDir, "hinfect-*.log")
	if err != nil {
		return Result{}, err
	}
	out := io.MultiWriter(d.Stderr, logFile)
	progress := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	var (
		res     Result
		outPath string
	)
	req := hetzner.CreateRequest{
		Name: opts.Name, ServerType: opts.ServerType, Location: opts.Location, Image: opts.Image,
		SSHKey: opts.SSHKey, Firewalls: opts.Firewalls, Networks: opts.Networks,
		Labels: config.SystemLabels(opts.Labels, opts.Flake, config.StatusInstalling),
	}
	steps := []pipeline.Step{
		{Name: "preflight", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			if err := d.CheckTools(); err != nil {
				return nil, err
			}
			if err := d.Hetzner.Preflight(ctx, req); err != nil {
				return nil, err
			}
			p, err := d.Nix.Eval(ctx, opts.Flake)
			outPath = p
			return nil, err
		}},
		{Name: "create server", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			srv, err := d.Hetzner.CreateServer(ctx, req)
			if srv.ID == 0 {
				return nil, err
			}
			res.Server = srv
			if d.OnServerCreated != nil {
				d.OnServerCreated(srv)
			}
			progress("    server %s (id %d) at %s", srv.Name, srv.ID, srv.IPv4)
			return func(ctx context.Context) error { return d.Hetzner.DeleteServer(ctx, srv.ID) }, err
		}},
		{Name: "wait for ssh", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			ctx, cancel := context.WithTimeout(ctx, d.SSHWait)
			defer cancel()
			return nil, d.SSH.WaitForSSH(ctx, res.Server.IPv4)
		}},
		{Name: "install nixos", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			args := nixos.InstallArgs{Flake: opts.Flake, Host: res.Server.IPv4, Identity: opts.Identity}
			return nil, d.Nix.Install(ctx, args, out)
		}},
		{Name: "verify", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			ctx, cancel := context.WithTimeout(ctx, d.VerifyWait)
			defer cancel()
			return nil, d.SSH.VerifyNixOS(ctx, res.Server.IPv4, outPath)
		}},
		{Name: "finalize", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			labels := config.SystemLabels(opts.Labels, opts.Flake, config.StatusReady)
			return nil, d.Hetzner.SetLabels(ctx, res.Server.ID, labels)
		}},
	}

	p := &pipeline.Pipeline{Progress: progress}
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	runErr := p.Run(runCtx, steps)
	if runErr == nil {
		logFile.Close()
		os.Remove(logFile.Name())
		res.StorePath = outPath
		return res, nil
	}

	progress("error: %v", runErr)
	if logPath, err := persistLog(logFile, opts.LogDir, opts.Name, d.Now()); err != nil {
		fmt.Fprintf(d.Stderr, "warning: could not save log: %v (temporary log: %s)\n", err, logFile.Name())
	} else {
		fmt.Fprintf(d.Stderr, "log saved to %s\n", logPath)
	}

	// The run context may be cancelled or expired; cleanup gets its own budget.
	p.Progress = func(format string, args ...any) { fmt.Fprintf(d.Stderr, format+"\n", args...) }
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), d.CleanupTimeout)
	defer cleanupCancel()
	if err := p.Cleanup(cleanupCtx); err != nil {
		s := res.Server
		fmt.Fprintf(d.Stderr, "\n!!! cleanup failed: server %s (id %d) may still exist and is being billed.\n!!! delete it with: hcloud server delete %d\n", s.Name, s.ID, s.ID)
		return res, fmt.Errorf("%w: %w (while handling: %w)", ErrCleanupFailed, err, runErr)
	}
	return res, runErr
}

// persistLog closes the temporary log and moves it to dir.
func persistLog(f *os.File, dir, name string, now time.Time) (string, error) {
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.log", name, now.UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", err
	}
	os.Remove(f.Name())
	return dst, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/create/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/create
git commit -m "feat(create): orchestrate install with cleanup on failure

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: `hinfect` CLI

**Files:**
- Create: `cmd/hinfect/main.go`
- Test: `cmd/hinfect/main_test.go`

**Interfaces:**
- Consumes: `config.ParseFlake`, `config.ParseLabels`, `config.CreateOptions`; `create.Run`, `create.Deps`, `create.ErrCleanupFailed`; `hetzner.New`, `hetzner.Server`; `nixos.Nix`, `nixos.ExecRunner`, `nixos.CheckTools`; `sshcheck.Checker`, `sshcheck.DefaultBackoff`, `sshcheck.AuthMethods`.
- Produces: `func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, created *atomic.Pointer[hetzner.Server]) int` (used by the integration test in Task 8).

- [ ] **Step 1: Add dependency**

```bash
go get github.com/spf13/cobra
```

- [ ] **Step 2: Write failing tests**

`cmd/hinfect/main_test.go`:

```go
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
```

(`--flake` given twice: cobra's `StringVar` keeps the last value, so `web1` wins.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./cmd/hinfect/`
Expected: FAIL — `undefined: run`.

- [ ] **Step 4: Implement**

`cmd/hinfect/main.go`:

```go
// Command hinfect creates Hetzner Cloud servers running NixOS.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"hinfect/internal/config"
	"hinfect/internal/create"
	"hinfect/internal/hetzner"
	"hinfect/internal/nixos"
	"hinfect/internal/sshcheck"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	var created atomic.Pointer[hetzner.Server]
	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go handleSignals(sigs, cancel, &created)
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv, &created))
}

// handleSignals cancels on the first signal (triggering cleanup), ignores
// the second, and force-exits on the third.
func handleSignals(sigs <-chan os.Signal, cancel context.CancelFunc, created *atomic.Pointer[hetzner.Server]) {
	n := 0
	for range sigs {
		n++
		switch n {
		case 1:
			fmt.Fprintln(os.Stderr, "\ninterrupted: cleaning up (press Ctrl-C twice more to force exit)")
			cancel()
		case 2:
			fmt.Fprintln(os.Stderr, "cleanup in progress; press Ctrl-C once more to force exit")
		default:
			if s := created.Load(); s != nil {
				fmt.Fprintf(os.Stderr, "!!! forced exit: server %s (id %d) may still exist and is being billed.\n!!! delete it with: hcloud server delete %d\n", s.Name, s.ID, s.ID)
			}
			os.Exit(2)
		}
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, created *atomic.Pointer[hetzner.Server]) int {
	root := &cobra.Command{
		Use:          "hinfect",
		Short:        "Install NixOS on Hetzner Cloud servers",
		SilenceUsage: true,
	}
	root.AddCommand(newCreateCmd(stdout, stderr, getenv, created))
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, create.ErrCleanupFailed):
		return 2
	default:
		return 1
	}
}

func newCreateCmd(stdout, stderr io.Writer, getenv func(string) string, created *atomic.Pointer[hetzner.Server]) *cobra.Command {
	var (
		opts   config.CreateOptions
		flake  string
		labels []string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a Hetzner Cloud server and install NixOS on it from a flake",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			var err error
			if opts.Flake, err = config.ParseFlake(flake); err != nil {
				return err
			}
			if opts.Labels, err = config.ParseLabels(labels); err != nil {
				return err
			}
			token := getenv("HCLOUD_TOKEN")
			if token == "" {
				return errors.New("HCLOUD_TOKEN is not set")
			}
			auth, closeAuth, err := sshcheck.AuthMethods(opts.Identity, getenv("SSH_AUTH_SOCK"))
			if err != nil {
				return err
			}
			defer closeAuth()

			progress := func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) }
			deps := create.Deps{
				Hetzner:         hetzner.New(token),
				Nix:             nixos.Nix{Runner: nixos.ExecRunner{}},
				SSH:             sshcheck.Checker{Auth: auth, Backoff: sshcheck.DefaultBackoff, Progress: progress},
				CheckTools:      func() error { return nixos.CheckTools(exec.LookPath) },
				Stderr:          stderr,
				Now:             time.Now,
				SSHWait:         5 * time.Minute,
				VerifyWait:      5 * time.Minute,
				CleanupTimeout:  2 * time.Minute,
				OnServerCreated: func(s hetzner.Server) { created.Store(&s) },
			}
			res, err := create.Run(cmd.Context(), opts, deps)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s %s %s %s\n", res.Server.Name, res.Server.IPv4, res.Server.IPv6, res.StorePath)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&flake, "flake", "", "flake to install, as <uri>#<host> (e.g. .#web1)")
	f.StringVar(&opts.ServerType, "type", "", "Hetzner server type (e.g. cx32, cax21)")
	f.StringVar(&opts.Location, "location", "", "Hetzner location (e.g. fsn1)")
	f.StringVar(&opts.SSHKey, "ssh-key", "", "name of an SSH key registered in the Hetzner project")
	f.StringVar(&opts.Identity, "identity", "", "private key file for that SSH key (default: use ssh-agent)")
	f.StringVar(&opts.Image, "image", "debian-12", "bootstrap image nixos-anywhere replaces")
	f.StringArrayVar(&opts.Firewalls, "firewall", nil, "existing firewall to attach (repeatable)")
	f.StringArrayVar(&opts.Networks, "network", nil, "existing network to attach (repeatable)")
	f.StringArrayVar(&labels, "label", nil, "extra server label key=value (repeatable)")
	f.StringVar(&opts.LogDir, "log-dir", "./hinfect-logs", "directory for logs of failed installs")
	f.DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "overall deadline")
	for _, name := range []string{"flake", "type", "location", "ssh-key"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}
```

- [ ] **Step 5: Run tests and build**

Run: `go test ./... && go vet ./... && go build -o hinfect ./cmd/hinfect && ./hinfect create --help`
Expected: all PASS; help text lists every flag above.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd/hinfect/main.go cmd/hinfect/main_test.go
git commit -m "feat(cli): hinfect create command

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 8: Example flake, integration test, README

**Files:**
- Create: `examples/flake/flake.nix`, `examples/flake/configuration.nix`, `examples/flake/disk-config.nix`, `examples/flake/authorized_keys`
- Create: `cmd/hinfect/integration_test.go`
- Create: `README.md`

**Interfaces:**
- Consumes: `run(...)` from Task 7; `hetzner.New`, `(*hetzner.Client).DeleteServer`, `hetzner.Server`.
- Produces: nothing consumed by code.

- [ ] **Step 1: Write the example flake**

`examples/flake/flake.nix`:

```nix
{
  description = "Minimal NixOS host installable with hinfect on Hetzner Cloud";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    disko.url = "github:nix-community/disko";
    disko.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { nixpkgs, disko, ... }:
    let
      mkHost = system: nixpkgs.lib.nixosSystem {
        modules = [
          disko.nixosModules.disko
          ./configuration.nix
          { nixpkgs.hostPlatform = system; }
        ];
      };
    in
    {
      nixosConfigurations = {
        example = mkHost "x86_64-linux"; # cx*/cpx*/ccx* server types
        example-arm = mkHost "aarch64-linux"; # cax* server types
      };
    };
}
```

`examples/flake/configuration.nix`:

```nix
{ modulesPath, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./disk-config.nix
  ];

  # Hetzner x86 boots via BIOS, arm via UEFI; this GRUB setup handles both.
  boot.loader.grub = {
    efiSupport = true;
    efiInstallAsRemovable = true;
  };

  services.openssh = {
    enable = true;
    settings.PasswordAuthentication = false;
  };

  # hinfect verifies the install by logging in as root with the key it
  # bootstrapped with, so that public key must be in this file.
  users.users.root.openssh.authorizedKeys.keyFiles = [ ./authorized_keys ];

  system.stateVersion = "26.05";
}
```

`examples/flake/disk-config.nix`:

```nix
{
  disko.devices.disk.main = {
    type = "disk";
    device = "/dev/sda";
    content = {
      type = "gpt";
      partitions = {
        boot = {
          size = "1M";
          type = "EF02"; # BIOS boot partition for GRUB
        };
        ESP = {
          size = "512M";
          type = "EF00";
          content = {
            type = "filesystem";
            format = "vfat";
            mountpoint = "/boot";
            mountOptions = [ "umask=0077" ];
          };
        };
        root = {
          size = "100%";
          content = {
            type = "filesystem";
            format = "ext4";
            mountpoint = "/";
          };
        };
      };
    };
  };
}
```

`examples/flake/authorized_keys`:

```
# Replace this line with the public key of the Hetzner SSH key you pass to --ssh-key.
```

If `nixos-26.05` does not exist on the `NixOS/nixpkgs` branches when you do this task, use the newest `nixos-YY.MM` release branch and set `system.stateVersion` to match.

- [ ] **Step 2: Write the integration test**

`cmd/hinfect/integration_test.go`:

```go
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
```

- [ ] **Step 3: Verify it compiles and skips without credentials**

Run: `go vet -tags integration ./... && go test -tags integration -run TestCreateIntegration -v ./cmd/hinfect/`
Expected: vet clean; test output `--- SKIP: TestCreateIntegration`.

- [ ] **Step 4: Write README**

`README.md`:

````markdown
# hinfect

Create a Hetzner Cloud server and install NixOS on it from your flake, in one command.

`hinfect create` creates the server from a stock Debian image, runs
[nixos-anywhere](https://github.com/nix-community/nixos-anywhere) against it
(kexec → disko → install, building on the server), and verifies that the
server boots into exactly the system your flake describes. If anything
fails — or you press Ctrl-C — the server is deleted and the full log is kept
in `./hinfect-logs/`.

hinfect stops at a verified first boot. Deploy later changes with
`nixos-rebuild --target-host`, deploy-rs, or colmena.

## Requirements

- Nix with flakes, and a nixos-anywhere release that supports `--build-on` on your `PATH`
  (`nix profile install github:nix-community/nixos-anywhere`).
- A Hetzner Cloud API token in `HCLOUD_TOKEN`.
- An SSH key registered in the Hetzner project, whose private half is in
  your ssh-agent (or passed with `--identity`).
- A flake whose `nixosConfigurations.<host>`:
  - has a disko config for `/dev/sda`,
  - sets `nixpkgs.hostPlatform` to match the server type (`cax*` is `aarch64-linux`, the rest `x86_64-linux`),
  - authorizes that same public key for root. **Otherwise hinfect cannot verify the install and will delete the server.**

`examples/flake/` is a known-good starting point.

## Usage

```sh
export HCLOUD_TOKEN=...
hinfect create web1 \
  --flake .#web1 \
  --type cx32 \
  --location fsn1 \
  --ssh-key admin \
  --firewall web \
  --label env=prod
```

On success, stdout gets one line: `<name> <ipv4> <ipv6> <store-path>`.
Progress goes to stderr.

| Exit code | Meaning |
|---|---|
| 0 | Server created and verified |
| 1 | Failed; nothing left behind |
| 2 | Failed **and** the server could not be deleted — it is still billed; the command to delete it is printed |

Servers get the labels `managed-by=hinfect`, `hinfect-flake=<flake>`, and
`hinfect-status=installing|ready`.

## Security note

The target's SSH host key is not checked during install: the server was
created seconds earlier by the same run, and its host key changes when
nixos-anywhere kexecs into the installer and again on first boot. If you
need host key pinning, pre-generate host keys and ship them in your flake.

## Development

```sh
go test ./...
# Real Hetzner run (costs a few cents, deletes the server afterwards):
HCLOUD_TOKEN=... HINFECT_TEST_SSH_KEY=admin HINFECT_TEST_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" \
  go test -tags integration -run TestCreateIntegration -timeout 45m -v ./cmd/hinfect/
```
````

- [ ] **Step 5: Full check**

Run: `go test -race ./... && go vet ./... && go vet -tags integration ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add examples cmd/hinfect/integration_test.go README.md
git commit -m "docs: example flake, integration test, README

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

- [ ] **Step 7 (manual, needs user approval — costs money): run the integration test**

Requires Nix and nixos-anywhere installed on this machine (neither is installed as of writing) plus a Hetzner token. Ask the user before running; then use the command in the README's Development section.
