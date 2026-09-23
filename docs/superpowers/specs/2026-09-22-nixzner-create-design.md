# nixzner `create` — Design

Date: 2026-09-22
Status: Approved (brainstorming)

## Purpose

`nixzner` is a Go CLI that provisions a Hetzner Cloud server and installs NixOS on it
for production use. It creates the server via the Hetzner Cloud API, installs NixOS
from a user-supplied flake by wrapping [nixos-anywhere](https://github.com/nix-community/nixos-anywhere)
(kexec + disko), and verifies the first boot.

v1 ships a single command: `create`.

## Decisions

| Topic | Decision |
|---|---|
| Install method | nixos-anywhere (kexec + disko), not nixos-infect |
| Integration | Wrap `nixos-anywhere` as a subprocess; operator machine needs Nix |
| Interface | Imperative CLI |
| Commands (v1) | `create` only |
| Resource scope | Servers only; existing firewalls/networks attached by name |
| Post-install | Out of scope; nixzner ends at verified first boot (day-2 via deploy-rs/colmena/nixos-rebuild) |
| Failure policy | Delete the server on any failure or interrupt, after saving logs locally |
| SSH keys | Existing Hetzner-registered key via `--ssh-key`; local auth via ssh-agent or `--identity` |
| Build location | Always `--build-on remote` (works from macOS and CI, x86 and ARM alike) |
| Code structure | Step pipeline with reverse-order cleanup |
| Libraries | `hcloud-go/v2`, `cobra`, `golang.org/x/crypto/ssh`, `os/exec` |

## CLI

```
nixzner create <name> \
  --flake .#web1                 # required
  --type cx32                    # required, Hetzner server type
  --location fsn1                # required
  --ssh-key admin                # required, name of a Hetzner-registered SSH key
  --identity ~/.ssh/id_ed25519   # optional; default: ssh-agent
  --image debian-12              # bootstrap image, default debian-12
  --firewall web                 # optional, repeatable, must already exist
  --network internal             # optional, repeatable, must already exist
  --label env=prod               # optional, repeatable
  --log-dir ./nixzner-logs       # default ./nixzner-logs
  --timeout 30m                  # overall deadline, default 30m
```

- Hetzner token is read from `HCLOUD_TOKEN`; missing token is a preflight error.
- Architecture is derived from the server type: `cax*` → arm64, otherwise x86. The
  bootstrap image is resolved for that architecture. The flake must set a matching
  `nixpkgs.hostPlatform`.
- Labels always applied: `managed-by=nixzner`, `nixzner-flake=<attr>` (sanitized to
  Hetzner label rules: ≤63 chars, `[a-zA-Z0-9._-]`, alphanumeric at both ends),
  `nixzner-status=installing|ready`. User `--label` values are validated against the
  same rules; keys starting with `nixzner-` or equal to `managed-by` are rejected.
- Output: human-readable progress on stderr. On success, one line on stdout:
  `<name> <ipv4> <ipv6> <store-path>` (space-separated).
- Exit codes: `0` success; `1` failed and cleaned up (or failed before creating
  anything); `2` failed **and** cleanup failed — server may still exist and be billed.

## Components

```
cmd/nixzner/main.go      cobra root + `create`; flag parsing, signal handling, exit codes
internal/config/         CreateOptions + Validate() — pure, no I/O
internal/hetzner/        interface over hcloud-go:
                           Preflight(ctx, opts) error
                           CreateServer(ctx, opts) (Server, error)  // waits for create action
                           DeleteServer(ctx, id) error
                           SetLabels(ctx, id, labels) error
internal/nixos/          interface over nix / nixos-anywhere subprocesses:
                           Eval(ctx, flake) (drvPath, outPath string, err error)
                           Install(ctx, InstallArgs, log io.Writer) error
internal/sshcheck/       WaitForSSH(ctx, host, auth) error
                         VerifyNixOS(ctx, host, auth, wantOutPath) error
internal/pipeline/       Step{Name, Run, Cleanup}; runs in order, cleanups in reverse
internal/create/         builds and runs the create pipeline; only package aware of step order
```

`Server` carries at least: ID, name, IPv4, IPv6.

Boundaries: `pipeline` knows nothing about Hetzner or Nix. `hetzner`, `nixos`, and
`sshcheck` are the only packages doing external I/O; each is consumed through an
interface so `create` can be tested with fakes.

### Expected store path

Before installing, `nixos.Eval` runs
`nix eval --raw <flake>.config.system.build.toplevel.drvPath` and `.outPath`
(where `<flake>` `.#web1` becomes `.#nixosConfigurations.web1`). Evaluation needs no
Linux builder, so it works on macOS. `outPath` is the value verification compares against.

### SSH host keys

Both the bootstrap image and the installed NixOS have fresh, unknown host keys.
`sshcheck` uses `ssh.InsecureIgnoreHostKey()` and nixos-anywhere is passed
`-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null`. This is an accepted
trust-on-first-use trade-off, limited to a server created seconds earlier by this run.

## `create` flow

| # | Step | Run | Cleanup |
|---|---|---|---|
| 1 | Preflight | validate options; `HCLOUD_TOKEN` set; `nixos-anywhere` and `nix` on PATH; server type, location, image, SSH key, firewalls, networks exist; server type in stock in the location (error names where it is in stock); name unused; `nix eval` drvPath + outPath | — |
| 2 | Create server | create with bootstrap image, SSH key, firewalls, networks, labels (`nixzner-status=installing`); wait for action | `DeleteServer(id)` |
| 3 | Wait for SSH | TCP 22 reachable, then root SSH auth succeeds; exponential backoff, 5 min cap | — |
| 4 | Install | `nixos-anywhere --flake <flake> --build-on remote --target-host root@<ipv4> [-i <identity>] --ssh-option StrictHostKeyChecking=no --ssh-option UserKnownHostsFile=/dev/null`; output tee'd to stderr and a temp log file | — |
| 5 | Verify | wait for SSH after reboot; `/etc/NIXOS` exists; `readlink /run/current-system` equals expected outPath; 5 min cap | — |
| 6 | Finalize | set `nixzner-status=ready` | — |

Preflight creates nothing, so a preflight failure needs no cleanup and exits `1`.

## Error handling

- One root `context.Context` carries the `--timeout` deadline; SIGINT/SIGTERM cancel it.
- On step error or cancellation, the pipeline runs registered cleanups in reverse order
  using a **fresh** context with a 2-minute budget (the root context may already be done).
- Before cleanup, the temp log is moved to `<log-dir>/<name>-<UTC timestamp>.log` and its
  path printed.
- Cleanup failure → exit `2`, print server ID, name, and `hcloud server delete <id>`.
- Signals during cleanup: the second SIGINT is ignored with a notice; a third forces exit
  with the same possibly-orphaned-server warning and exit `2`.
- No automatic retries apart from the SSH wait loops.

## Testing

Unit tests (no network):

- `config` — table tests for required flags, label validation/sanitization, architecture
  derivation.
- `pipeline` — ordered execution; reverse cleanup on error; cleanup on ctx cancel; cleanup
  errors aggregated; steps without cleanup skipped.
- `create` — full flow against fakes of `hetzner`, `nixos`, `sshcheck`; one scenario per
  failing step asserting server deletion, log persisted, exit code; cleanup-failure → `2`.
- `nixos` — argument construction against golden command lines; injected command runner
  using the `TestHelperProcess` pattern.
- `hetzner` — against an `httptest` server with recorded JSON: preflight lookups, create
  with labels, delete, label validation.

Integration test (`//go:build integration`, manual, never in CI):

- Requires `HCLOUD_TOKEN`, `NIXZNER_TEST_SSH_KEY` (Hetzner key name) and a matching local
  key/agent.
- Uses `testdata/flake/` (disko for `/dev/sda`, openssh, root authorized key from env).
- Creates a `cx22`, asserts success, always deletes the server in `t.Cleanup`.

## Deliverables

- The `nixzner` binary (`cmd/nixzner`).
- `examples/flake/`: a known-good minimal flake (disko layout for Hetzner, `hostPlatform`,
  openssh, root authorized keys) documented in the README.
- README: prerequisites (Nix with flakes, a nixos-anywhere release supporting `--build-on`, `HCLOUD_TOKEN`), usage, the
  requirement that the flake authorize the same SSH key, and the TOFU host-key note.

## Out of scope (v1)

`reinstall`, `destroy`, `list`, `status`/`console`; creating or owning firewalls,
networks, volumes, IPs; declarative config; day-2 deployment; JSON output; local builds;
nixos-infect.
