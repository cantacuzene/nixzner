package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFlake(t *testing.T) {
	cases := []struct {
		in      string
		want    FlakeRef
		wantErr bool
	}{
		{in: ".#web1", want: FlakeRef{URI: ".", Host: "web1"}},
		{in: "github:org/infra#db-1", want: FlakeRef{URI: "github:org/infra", Host: "db-1"}},
		{in: "path:/tmp/x#example", want: FlakeRef{URI: "path:/tmp/x", Host: "example"}},
		{in: ".", wantErr: true},
		{in: "#web1", wantErr: true},
		{in: ".#", wantErr: true},
		{in: `.#we"b`, wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseFlake(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseFlake(%q): want error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFlake(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseFlake(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if got.String() != c.in {
			t.Errorf("String() = %q, want %q", got.String(), c.in)
		}
	}
}

func TestToplevelAttr(t *testing.T) {
	cases := map[string]string{
		".#web1":             ".#nixosConfigurations.web1.config.system.build.toplevel",
		".#web1.example.com": `.#nixosConfigurations."web1.example.com".config.system.build.toplevel`,
	}
	for in, want := range cases {
		f, err := ParseFlake(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.ToplevelAttr(); got != want {
			t.Errorf("ToplevelAttr(%q) = %q, want %q", in, got, want)
		}
	}
}

// symlinkedDir returns a symlink and the real directory it points at. On
// macOS the temp dir itself sits behind the /var -> /private/var symlink.
func symlinkedDir(t *testing.T) (link, real string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real = filepath.Join(base, "real")
	link = filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return link, real
}

func TestResolveLocalFlake(t *testing.T) {
	link, real := symlinkedDir(t)
	cases := []struct{ in, want string }{
		{in: "path:" + link, want: "path:" + real},
		{in: "path:" + link + "?dir=sub", want: "path:" + real + "?dir=sub"},
		{in: link, want: real},
		{in: "github:org/infra", want: "github:org/infra"},
		{in: "nixpkgs", want: "nixpkgs"},
		{in: "git+file:" + link, want: "git+file:" + link},
	}
	for _, c := range cases {
		got, err := ResolveLocalFlake(FlakeRef{URI: c.in, Host: "web1"})
		if err != nil {
			t.Errorf("ResolveLocalFlake(%q): %v", c.in, err)
			continue
		}
		if want := (FlakeRef{URI: c.want, Host: "web1"}); got != want {
			t.Errorf("ResolveLocalFlake(%q) = %+v, want %+v", c.in, got, want)
		}
	}
}

func TestResolveLocalFlakeRelative(t *testing.T) {
	link, real := symlinkedDir(t)
	t.Chdir(filepath.Dir(link))
	got, err := ResolveLocalFlake(FlakeRef{URI: "./link", Host: "web1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URI != real {
		t.Errorf("URI = %q, want %q", got.URI, real)
	}
	t.Chdir(link)
	got, err = ResolveLocalFlake(FlakeRef{URI: ".", Host: "web1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URI != real {
		t.Errorf(". resolved to %q, want %q", got.URI, real)
	}
}

func TestResolveLocalFlakeMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := ResolveLocalFlake(FlakeRef{URI: "path:" + missing, Host: "web1"}); err == nil {
		t.Fatal("want error for a missing flake directory")
	}
}
