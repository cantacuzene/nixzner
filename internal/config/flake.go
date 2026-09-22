package config

import (
	"fmt"
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

func (f FlakeRef) String() string { return f.URI + "#" + f.Host }

// ToplevelAttr is the installable for the host's system closure.
func (f FlakeRef) ToplevelAttr() string {
	host := f.Host
	if strings.Contains(host, ".") {
		host = `"` + host + `"`
	}
	return f.URI + "#nixosConfigurations." + host + ".config.system.build.toplevel"
}
