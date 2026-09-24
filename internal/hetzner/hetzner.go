// Package hetzner is the slice of the Hetzner Cloud API nixzner needs.
package hetzner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	opts = append([]hcloud.ClientOption{hcloud.WithToken(token), hcloud.WithApplication("nixzner", "dev")}, opts...)
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

// Preflight checks that every referenced resource exists, the server type
// is in stock in the location, and the name is free.
func (c *Client) Preflight(ctx context.Context, r CreateRequest) error {
	res, err := c.resolve(ctx, r)
	if res.serverType != nil && res.location != nil {
		err = errors.Join(err, checkStock(res.serverType, res.location.Name))
	}
	existing, _, lookupErr := c.hc.Server.GetByName(ctx, r.Name)
	switch {
	case lookupErr != nil:
		err = errors.Join(err, fmt.Errorf("looking up server %q: %w", r.Name, lookupErr))
	case existing != nil:
		err = errors.Join(err, fmt.Errorf("server %q already exists (id %d)", r.Name, existing.ID))
	}
	return err
}

type Location struct {
	Name    string
	City    string
	Country string
}

// Locations lists every Hetzner location, sorted by name.
func (c *Client) Locations(ctx context.Context) ([]Location, error) {
	all, err := c.hc.Location.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing locations: %w", err)
	}
	out := make([]Location, 0, len(all))
	for _, l := range all {
		out = append(out, Location{Name: l.Name, City: l.City, Country: l.Country})
	}
	slices.SortFunc(out, func(a, b Location) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type ServerTypeOffer struct {
	Name         string
	Architecture string
	Cores        int
	MemoryGB     float32
	HourlyGross  float64 // EUR, in the queried location
}

// ServerTypesInStock lists the server types that can be ordered in location
// right now, cheapest first. Types deprecated there are left out.
func (c *Client) ServerTypesInStock(ctx context.Context, location string) ([]ServerTypeOffer, error) {
	all, err := c.hc.ServerType.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing server types: %w", err)
	}
	var out []ServerTypeOffer
	for _, st := range all {
		i := slices.IndexFunc(st.Locations, func(l hcloud.ServerTypeLocation) bool {
			return l.Location != nil && l.Location.Name == location
		})
		if i < 0 || !st.Locations[i].Available || st.Locations[i].IsDeprecated() {
			continue
		}
		offer := ServerTypeOffer{Name: st.Name, Architecture: string(st.Architecture), Cores: st.Cores, MemoryGB: st.Memory}
		for _, p := range st.Pricings {
			if p.Location != nil && p.Location.Name == location {
				offer.HourlyGross, _ = strconv.ParseFloat(p.Hourly.Gross, 64)
			}
		}
		out = append(out, offer)
	}
	slices.SortFunc(out, func(a, b ServerTypeOffer) int {
		return cmp.Or(cmp.Compare(a.HourlyGross, b.HourlyGross), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

// SSHKeyNames lists the names of the project's SSH keys, sorted.
func (c *Client) SSHKeyNames(ctx context.Context) ([]string, error) {
	all, err := c.hc.SSHKey.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing ssh keys: %w", err)
	}
	out := make([]string, 0, len(all))
	for _, k := range all {
		out = append(out, k.Name)
	}
	slices.Sort(out)
	return out, nil
}

// checkStock reports when the server type cannot be ordered in location,
// naming the locations where it can. A server type without per-location
// data is assumed orderable.
func checkStock(st *hcloud.ServerType, location string) error {
	if len(st.Locations) == 0 {
		return nil
	}
	offered, inStock := false, []string{}
	for _, l := range st.Locations {
		if l.Location == nil {
			continue
		}
		if l.Location.Name == location {
			if l.Available {
				return nil
			}
			offered = true
		}
		if l.Available {
			inStock = append(inStock, l.Location.Name)
		}
	}
	problem := "is not offered in"
	if offered {
		problem = "is out of stock in"
	}
	alternatives := "not in stock in any location"
	if len(inStock) > 0 {
		slices.Sort(inStock)
		alternatives = "in stock in: " + strings.Join(inStock, ", ")
	}
	return fmt.Errorf("server type %q %s %s (%s)", st.Name, problem, location, alternatives)
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

// FindServer looks up an existing server by name and returns its labels.
func (c *Client) FindServer(ctx context.Context, name string) (Server, map[string]string, error) {
	s, _, err := c.hc.Server.GetByName(ctx, name)
	if err != nil {
		return Server{}, nil, fmt.Errorf("looking up server %q: %w", name, err)
	}
	if s == nil {
		return Server{}, nil, fmt.Errorf("server %q not found", name)
	}
	return toServer(s), s.Labels, nil
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
