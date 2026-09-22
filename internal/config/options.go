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
	for _, f := range []struct{ flag, value string }{
		{"--type", o.ServerType}, {"--location", o.Location}, {"--ssh-key", o.SSHKey},
		{"--image", o.Image}, {"--log-dir", o.LogDir},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.flag))
		}
	}
	if o.Timeout <= 0 {
		errs = append(errs, errors.New("--timeout must be positive"))
	}
	return errors.Join(errs...)
}
