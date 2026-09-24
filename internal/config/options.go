package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// CreateOptions is everything `nixzner create` needs, already parsed.
type CreateOptions struct {
	Name       string
	Flake      FlakeRef
	ServerType string
	Location   string
	SSHKey     string
	Identity   string // private key file; empty means use ssh-agent
	User       string // login user to create; empty keeps root login
	Image      string
	Firewalls  []string
	Networks   []string
	Labels     map[string]string
	LogDir     string
	Timeout    time.Duration
}

var serverNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateServerName checks that name is usable as a server's hostname.
func ValidateServerName(name string) error {
	if !serverNameRe.MatchString(name) {
		return fmt.Errorf("server name %q must be a lowercase hostname label (a-z, 0-9, -; at most 63 chars)", name)
	}
	return nil
}

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidateUser checks name is usable as the login user nixzner creates.
func ValidateUser(name string) error {
	if name == "root" {
		return errors.New(`--user must not be "root" (leave it out to keep root login)`)
	}
	if !userRe.MatchString(name) {
		return fmt.Errorf("--user %q must be a Linux user name (a-z, 0-9, _, -; starts with a letter or _; at most 32 chars)", name)
	}
	return nil
}

// Validate checks the options without any I/O and reports every problem.
func (o CreateOptions) Validate() error {
	var errs []error
	if err := ValidateServerName(o.Name); err != nil {
		errs = append(errs, err)
	}
	if o.User != "" {
		if err := ValidateUser(o.User); err != nil {
			errs = append(errs, err)
		}
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
