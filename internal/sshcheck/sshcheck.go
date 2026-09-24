// Package sshcheck polls a freshly installed server over SSH.
package sshcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Backoff struct {
	Initial, Max time.Duration
}

var DefaultBackoff = Backoff{Initial: 2 * time.Second, Max: 15 * time.Second}

// attemptTimeout bounds one connect + command round trip.
const attemptTimeout = 30 * time.Second

// Checker connects as root. Host keys are not checked: the server was
// created seconds earlier by this run and its host key changes during
// install (accepted trust-on-first-use trade-off, see the design spec).
type Checker struct {
	Auth     []ssh.AuthMethod
	Port     int // 0 means 22
	Backoff  Backoff
	Progress func(format string, args ...any)
}

// WaitForSSH polls until root can log in and run a command.
func (c Checker) WaitForSSH(ctx context.Context, host string) error {
	return c.poll(ctx, "root", host, func(client *ssh.Client) error {
		_, err := run(client, "true")
		return err
	})
}

// VerifyNixOS polls until user ("" means root) can log in to a host running
// NixOS with /run/current-system pointing at wantOutPath. Polling matters:
// right after install the kexec installer (also NixOS) may still be
// answering SSH.
func (c Checker) VerifyNixOS(ctx context.Context, user, host, wantOutPath string) error {
	return c.poll(ctx, user, host, func(client *ssh.Client) error {
		out, err := run(client, "test -e /etc/NIXOS && readlink /run/current-system")
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(out); got != wantOutPath {
			return fmt.Errorf("running system is %q, want %q", got, wantOutPath)
		}
		return nil
	})
}

func (c Checker) poll(ctx context.Context, user, host string, check func(*ssh.Client) error) error {
	delay := c.Backoff.Initial
	var last error
	for {
		err := c.attempt(ctx, user, host, check)
		if err == nil {
			return nil
		}
		// An attempt cut short by ctx fails with a meaningless connection
		// error; report the last attempt that ran to completion instead.
		if ctx.Err() == nil || last == nil {
			last = err
		}
		c.progress("    %s not ready: %v (retrying in %s)", host, err, delay)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
		case <-time.After(delay):
		}
		delay = min(delay*2, c.Backoff.Max)
	}
}

func (c Checker) attempt(ctx context.Context, user, host string, check func(*ssh.Client) error) error {
	if user == "" {
		user = "root"
	}
	port := c.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(attemptTimeout))
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            c.Auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	return check(client)
}

func run(client *ssh.Client, cmd string) (string, error) {
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	out, err := s.Output(cmd)
	if err != nil {
		return "", fmt.Errorf("%q: %w", cmd, err)
	}
	return string(out), nil
}

func (c Checker) progress(format string, args ...any) {
	if c.Progress != nil {
		c.Progress(format, args...)
	}
}

// AuthMethods loads the private key at identity, or, when identity is
// empty, uses the ssh-agent listening on agentSock. Call the returned
// func to release the agent connection.
func AuthMethods(identity, agentSock string) ([]ssh.AuthMethod, func() error, error) {
	if identity != "" {
		data, err := os.ReadFile(identity)
		if err != nil {
			return nil, nil, fmt.Errorf("reading identity: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(data)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, nil, fmt.Errorf("%s is passphrase-protected: add it to ssh-agent and omit --identity", identity)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parsing identity %s: %w", identity, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, func() error { return nil }, nil
	}
	if agentSock == "" {
		return nil, nil, errors.New("no --identity given and SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", agentSock)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to ssh-agent: %w", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(conn).Signers)}, conn.Close, nil
}
