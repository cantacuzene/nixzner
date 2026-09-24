// Package create implements `nixzner create`: provision a Hetzner server,
// install NixOS with nixos-anywhere, verify the boot, and delete the server
// if anything fails.
package create

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"nixzner/internal/config"
	"nixzner/internal/flakewrap"
	"nixzner/internal/hetzner"
	"nixzner/internal/nixos"
	"nixzner/internal/pipeline"
	"nixzner/internal/runlog"
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
	VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error
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
	flake, cleanupWrap, err := flakewrap.Wrap(opts.Flake, opts.User)
	if err != nil {
		return Result{}, err
	}
	defer cleanupWrap()
	logFile, err := runlog.Start(d.TempDir)
	if err != nil {
		return Result{}, err
	}
	out := io.MultiWriter(d.Stderr, logFile)
	progress := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	var (
		res     Result
		outPath string
	)
	labels := func(status string) map[string]string {
		l := config.SystemLabels(opts.Labels, opts.Flake, status)
		if opts.User != "" {
			l[config.LabelUser] = opts.User
		}
		return l
	}
	req := hetzner.CreateRequest{
		Name: opts.Name, ServerType: opts.ServerType, Location: opts.Location, Image: opts.Image,
		SSHKey: opts.SSHKey, Firewalls: opts.Firewalls, Networks: opts.Networks,
		Labels: labels(config.StatusInstalling),
	}
	steps := []pipeline.Step{
		{Name: "preflight", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			if err := d.CheckTools(); err != nil {
				return nil, err
			}
			if err := d.Hetzner.Preflight(ctx, req); err != nil {
				return nil, err
			}
			p, err := d.Nix.Eval(ctx, flake)
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
			args := nixos.InstallArgs{Flake: flake, Host: res.Server.IPv4, Identity: opts.Identity}
			return nil, d.Nix.Install(ctx, args, out)
		}},
		{Name: "verify", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			ctx, cancel := context.WithTimeout(ctx, d.VerifyWait)
			defer cancel()
			return nil, d.SSH.VerifyNixOS(ctx, opts.User, res.Server.IPv4, outPath)
		}},
		{Name: "finalize", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			return nil, d.Hetzner.SetLabels(ctx, res.Server.ID, labels(config.StatusReady))
		}},
	}

	p := &pipeline.Pipeline{Progress: progress}
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	runErr := p.Run(runCtx, steps)
	if runErr == nil {
		logFile.Discard()
		res.StorePath = outPath
		return res, nil
	}

	progress("error: %v", runErr)
	if logPath, err := logFile.Persist(opts.LogDir, opts.Name, d.Now()); err != nil {
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
