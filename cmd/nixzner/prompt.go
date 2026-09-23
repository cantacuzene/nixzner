package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"nixzner/internal/config"
	"nixzner/internal/hetzner"
)

var errAborted = errors.New("aborted: nothing was created")

type option struct {
	Label string
	Value string
}

// prompter asks the user for one value at a time.
type prompter interface {
	Input(title, initial string, validate func(string) error) (string, error)
	Select(title string, options []option) (string, error)
	Confirm(title, summary string) (bool, error)
}

// catalog is what the Hetzner project offers to choose from.
type catalog interface {
	Locations(ctx context.Context) ([]hetzner.Location, error)
	ServerTypesInStock(ctx context.Context, location string) ([]hetzner.ServerTypeOffer, error)
	SSHKeyNames(ctx context.Context) ([]string, error)
}

// answers are the create options interactive mode can ask for.
type answers struct {
	Name       string
	Location   string
	ServerType string
	SSHKey     string
	Flake      string
}

func (a answers) complete() bool {
	return a.Name != "" && a.Location != "" && a.ServerType != "" && a.SSHKey != "" && a.Flake != ""
}

// promptMissing asks for every empty answer, then for confirmation. It asks
// nothing when a is already complete.
func promptMissing(ctx context.Context, p prompter, cat catalog, a *answers, stderr io.Writer) error {
	if a.complete() {
		return nil
	}
	var err error
	if a.Name == "" {
		if a.Name, err = p.Input("Server name", "", config.ValidateServerName); err != nil {
			return err
		}
	}
	if err := promptPlacement(ctx, p, cat, a, stderr); err != nil {
		return err
	}
	if a.SSHKey == "" {
		keys, err := cat.SSHKeyNames(ctx)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return errors.New("the Hetzner project has no SSH keys; add one in the Cloud Console or with `hcloud ssh-key create`")
		}
		var opts []option
		for _, k := range keys {
			opts = append(opts, option{Label: k, Value: k})
		}
		if a.SSHKey, err = p.Select("SSH key", opts); err != nil {
			return err
		}
	}
	if a.Flake == "" {
		validate := func(s string) error { _, err := config.ParseFlake(s); return err }
		if a.Flake, err = p.Input("Flake", ".#"+a.Name, validate); err != nil {
			return err
		}
	}
	summary := fmt.Sprintf("name:      %s\nlocation:  %s\ntype:      %s\nssh key:   %s\nflake:     %s",
		a.Name, a.Location, a.ServerType, a.SSHKey, a.Flake)
	ok, err := p.Confirm("Create this server?", summary)
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	return nil
}

// promptPlacement asks for the location, then for a server type in stock
// there, asking for another location while the chosen one has no stock.
func promptPlacement(ctx context.Context, p prompter, cat catalog, a *answers, stderr io.Writer) error {
	askLocation := a.Location == ""
	for {
		if askLocation {
			locs, err := cat.Locations(ctx)
			if err != nil {
				return err
			}
			var opts []option
			for _, l := range locs {
				opts = append(opts, option{Label: fmt.Sprintf("%-6s %s, %s", l.Name, l.City, l.Country), Value: l.Name})
			}
			if a.Location, err = p.Select("Location", opts); err != nil {
				return err
			}
		}
		if a.ServerType != "" {
			return nil
		}
		offers, err := cat.ServerTypesInStock(ctx, a.Location)
		if err != nil {
			return err
		}
		if len(offers) == 0 {
			if !askLocation {
				return fmt.Errorf("no server type is in stock in %s", a.Location)
			}
			fmt.Fprintf(stderr, "no server type is in stock in %s; pick another location\n", a.Location)
			continue
		}
		var opts []option
		for _, o := range offers {
			label := fmt.Sprintf("%-8s %-4s %2d vCPU  %3.0f GB  €%.4f/h", o.Name, o.Architecture, o.Cores, o.MemoryGB, o.HourlyGross)
			opts = append(opts, option{Label: label, Value: o.Name})
		}
		a.ServerType, err = p.Select("Server type", opts)
		return err
	}
}
