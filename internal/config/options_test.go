package config

import (
	"strings"
	"testing"
	"time"
)

func validOpts() CreateOptions {
	return CreateOptions{
		Name:       "web1",
		Flake:      FlakeRef{URI: ".", Host: "web1"},
		ServerType: "cx32",
		Location:   "fsn1",
		SSHKey:     "admin",
		Image:      "debian-12",
		LogDir:     "./nixzner-logs",
		Timeout:    30 * time.Minute,
	}
}

func TestValidateAcceptsValidOptions(t *testing.T) {
	if err := validOpts().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*CreateOptions)
		wantMsg string
	}{
		"uppercase name":   {func(o *CreateOptions) { o.Name = "Web1" }, "server name"},
		"empty name":       {func(o *CreateOptions) { o.Name = "" }, "server name"},
		"missing flake":    {func(o *CreateOptions) { o.Flake = FlakeRef{} }, "--flake"},
		"missing type":     {func(o *CreateOptions) { o.ServerType = "" }, "--type"},
		"missing location": {func(o *CreateOptions) { o.Location = "" }, "--location"},
		"missing ssh key":  {func(o *CreateOptions) { o.SSHKey = "" }, "--ssh-key"},
		"missing image":    {func(o *CreateOptions) { o.Image = "" }, "--image"},
		"missing log dir":  {func(o *CreateOptions) { o.LogDir = "" }, "--log-dir"},
		"zero timeout":     {func(o *CreateOptions) { o.Timeout = 0 }, "--timeout"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := validOpts()
			c.mutate(&o)
			err := o.Validate()
			if err == nil || !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantMsg)
			}
		})
	}
}
