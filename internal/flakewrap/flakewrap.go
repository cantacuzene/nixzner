// Package flakewrap adds nixzner's login-user module on top of a flake's
// host without editing the flake, by generating a wrapper flake.
package flakewrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nixzner/internal/config"
)

const moduleTemplate = `{ config, lib, ... }:
let
  rootKeys = config.users.users.root.openssh.authorizedKeys;
  userKeys = config.users.users."@USER@".openssh.authorizedKeys;
in
{
  users.users."@USER@" = {
    isNormalUser = true;
    openssh.authorizedKeys = { inherit (rootKeys) keys keyFiles; };
  };
  security.sudo.extraRules = [
    { users = [ "@USER@" ]; commands = [ { command = "ALL"; options = [ "NOPASSWD" ]; } ]; }
  ];
  nix.settings.trusted-users = [ "@USER@" ];
  services.openssh.settings.PermitRootLogin = lib.mkForce "no";
  assertions = [
    {
      assertion = userKeys.keys != [ ] || userKeys.keyFiles != [ ];
      message = "nixzner --user: @USER@ has no SSH keys (the flake gives none to @USER@ or to root)";
    }
  ];
}
`

// UserModule is the NixOS module that creates user with the SSH keys the
// flake gives root, lets it sudo and use nix without a password, and
// disables root SSH login. user must pass config.ValidateUser.
func UserModule(user string) string {
	return strings.ReplaceAll(moduleTemplate, "@USER@", user)
}

// WrapperFlake is a flake whose nixosConfigurations.<host> is target's,
// extended with ./nixzner-user.nix.
func WrapperFlake(targetURI, host string) string {
	h := nixString(host)
	return fmt.Sprintf(`{
  inputs.target.url = %s;
  outputs = { target, ... }: {
    nixosConfigurations.%s =
      target.nixosConfigurations.%s.extendModules { modules = [ ./nixzner-user.nix ]; };
  };
}
`, nixString(targetURI), h, h)
}

// nixString quotes s as a Nix string literal.
func nixString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `${`, `\${`)
	return `"` + r.Replace(s) + `"`
}

// Wrap returns a flake reference with user's module added to f's host, and
// a cleanup that deletes the generated files. With no user it returns f.
// f.URI must not be relative: inside the wrapper "." would mean the
// wrapper's own directory.
func Wrap(f config.FlakeRef, user string) (config.FlakeRef, func(), error) {
	if user == "" {
		return f, func() {}, nil
	}
	if strings.HasPrefix(f.URI, ".") {
		return config.FlakeRef{}, nil, fmt.Errorf("flakewrap: flake %q must be resolved to an absolute path first", f.URI)
	}
	tmp, err := os.MkdirTemp("", "nixzner-wrap-*")
	if err != nil {
		return config.FlakeRef{}, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	dir, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		cleanup()
		return config.FlakeRef{}, nil, err
	}
	files := map[string]string{
		"flake.nix":        WrapperFlake(f.URI, f.Host),
		"nixzner-user.nix": UserModule(user),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			cleanup()
			return config.FlakeRef{}, nil, err
		}
	}
	return config.FlakeRef{URI: "path:" + dir, Host: f.Host}, cleanup, nil
}
