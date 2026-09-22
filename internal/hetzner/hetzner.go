// Package hetzner is the slice of the Hetzner Cloud API hinfect needs.
package hetzner

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type Server struct {
	ID   int64
	Name string
	IPv4 string
	IPv6 string // the ::1 address of the server's /64
}

type CreateRequest struct {
	Name       string
	ServerType string
	Location   string
	Image      string // bootstrap image name, resolved for the server type's architecture
	SSHKey     string
	Firewalls  []string
	Networks   []string
	Labels     map[string]string
}

type Client struct {
	hc *hcloud.Client
}

func New(token string, opts ...hcloud.ClientOption) *Client {
	opts = append([]hcloud.ClientOption{hcloud.WithToken(token), hcloud.WithApplication("hinfect", "dev")}, opts...)
	return &Client{hc: hcloud.NewClient(opts...)}
}

type resolved struct {
	serverType *hcloud.ServerType
	location   *hcloud.Location
	image      *hcloud.Image
	sshKey     *hcloud.SSHKey
	firewalls  []*hcloud.Firewall
	networks   []*hcloud.Network
}

// resolve looks up every named resource and reports all missing ones.
func (c *Client) resolve(ctx context.Context, r CreateRequest) (*resolved, error) {
	var errs []error
	missing := func(kind, name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("looking up %s %q: %w", kind, name, err))
		} else {
			errs = append(errs, fmt.Errorf("%s %q not found", kind, name))
		}
	}
	res := &resolved{}
	var err error
	if res.serverType, _, err = c.hc.ServerType.GetByName(ctx, r.ServerType); res.serverType == nil {
		missing("server type", r.ServerType, err)
	}
	if res.location, _, err = c.hc.Location.GetByName(ctx, r.Location); res.location == nil {
		missing("location", r.Location, err)
	}
	if res.serverType != nil {
		arch := res.serverType.Architecture
		if res.image, _, err = c.hc.Image.GetForArchitecture(ctx, r.Image, arch); res.image == nil {
			missing(fmt.Sprintf("%s image", arch), r.Image, err)
		}
	}
	if res.sshKey, _, err = c.hc.SSHKey.GetByName(ctx, r.SSHKey); res.sshKey == nil {
		missing("ssh key", r.SSHKey, err)
	}
	for _, name := range r.Firewalls {
		fw, _, err := c.hc.Firewall.GetByName(ctx, name)
		if fw == nil {
			missing("firewall", name, err)
			continue
		}
		res.firewalls = append(res.firewalls, fw)
	}
	for _, name := range r.Networks {
		n, _, err := c.hc.Network.GetByName(ctx, name)
		if n == nil {
			missing("network", name, err)
			continue
		}
		res.networks = append(res.networks, n)
	}
	return res, errors.Join(errs...)
}

// Preflight checks that every referenced resource exists and the name is free.
func (c *Client) Preflight(ctx context.Context, r CreateRequest) error {
	_, err := c.resolve(ctx, r)
	existing, _, lookupErr := c.hc.Server.GetByName(ctx, r.Name)
	switch {
	case lookupErr != nil:
		err = errors.Join(err, fmt.Errorf("looking up server %q: %w", r.Name, lookupErr))
	case existing != nil:
		err = errors.Join(err, fmt.Errorf("server %q already exists (id %d)", r.Name, existing.ID))
	}
	return err
}

// CreateServer creates and starts the server. Whenever the server exists
// the returned Server has its ID set, even if an error is also returned.
func (c *Client) CreateServer(ctx context.Context, r CreateRequest) (Server, error) {
	res, err := c.resolve(ctx, r)
	if err != nil {
		return Server{}, err
	}
	opts := hcloud.ServerCreateOpts{
		Name:             r.Name,
		ServerType:       res.serverType,
		Image:            res.image,
		Location:         res.location,
		SSHKeys:          []*hcloud.SSHKey{res.sshKey},
		Networks:         res.networks,
		Labels:           r.Labels,
		StartAfterCreate: hcloud.Ptr(true),
	}
	for _, fw := range res.firewalls {
		opts.Firewalls = append(opts.Firewalls, &hcloud.ServerCreateFirewall{Firewall: *fw})
	}
	result, _, err := c.hc.Server.Create(ctx, opts)
	if err != nil {
		return Server{}, fmt.Errorf("creating server: %w", err)
	}
	srv := toServer(result.Server)
	actions := slices.DeleteFunc(append([]*hcloud.Action{result.Action}, result.NextActions...),
		func(a *hcloud.Action) bool { return a == nil })
	if err := c.hc.Action.WaitFor(ctx, actions...); err != nil {
		return srv, fmt.Errorf("waiting for server %d to start: %w", srv.ID, err)
	}
	return srv, nil
}

func toServer(s *hcloud.Server) Server {
	out := Server{ID: s.ID, Name: s.Name}
	if ip := s.PublicNet.IPv4.IP; ip != nil {
		out.IPv4 = ip.String()
	}
	if n := s.PublicNet.IPv6.Network; n != nil {
		ip := slices.Clone(n.IP.To16())
		ip[15] |= 1
		out.IPv6 = ip.String()
	}
	return out
}

// DeleteServer deletes the server and waits for it to be gone. A server
// that no longer exists counts as deleted.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	res, _, err := c.hc.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	if res.Action != nil {
		if err := c.hc.Action.WaitFor(ctx, res.Action); err != nil {
			return fmt.Errorf("waiting for server %d deletion: %w", id, err)
		}
	}
	return nil
}

// SetLabels replaces all of the server's labels.
func (c *Client) SetLabels(ctx context.Context, id int64, labels map[string]string) error {
	if _, _, err := c.hc.Server.Update(ctx, &hcloud.Server{ID: id}, hcloud.ServerUpdateOpts{Labels: labels}); err != nil {
		return fmt.Errorf("setting labels on server %d: %w", id, err)
	}
	return nil
}
