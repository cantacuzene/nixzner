package rebuild

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
	"testing"
	"time"

	"nixzner/internal/config"
	"nixzner/internal/hetzner"
	"nixzner/internal/nixos"
)

const storePath = "/nix/store/abc-nixos-system-web1"

type fakes struct {
	calls     []string
	labels    map[string]string
	setLabels map[string]string
	remotes   []nixos.Remote
	built     string
	fail      map[string]error
}

func newFakes() *fakes {
	return &fakes{
		labels: map[string]string{"managed-by": "nixzner", "nixzner-flake": "old", "nixzner-status": "ready", "env": "prod"},
		built:  storePath,
		fail:   map[string]error{},
	}
}

func (f *fakes) record(format string, args ...any) error {
	c := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, c)
	return f.fail[strings.Fields(c)[0]]
}

func (f *fakes) FindServer(_ context.Context, name string) (hetzner.Server, map[string]string, error) {
	err := f.record("find %s", name)
	return hetzner.Server{ID: 42, Name: name, IPv4: "203.0.113.10"}, f.labels, err
}

func (f *fakes) SetLabels(_ context.Context, id int64, labels map[string]string) error {
	f.setLabels = labels
	return f.record("labels %d", id)
}

func (f *fakes) Eval(_ context.Context, fl config.FlakeRef) (string, error) {
	return storePath, f.record("eval %s", fl)
}

func (f *fakes) BuildRemote(_ context.Context, fl config.FlakeRef, r nixos.Remote, log io.Writer) (string, error) {
	f.remotes = append(f.remotes, r)
	fmt.Fprintln(log, "build output")
	return f.built, f.record("build %s on %s", fl, r.Host)
}

func (f *fakes) Activate(_ context.Context, r nixos.Remote, out string, _ io.Writer) error {
	return f.record("activate %s on %s", out, r.Host)
}

func (f *fakes) MakeBootDefault(_ context.Context, r nixos.Remote, out string, _ io.Writer) error {
	return f.record("bootdefault %s on %s", out, r.Host)
}

func (f *fakes) VerifyNixOS(_ context.Context, host, want string) error {
	return f.record("verify %s %s", host, want)
}

func setup(t *testing.T) (Options, Deps, *fakes, *bytes.Buffer) {
	t.Helper()
	f := newFakes()
	fl, err := config.ParseFlake(".#web1")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Name: "web1", Flake: fl, Identity: "/k/id", LogDir: filepath.Join(t.TempDir(), "logs"), Timeout: time.Minute}
	var stderr bytes.Buffer
	d := Deps{
		Hetzner: f, Nix: f, SSH: f,
		CheckTools: func() error { return nil },
		Stderr:     &stderr,
		Now:        func() time.Time { return time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) },
		TempDir:    t.TempDir(),
		VerifyWait: time.Minute,
	}
	return opts, d, f, &stderr
}

func TestRunRebuildsInOrder(t *testing.T) {
	opts, d, f, _ := setup(t)
	res, err := Run(context.Background(), opts, d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"find web1",
		"eval .#web1",
		"build .#web1 on 203.0.113.10",
		"activate " + storePath + " on 203.0.113.10",
		"bootdefault " + storePath + " on 203.0.113.10",
		"verify 203.0.113.10 " + storePath,
		"labels 42",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if res.Server.ID != 42 || res.StorePath != storePath {
		t.Errorf("result = %+v", res)
	}
	if f.remotes[0].Identity != "/k/id" {
		t.Errorf("remote = %+v, want identity passed through", f.remotes[0])
	}
	wantLabels := map[string]string{
		"managed-by": "nixzner", "nixzner-flake": config.SanitizeLabelValue(".#web1"),
		"nixzner-status": "ready", "env": "prod",
	}
	if fmt.Sprint(f.setLabels) != fmt.Sprint(wantLabels) {
		t.Errorf("labels = %v, want %v", f.setLabels, wantLabels)
	}
	if _, err := os.Stat(opts.LogDir); !os.IsNotExist(err) {
		t.Errorf("log dir created on success: %v", err)
	}
}

func TestRunRefusesServerNotManagedByNixzner(t *testing.T) {
	opts, d, f, _ := setup(t)
	f.labels = map[string]string{"env": "prod"}
	_, err := Run(context.Background(), opts, d)
	if err == nil || !strings.Contains(err.Error(), `server "web1" is not managed by nixzner`) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"find web1"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestRunDoesNotSwitchWhenBuildFails(t *testing.T) {
	opts, d, f, stderr := setup(t)
	f.fail["build"] = errors.New("builder failed")
	_, err := Run(context.Background(), opts, d)
	if err == nil || !strings.Contains(err.Error(), "builder failed") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "activate") || strings.HasPrefix(c, "bootdefault") {
			t.Errorf("switched after a failed build: %v", f.calls)
		}
	}
	if !strings.Contains(stderr.String(), "nothing on the server was changed") {
		t.Errorf("stderr = %q", stderr.String())
	}
	logPath := filepath.Join(opts.LogDir, "web1-20260924T100000Z.log")
	if data, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(data), "build output") {
		t.Errorf("log = %q, %v", data, err)
	}
}

func TestRunRejectsBuildOfDifferentSystem(t *testing.T) {
	opts, d, f, _ := setup(t)
	f.built = "/nix/store/zzz-other"
	_, err := Run(context.Background(), opts, d)
	if err == nil || !strings.Contains(err.Error(), "built /nix/store/zzz-other, but the flake evaluates to "+storePath) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "activate") {
			t.Errorf("activated a mismatched build: %v", f.calls)
		}
	}
}

func TestRunActivationFailureExplainsRollback(t *testing.T) {
	opts, d, f, stderr := setup(t)
	f.fail["activate"] = errors.New("unit failed")
	if _, err := Run(context.Background(), opts, d); err == nil {
		t.Fatal("want error")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "bootdefault") {
			t.Errorf("made a failed activation the boot default: %v", f.calls)
		}
	}
	for _, want := range []string{
		"the previous system is still the boot default",
		"ssh root@203.0.113.10 /nix/var/nix/profiles/system/bin/switch-to-configuration test",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not mention %q", stderr.String(), want)
		}
	}
}

func TestRunValidatesOptions(t *testing.T) {
	opts, d, f, _ := setup(t)
	opts.Name = "Web_1"
	if _, err := Run(context.Background(), opts, d); err == nil {
		t.Fatal("want error for invalid name")
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %v, want none", f.calls)
	}
}
