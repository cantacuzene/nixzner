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
