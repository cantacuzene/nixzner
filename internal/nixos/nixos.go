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
