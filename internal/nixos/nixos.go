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

	"nixzner/internal/config"
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

// sshOptions trusts a host key on first contact and refuses a changed one:
// unlike during install, a running server's host key should be stable.
func (r Remote) sshOptions() []string {
	opts := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if r.Identity != "" {
		opts = append(opts, "-i", r.Identity)
	}
	return opts
}

// BuildRemote builds the host's system closure on the server itself (the
// local machine may not be able to build Linux derivations) and returns its
// store path. Build output goes to log.
func (n Nix) BuildRemote(ctx context.Context, f config.FlakeRef, r Remote, log io.Writer) (string, error) {
	args := append([]string{"NIX_SSHOPTS=" + strings.Join(r.sshOptions(), " "), "nix"}, nixFlags...)
	args = append(args, "build", "--print-out-paths", "--no-link",
		"--eval-store", "auto", "--store", "ssh-ng://"+r.login()+"@"+r.Host+"?compress=true",
		f.ToplevelAttr())
	var out bytes.Buffer
	if err := n.Runner.Run(ctx, "env", args, &out, log); err != nil {
		return "", fmt.Errorf("nix build on %s: %w", r.Host, err)
	}
	path := strings.TrimSpace(out.String())
	if !strings.HasPrefix(path, "/nix/store/") {
		return "", fmt.Errorf("nix build on %s: unexpected output %q", r.Host, path)
	}
	return path, nil
}

// Activate switches the running system to outPath without touching the
// bootloader, so a reboot still returns to the previous generation.
func (n Nix) Activate(ctx context.Context, r Remote, outPath string, log io.Writer) error {
	return n.ssh(ctx, r, r.sudo()+outPath+"/bin/switch-to-configuration test", log)
}

// MakeBootDefault records outPath as the newest system generation and
// installs it as the bootloader default.
func (n Nix) MakeBootDefault(ctx context.Context, r Remote, outPath string, log io.Writer) error {
	return n.ssh(ctx, r, r.sudo()+"nix-env -p /nix/var/nix/profiles/system --set "+outPath+" && "+r.sudo()+outPath+"/bin/switch-to-configuration boot", log)
}

func (n Nix) ssh(ctx context.Context, r Remote, command string, log io.Writer) error {
	args := append(r.sshOptions(), r.login()+"@"+r.Host, "--", command)
	if err := n.Runner.Run(ctx, "ssh", args, log, log); err != nil {
		return fmt.Errorf("ssh %s@%s %q: %w", r.login(), r.Host, command, err)
	}
	return nil
}

// CheckTools verifies that each tool is on PATH.
func CheckTools(lookPath func(string) (string, error), tools ...string) error {
	var errs []error
	for _, tool := range tools {
		if _, err := lookPath(tool); err != nil {
			errs = append(errs, fmt.Errorf("%s not found on PATH", tool))
		}
	}
	return errors.Join(errs...)
}
