// Package rebuild implements `nixzner rebuild`: build a flake's system on an
// existing nixzner server, switch to it, and verify it is running. Unlike
// create, a failure never deletes the server.
package rebuild

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"nixzner/internal/config"
	"nixzner/internal/flakewrap"
	"nixzner/internal/hetzner"
	"nixzner/internal/nixos"
	"nixzner/internal/pipeline"
	"nixzner/internal/runlog"
)

type Hetzner interface {
	FindServer(ctx context.Context, name string) (hetzner.Server, map[string]string, error)
	SetLabels(ctx context.Context, id int64, labels map[string]string) error
}

type Nix interface {
	Eval(ctx context.Context, f config.FlakeRef) (string, error)
	BuildRemote(ctx context.Context, f config.FlakeRef, r nixos.Remote, log io.Writer) (string, error)
	Activate(ctx context.Context, r nixos.Remote, outPath string, log io.Writer) error
	MakeBootDefault(ctx context.Context, r nixos.Remote, outPath string, log io.Writer) error
}

type SSH interface {
	VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error
}

type Options struct {
	Name     string
	Flake    config.FlakeRef
	Identity string // private key file; empty means use ssh-agent
	User     string // login user to set up; "" means the server's nixzner-user label, if any
	LogDir   string
	Timeout  time.Duration
}

func (o Options) validate() error {
	var errs []error
	if err := config.ValidateServerName(o.Name); err != nil {
		errs = append(errs, err)
	}
	if o.User != "" {
		if err := config.ValidateUser(o.User); err != nil {
			errs = append(errs, err)
		}
	}
	if o.Flake.Host == "" {
		errs = append(errs, errors.New("--flake is required"))
	}
	if o.LogDir == "" {
		errs = append(errs, errors.New("--log-dir is required"))
	}
	if o.Timeout <= 0 {
		errs = append(errs, errors.New("--timeout must be positive"))
	}
	return errors.Join(errs...)
}

type Deps struct {
	Hetzner    Hetzner
	Nix        Nix
	SSH        SSH
	CheckTools func() error
	Stderr     io.Writer
	Now        func() time.Time
	TempDir    string // where the in-progress log lives; "" means os.TempDir()
	VerifyWait time.Duration
}

type Result struct {
	Server    hetzner.Server
	StorePath string
}

func Run(ctx context.Context, opts Options, d Deps) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	log, err := runlog.Start(d.TempDir)
	if err != nil {
		return Result{}, err
	}
	out := io.MultiWriter(d.Stderr, log)
	progress := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	cleanupWrap := func() {}
	defer func() { cleanupWrap() }()
	var (
		res    Result
		labels map[string]string
		remote nixos.Remote
		flake  config.FlakeRef
		target string
		// state describes the server if the current step fails.
		state = "nothing on the server was changed"
	)
	steps := []pipeline.Step{
		{Name: "preflight", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			if err := d.CheckTools(); err != nil {
				return nil, err
			}
			srv, l, err := d.Hetzner.FindServer(ctx, opts.Name)
			if err != nil {
				return nil, err
			}
			if l[config.LabelManagedBy] != config.ManagedByValue {
				return nil, fmt.Errorf("server %q is not managed by nixzner (no %s=%s label)", opts.Name, config.LabelManagedBy, config.ManagedByValue)
			}
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
		}},
		{Name: "build", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			built, err := d.Nix.BuildRemote(ctx, flake, remote, out)
			if err != nil {
				return nil, err
			}
			if built != res.StorePath {
				return nil, fmt.Errorf("built %s, but the flake evaluates to %s", built, res.StorePath)
			}
			return nil, nil
		}},
		{Name: "activate", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			login, sudo := "root", ""
			if remote.User != "" {
				login, sudo = remote.User, "sudo "
			}
			state = fmt.Sprintf("the previous system is still the boot default; to return to it now, run:\n    ssh %s@%s %s/nix/var/nix/profiles/system/bin/switch-to-configuration test", login, remote.Host, sudo)
			return nil, d.Nix.Activate(ctx, remote, res.StorePath, out)
		}},
		{Name: "make boot default", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			state = "the new system is running, but may not be the boot default; a reboot may return to the previous system"
			return nil, d.Nix.MakeBootDefault(ctx, remote, res.StorePath, out)
		}},
		{Name: "verify", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			state = "the new system was activated and made the boot default, but is not what the server reports as running"
			ctx, cancel := context.WithTimeout(ctx, d.VerifyWait)
			defer cancel()
			return nil, d.SSH.VerifyNixOS(ctx, target, remote.Host, res.StorePath)
		}},
		{Name: "finalize", Run: func(ctx context.Context) (pipeline.Cleanup, error) {
			state = "the server runs the new system; only updating its labels failed"
			l := maps.Clone(labels)
			l[config.LabelFlake] = config.FlakeLabel(opts.Flake)
			if target != "" {
				l[config.LabelUser] = target
			}
			return nil, d.Hetzner.SetLabels(ctx, res.Server.ID, l)
		}},
	}

	p := &pipeline.Pipeline{Progress: progress}
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	runErr := p.Run(runCtx, steps)
	if runErr == nil {
		log.Discard()
		return res, nil
	}

	progress("error: %v", runErr)
	progress("%s", state)
	if logPath, err := log.Persist(opts.LogDir, opts.Name, d.Now()); err != nil {
		fmt.Fprintf(d.Stderr, "warning: could not save log: %v (temporary log: %s)\n", err, log.Name())
	} else {
		fmt.Fprintf(d.Stderr, "log saved to %s\n", logPath)
	}
	return res, runErr
}
