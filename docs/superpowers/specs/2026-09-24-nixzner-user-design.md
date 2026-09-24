# nixzner `--user` — Design

Date: 2026-09-24. Extends `2026-09-22-nixzner-create-design.md` (create, rebuild).

## Purpose

`nixzner create` and `nixzner rebuild` gain `--user <name>`. With it, the installed
system has a login user `<name>` that holds the SSH keys, root SSH login is disabled,
and nixzner itself connects as `<name>` (with sudo) wherever it previously connected
to the running system as root. The user's flake is not edited: nixzner adds a NixOS
module on top of it for the run.

## Decisions

| Topic | Decision |
|---|---|
| Where the config comes from | nixzner injects a generated module via a generated wrapper flake (`extendModules`); the user's flake is unchanged |
| The user's SSH keys | Root's keys from the flake (`users.users.root.openssh.authorizedKeys` keys and keyFiles) are added to the user; keys the flake already gives the user also count |
| Privileges | Passwordless sudo for this user only (a `security.sudo.extraRules` entry), plus `nix.settings.trusted-users` so it can build over `ssh-ng` |
| Root login | `services.openssh.settings.PermitRootLogin = lib.mkForce "no"` |
| Lockout guard | Evaluation fails if the user ends up with no SSH keys |
| Remembering the user | Label `nixzner-user=<name>`; `rebuild` defaults `--user` from it |
| Without `--user` | Behaviour of both commands is unchanged |

## CLI

```
nixzner create  [name] ... [--user mp]
nixzner rebuild <name> --flake <uri>#<host> [--user mp]
```

- `--user` must match `^[a-z_][a-z0-9_-]{0,31}$` and must not be `root`.
- Interactive mode does not prompt for it.

## The wrapper flake

For each run with a user, nixzner writes two files to a fresh temp directory (its
real path, resolved through symlinks such as macOS `/var`) and deletes the directory
when the run ends:

`flake.nix`:

```nix
{
  inputs.target.url = "<the user's flake URI, after ResolveLocalFlake>";
  outputs = { target, ... }: {
    nixosConfigurations.<host> =
      target.nixosConfigurations.<host>.extendModules { modules = [ ./nixzner-user.nix ]; };
  };
}
```

`nixzner-user.nix` (`<u>` is the user name):

```nix
{ config, lib, ... }:
let
  rootKeys = config.users.users.root.openssh.authorizedKeys;
  userKeys = config.users.users.<u>.openssh.authorizedKeys;
in {
  users.users.<u> = {
    isNormalUser = true;
    openssh.authorizedKeys = { inherit (rootKeys) keys keyFiles; };
  };
  security.sudo.extraRules = [{ users = [ "<u>" ]; commands = [{ command = "ALL"; options = [ "NOPASSWD" ]; }]; }];
  nix.settings.trusted-users = [ "<u>" ];
  services.openssh.settings.PermitRootLogin = lib.mkForce "no";
  assertions = [{ assertion = userKeys.keys != [ ] || userKeys.keyFiles != [ ];
                  message = "nixzner --user: <u> has no SSH keys (the flake gives none to <u> or to root)"; }];
}
```

Every tool in the run (`nix eval`, nixos-anywhere, `nix build`) gets
`path:<tmpdir>#<host>`, so the evaluated, installed and verified system is the same.
The generated files depend only on `<u>`, so the same flake and user always give the
same system.

Verified before implementation (2026-09-24) by evaluating a hand-written wrapper
around `~/Code/mithril`:
- the wrapper's lock takes the target's pinned inputs (same nixpkgs rev);
- a plain local path works as `inputs.target.url`;
- a flake that already defines the user and gives it keys (and none to root)
  evaluates, with the flake's groups and keys kept;
- a flake with no keys for root or the user fails with the guard's message.

## `create --user <u>`

- Wait for SSH (Debian bootstrap) and install (nixos-anywhere) still use root: only
  root can log in to the bootstrap image and the kexec installer.
- The install uses the wrapper flake. Verify logs in as `<u>`, which proves the user's
  key login works on the booted system.
- Labels gain `nixzner-user=<u>`.

## `rebuild [--user <u>]`

- The target user is `--user`, or else the server's `nixzner-user` label, or else none.
- nixzner connects as the user currently allowed to log in: the label's user if the
  server has one, otherwise root.
- Build: `nix build --store ssh-ng://<login>@<ip>`. Activate and make-boot-default run
  their commands under `sudo` when the login user is not root.
- Verify logs in as the target user (root if none).
- Finalize sets `nixzner-user=<target>` when there is one.
- Converting a root-only server: `--user` given, no label → connect as root, activate
  the wrapped system (root login disabled from then on), verify as the new user, set
  the label.
- Refused before anything runs: `--user` differs from an existing `nixzner-user`
  label ("changing the user is not supported").
- No `--user` and no label: a plain rebuild as root, unchanged from today.

## Components

- `config`: `ValidateUser`, label constant `nixzner-user`.
- New `internal/flakewrap`: `Wrap(flake FlakeRef, user string) (wrapped FlakeRef, cleanup func(), error)`;
  returns the input unchanged (and a no-op cleanup) when `user` is empty.
- `nixos.Remote`: `User` (default root) used in the `ssh-ng://` store URL and in `ssh`
  commands; `sudo` prefixes the switch commands when the user is not root.
- `sshcheck.Checker`: `User` (default root) instead of the hard-coded root.
- `create`: wraps the flake; verify uses a checker for the user; sets the label.
- `rebuild`: resolves target and login users from the flag and label; wraps; refuses
  a user change.
- CLI: `--user` on both commands.

## Error handling

- Invalid `--user`: validation error before anything runs.
- Guard assertion: surfaces as a `nix eval` failure in preflight; nothing is created
  or changed.
- `create`: verify-as-user failure is an ordinary step failure → server deleted.
- `rebuild`: the existing per-step state messages apply; after a failed activation of a
  converted server, the rollback hint uses the login that still works (root until the
  new system is active).

## Testing

- Unit: generated files for a user (exact content), `Wrap` with no user is a no-op,
  cleanup removes the directory; `ValidateUser`; `ssh-ng://<user>@` and `sudo` in
  build/switch commands; checker user; create verifies as the user and labels it;
  rebuild target/login resolution (flag, label, none), refusal on a user change,
  conversion from root.
- Integration: `NIXZNER_TEST_USER` runs `TestCreateIntegration` with `--user`.
- Real: convert `mithril` with `nixzner rebuild mithril --flake .#mithril --user mp`;
  check `mp` logs in, root is refused, and the label is set.

## Out of scope

Changing or removing the user of an existing server; a password for the user;
prompting for `--user` in interactive mode.
