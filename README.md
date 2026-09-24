# nixzner

Create a Hetzner Cloud server and install NixOS on it from your flake, in one command.

`nixzner create` creates the server from a stock Debian image, runs
[nixos-anywhere](https://github.com/nix-community/nixos-anywhere) against it
(kexec → disko → install, building on the server), and verifies that the
server boots into exactly the system your flake describes. If anything
fails — or you press Ctrl-C — the server is deleted and the full log is kept
in `./nixzner-logs/`.

To change a server later, edit its flake and run `nixzner rebuild` (below),
or use `nixos-rebuild --target-host`, deploy-rs, or colmena.

## Requirements

- Nix with flakes, and a nixos-anywhere release that supports `--build-on` on your `PATH`
  (`nix profile install github:nix-community/nixos-anywhere`).
- A Hetzner Cloud API token in `HCLOUD_TOKEN`.
- An SSH key registered in the Hetzner project, whose private half is in
  your ssh-agent (or passed with `--identity`).
- A flake whose `nixosConfigurations.<host>`:
  - has a disko config for `/dev/sda`,
  - sets `nixpkgs.hostPlatform` to match the server type (`cax*` is `aarch64-linux`, the rest `x86_64-linux`),
  - authorizes that same public key for root. **Otherwise nixzner cannot verify the install and will delete the server.**

`examples/flake/` is a known-good starting point. Put your public key in its
`authorized_keys` file (and `git add` it — flakes only see tracked files).

Building nixzner itself needs Go 1.26 or newer: `go build ./cmd/nixzner`.

## Usage

```sh
export HCLOUD_TOKEN=...
nixzner create web1 \
  --flake .#web1 \
  --type cx32 \
  --location fsn1 \
  --ssh-key admin \
  --firewall web \
  --label env=prod
```

Or run `nixzner create` on its own in a terminal: it asks for the name,
then a location, a server type in stock there (cheapest first), one of the
project's SSH keys, and the flake (default `.#<name>`), shows a summary and
asks before creating anything. Flags you pass are not asked for, so
`nixzner create web1 --flake .#web1` only asks where, what size, and which key.
Outside a terminal (scripts, CI) missing values are errors instead.

Before anything is created, nixzner checks that every named resource exists
and that the server type is in stock in the location; if it is not, the error
names the locations where it is.

On success, stdout gets one line: `<name> <ipv4> <ipv6> <store-path>`.
Progress goes to stderr.

| Exit code | Meaning |
|---|---|
| 0 | Server created and verified |
| 1 | Failed; nothing left behind |
| 2 | Failed **and** the server could not be deleted — it is still billed; the command to delete it is printed |

Servers get the labels `managed-by=nixzner`, `nixzner-flake=<host>` (the flake's `#<host>` part), and
`nixzner-status=installing|ready`.

## Changing a server: `nixzner rebuild`

```sh
nixzner rebuild web1 --flake .#web1
```

Looks up `web1` (only servers labelled `managed-by=nixzner`), builds the
flake's system **on the server** (your machine does not need to build Linux),
activates it, and only if activation succeeds makes it the boot default; then
checks the server runs exactly that system. Nothing is ever deleted: if the
build fails nothing changed; if activation fails the previous system is still
the boot default and the error prints the command to switch back. On a
terminal `--flake` is asked for when missing (default `.#<name>`).

Unlike `create`, rebuild checks the server's SSH host key: it is trusted the
first time and a changed key is refused. Some settings, like
`networking.hostName`, only fully apply after a reboot, as with
`nixos-rebuild switch`.

## Security note

The target's SSH host key is not checked during install: the server was
created seconds earlier by the same run, and its host key changes when
nixos-anywhere kexecs into the installer and again on first boot. If you
need host key pinning, pre-generate host keys and ship them in your flake.

## Development

```sh
go test ./...
# Real Hetzner run (costs a few cents, deletes the server afterwards):
HCLOUD_TOKEN=... NIXZNER_TEST_SSH_KEY=admin NIXZNER_TEST_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" \
  go test -tags integration -run TestCreateIntegration -timeout 45m -v ./cmd/nixzner/
```

Optional overrides for the integration test: `NIXZNER_TEST_TYPE` (default
`cx23`), `NIXZNER_TEST_LOCATION` (default `nbg1`), `NIXZNER_TEST_HOST`
(default `example`; use `example-arm` with a `cax*` type).
