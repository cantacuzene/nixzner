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
	err := CheckTools(missing, "nix", "nixos-anywhere")
	if err == nil || !strings.Contains(err.Error(), "nixos-anywhere not found on PATH") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckTools(missing, "nix", "ssh"); err != nil {
		t.Fatalf("only checks the tools asked for: %v", err)
	}
	all := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	if err := CheckTools(all, "nix", "nixos-anywhere"); err != nil {
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

const sshOpts = "-o BatchMode=yes -o StrictHostKeyChecking=accept-new"

func TestBuildRemoteBuildsOnTheServer(t *testing.T) {
	r := &fakeRunner{stdout: "/nix/store/abc-nixos-system-web1\n"}
	var log bytes.Buffer
	got, err := Nix{Runner: r}.BuildRemote(context.Background(), flake(t, ".#web1"), Remote{Host: "203.0.113.10"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/nix/store/abc-nixos-system-web1" {
		t.Fatalf("outPath = %q", got)
	}
	want := call{"env", []string{
		"NIX_SSHOPTS=" + sshOpts,
		"nix", "--extra-experimental-features", "nix-command flakes",
		"build", "--print-out-paths", "--no-link",
		"--eval-store", "auto", "--store", "ssh-ng://root@203.0.113.10?compress=true",
		".#nixosConfigurations.web1.config.system.build.toplevel",
	}}
	if len(r.calls) != 1 || r.calls[0].name != want.name || !slices.Equal(r.calls[0].args, want.args) {
		t.Fatalf("calls = %+v, want %+v", r.calls, want)
	}
}

func TestBuildRemotePassesIdentity(t *testing.T) {
	r := &fakeRunner{stdout: "/nix/store/abc\n"}
	_, err := Nix{Runner: r}.BuildRemote(context.Background(), flake(t, ".#web1"), Remote{Host: "h", Identity: "/k/id"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].args[0]; got != "NIX_SSHOPTS="+sshOpts+" -i /k/id" {
		t.Fatalf("NIX_SSHOPTS arg = %q", got)
	}
}

func TestBuildRemoteSendsBuildOutputToLog(t *testing.T) {
	r := &fakeRunner{stdout: "/nix/store/abc\n", stderr: "building foo\n"}
	var log bytes.Buffer
	if _, err := (Nix{Runner: r}).BuildRemote(context.Background(), flake(t, ".#web1"), Remote{Host: "h"}, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "building foo") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestBuildRemoteRejectsNonStorePath(t *testing.T) {
	r := &fakeRunner{stdout: "garbage\n"}
	if _, err := (Nix{Runner: r}).BuildRemote(context.Background(), flake(t, ".#web1"), Remote{Host: "h"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error")
	}
}

func TestActivateRunsSwitchToConfigurationTest(t *testing.T) {
	r := &fakeRunner{}
	if err := (Nix{Runner: r}).Activate(context.Background(), Remote{Host: "h"}, "/nix/store/abc", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "root@h", "--",
		"/nix/store/abc/bin/switch-to-configuration test"}
	if len(r.calls) != 1 || r.calls[0].name != "ssh" || !slices.Equal(r.calls[0].args, want) {
		t.Fatalf("calls = %+v, want ssh %v", r.calls, want)
	}
}

func TestMakeBootDefaultSetsProfileThenBootloader(t *testing.T) {
	r := &fakeRunner{}
	if err := (Nix{Runner: r}).MakeBootDefault(context.Background(), Remote{Host: "h", Identity: "/k/id"}, "/nix/store/abc", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-i", "/k/id", "root@h", "--",
		"nix-env -p /nix/var/nix/profiles/system --set /nix/store/abc && /nix/store/abc/bin/switch-to-configuration boot"}
	if len(r.calls) != 1 || r.calls[0].name != "ssh" || !slices.Equal(r.calls[0].args, want) {
		t.Fatalf("calls = %+v, want ssh %v", r.calls, want)
	}
}
