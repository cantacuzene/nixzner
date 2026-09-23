package nixos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"nixzner/internal/config"
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
