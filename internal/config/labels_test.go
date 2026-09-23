package config

import (
	"maps"
	"strings"
	"testing"
)

func TestValidLabelKey(t *testing.T) {
	cases := map[string]bool{
		"env":                   true,
		"example.com/team":      true,
		"a.b_c-d":               true,
		"":                      false,
		"-bad":                  false,
		"bad-":                  false,
		"has space":             false,
		"a/b/c":                 false,
		"UPPER.com/x":           false,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
	}
	for k, want := range cases {
		if got := ValidLabelKey(k); got != want {
			t.Errorf("ValidLabelKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestValidLabelValue(t *testing.T) {
	cases := map[string]bool{"": true, "prod": true, "v1.2_3-x": true, "has space": false, "-x": false, "a/b": false}
	for v, want := range cases {
		if got := ValidLabelValue(v); got != want {
			t.Errorf("ValidLabelValue(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := map[string]string{
		".#web1":                "web1",
		"github:org/infra#db-1": "github-org-infra-db-1",
		"path:/tmp/x#example":   "path-tmp-x-example",
	}
	for in, want := range cases {
		if got := SanitizeLabelValue(in); got != want {
			t.Errorf("SanitizeLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeLabelValue(strings.Repeat("a", 62) + "-#x")
	if len(long) > 63 || !ValidLabelValue(long) {
		t.Errorf("long value %q is not a valid label value", long)
	}
}

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels([]string{"env=prod", "example.com/team=infra", "empty="})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "prod", "example.com/team": "infra", "empty": ""}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	bad := map[string]string{
		"noequals":         "key=value",
		"bad key=x":        "invalid label key",
		"env=bad value":    "invalid label value",
		"managed-by=me":    "reserved",
		"nixzner-status=x": "reserved",
	}
	for in, wantMsg := range bad {
		_, err := ParseLabels([]string{in})
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("ParseLabels(%q) error = %v, want containing %q", in, err, wantMsg)
		}
	}
}

func TestSystemLabels(t *testing.T) {
	user := map[string]string{"env": "prod"}
	flake, _ := ParseFlake(".#web1")
	got := SystemLabels(user, flake, StatusInstalling)
	want := map[string]string{
		"env":            "prod",
		"managed-by":     "nixzner",
		"nixzner-flake":  "web1",
		"nixzner-status": "installing",
	}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if len(user) != 1 {
		t.Errorf("SystemLabels mutated its input: %v", user)
	}
}
