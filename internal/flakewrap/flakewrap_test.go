package flakewrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nixzner/internal/config"
)

func TestUserModule(t *testing.T) {
	m := UserModule("mp")
	for _, want := range []string{
		`users.users."mp" = {`,
		`isNormalUser = true;`,
		`openssh.authorizedKeys = { inherit (rootKeys) keys keyFiles; };`,
		`{ users = [ "mp" ]; commands = [ { command = "ALL"; options = [ "NOPASSWD" ]; } ]; }`,
		`nix.settings.trusted-users = [ "mp" ];`,
		`services.openssh.settings.PermitRootLogin = lib.mkForce "no";`,
		`assertion = userKeys.keys != [ ] || userKeys.keyFiles != [ ];`,
		`message = "nixzner --user: mp has no SSH keys (the flake gives none to mp or to root)";`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("module does not contain %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "@USER@") {
		t.Errorf("unreplaced placeholder:\n%s", m)
	}
}

func TestWrapperFlake(t *testing.T) {
	got := WrapperFlake("path:/src/infra", "web1")
	want := `{
  inputs.target.url = "path:/src/infra";
  outputs = { target, ... }: {
    nixosConfigurations."web1" =
      target.nixosConfigurations."web1".extendModules { modules = [ ./nixzner-user.nix ]; };
  };
}
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNixStringEscapes(t *testing.T) {
	if got, want := nixString(`a"b\c${d}`), `"a\"b\\c\${d}"`; got != want {
		t.Fatalf("nixString = %s, want %s", got, want)
	}
}

func TestWrapWithoutUserIsNoop(t *testing.T) {
	f := config.FlakeRef{URI: ".", Host: "web1"}
	got, cleanup, err := Wrap(f, "")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if got != f {
		t.Fatalf("got %+v, want %+v", got, f)
	}
}

func TestWrapWritesWrapperAndCleansUp(t *testing.T) {
	f := config.FlakeRef{URI: "path:/src/infra", Host: "web1"}
	got, cleanup, err := Wrap(f, "mp")
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := strings.CutPrefix(got.URI, "path:")
	if !ok || !filepath.IsAbs(dir) || got.Host != "web1" {
		t.Fatalf("wrapped = %+v", got)
	}
	if real, _ := filepath.EvalSymlinks(dir); real != dir {
		t.Errorf("wrapper dir %q is not a real path (%q)", dir, real)
	}
	flake, _ := os.ReadFile(filepath.Join(dir, "flake.nix"))
	module, _ := os.ReadFile(filepath.Join(dir, "nixzner-user.nix"))
	if string(flake) != WrapperFlake("path:/src/infra", "web1") || string(module) != UserModule("mp") {
		t.Errorf("files = %q / %q", flake, module)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("wrapper dir still exists after cleanup: %v", err)
	}
}

func TestWrapRejectsRelativeFlake(t *testing.T) {
	_, _, err := Wrap(config.FlakeRef{URI: ".", Host: "web1"}, "mp")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v", err)
	}
}
