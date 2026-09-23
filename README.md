# nixzner

Create a Hetzner Cloud server and install NixOS on it from your flake, in one command.

`nixzner create` creates the server from a stock Debian image, runs
[nixos-anywhere](https://github.com/nix-community/nixos-anywhere) against it
(kexec → disko → install, building on the server), and verifies that the
server boots into exactly the system your flake describes. If anything
fails — or you press Ctrl-C — the server is deleted and the full log is kept
in `./nixzner-logs/`.

nixzner stops at a verified first boot. Deploy later changes with
`nixos-rebuild --target-host`, deploy-rs, or colmena.

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

On success, stdout gets one line: `<name> <ipv4> <ipv6> <store-path>`.
Progress goes to stderr.

| Exit code | Meaning |
|---|---|
| 0 | Server created and verified |
| 1 | Failed; nothing left behind |
| 2 | Failed **and** the server could not be deleted — it is still billed; the command to delete it is printed |

Servers get the labels `managed-by=nixzner`, `nixzner-flake=<flake>`, and
`nixzner-status=installing|ready`.

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
