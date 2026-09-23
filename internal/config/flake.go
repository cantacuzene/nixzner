package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// FlakeRef is a flake reference of the form <uri>#<host>, e.g. ".#web1",
// where <host> names an entry in the flake's nixosConfigurations.
type FlakeRef struct {
	URI  string
	Host string
}

func ParseFlake(s string) (FlakeRef, error) {
	uri, host, ok := strings.Cut(s, "#")
	if !ok || uri == "" || host == "" {
		return FlakeRef{}, fmt.Errorf("flake %q must have the form <uri>#<host>, e.g. .#web1", s)
	}
	if strings.Contains(host, `"`) {
		return FlakeRef{}, fmt.Errorf("flake host %q must not contain quotes", host)
	}
	return FlakeRef{URI: uri, Host: host}, nil
}

// ResolveLocalFlake replaces a local flake directory (a path starting with
// "." or "/", or a "path:" URI) with its real path. Some Nix versions refuse
// flake paths that pass through a symlink, such as macOS's /var. Other URIs
// are returned unchanged.
func ResolveLocalFlake(f FlakeRef) (FlakeRef, error) {
	prefix, rest := "", f.URI
	switch {
	case strings.HasPrefix(f.URI, "path:"):
		prefix, rest = "path:", strings.TrimPrefix(f.URI, "path:")
	case strings.HasPrefix(f.URI, "."), strings.HasPrefix(f.URI, "/"):
	default:
		return f, nil
	}
	dir, query, _ := strings.Cut(rest, "?")
	abs, err := filepath.Abs(dir)
	if err != nil {
		return FlakeRef{}, fmt.Errorf("flake path %q: %w", dir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return FlakeRef{}, fmt.Errorf("flake path %q: %w", dir, err)
	}
	f.URI = prefix + real
	if query != "" {
		f.URI += "?" + query
	}
	return f, nil
}

func (f FlakeRef) String() string { return f.URI + "#" + f.Host }

// ToplevelAttr is the installable for the host's system closure.
func (f FlakeRef) ToplevelAttr() string {
	host := f.Host
	if strings.Contains(host, ".") {
		host = `"` + host + `"`
	}
	return f.URI + "#nixosConfigurations." + host + ".config.system.build.toplevel"
}
