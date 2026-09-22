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
