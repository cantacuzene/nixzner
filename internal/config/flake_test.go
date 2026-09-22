package config

import "testing"

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
