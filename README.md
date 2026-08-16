# DFS

DFS is an experimental, quota-aware distributed filesystem for Linux and
macOS. It presents the same complete file and directory tree on every device
while allowing each device to keep only a quota-limited subset of file content.
Git stores the namespace and history, git-annex stores content, and FUSE exposes
the result as a regular mounted drive.

Missing content is downloaded when it is opened, explicitly fetched, or
pinned. Moving, renaming, and organizing files therefore does not require their
content to be present locally.

> [!WARNING]
> DFS is an MVP. Use a test dataset and keep an independent backup. Linux and
> macOS are covered by native FUSE and cross-peer tests, but broad application
> compatibility and long-term history retention still need more work.

## Main use cases

### Use one shared filesystem on several devices

- Browse the complete namespace on every peer and hydrate content on demand.
- Create, edit, move, rename, and delete files through a normal FUSE mount.
- Continue working while disconnected; deterministic variant names preserve
  both sides of concurrent edits, moves, and delete/modify conflicts.
- Preserve permissions, ownership, timestamps, and extended attributes across
  local remounts. These attributes are currently peer-local and do not
  replicate between devices.
- Normalize new names to Unicode NFC while preserving case, including case-only
  renames on case-sensitive and case-insensitive hosts.

### Control which content occupies each device

- Set an independent cache limit on every peer.
- Fetch content temporarily, pin content against automatic eviction, or safely
  evict local copies while retaining their namespace entries.
- Let LRU pruning enforce the configured target without dropping open or pinned
  files or violating git-annex copy-safety rules.

### Add and operate trusted devices

- Discover nearby DFS filesystems over mDNS/DNS-SD.
- Join with an authenticated, one-use invitation and one explicit approval.
- Resume or abort an interrupted `dfs setup` transaction safely.
- Reconcile signed full-mesh membership, remotes, trust pins, and restricted SSH
  authorizations automatically, including after an offline peer returns.
- Transfer Git metadata, git-annex content, membership, and diagnostics over
  mutually authenticated QUIC, with repository-restricted SSH as a fallback.
- Diagnose every directed peer-to-peer path, including ordinary passwordless
  SSH, with `doctor --mesh`.
- Run more than one active DFS filesystem on the same host. Each repository has
  independent state, mountpoint, service, logs, and TCP/UDP transport port.

### Protect and recover data

- Inspect Git history and restore an older path as a new, non-destructive commit.
- Preserve the last durably published file after interrupted writes or crashes
  and quarantine uncertain private staging state for inspection.
- Add optional encrypted S3-compatible git-annex storage for durable content.
- Add an optional bare Git relay so peers can exchange metadata without being
  online at the same time.

## Requirements and build

Each peer needs Go 1.26 or newer to build, Git, git-annex, OpenSSH, rsync, and a
FUSE runtime. QUIC is the primary managed peer transport. OpenSSH and rsync are
required for the compatibility fallback and diagnostics; inbound SSH requires
an SSH server on that peer.

Arch Linux/CachyOS:

```sh
sudo pacman -S --needed go git git-annex openssh rsync fuse3
```

Debian/Ubuntu:

```sh
sudo apt install golang-go git git-annex openssh-client openssh-server rsync fuse3
```

macOS:

```sh
brew install go git git-annex rsync
```

Install the current macFUSE package from [macfuse.io](https://macfuse.io/).
DFS currently uses macFUSE's VFS/kernel backend, not FSKit. Do not run the
optional `brew services start git-annex`; DFS itself controls synchronization.

Build and check the local dependencies:

```sh
make build
./bin/dfs doctor
```

The binary uses the native Go FUSE protocol implementation and does not link
against libfuse, but FUSE 3 or macFUSE is still required at runtime. See
[INSTALL.md](INSTALL.md) for complete platform setup, macOS permissions,
upgrading managed daemons, firewalls, and live acceptance testing.

## Create the first filesystem

Initialize the first peer, then install its managed mount service:

```sh
./bin/dfs init ~/.local/share/dfs/repository \
  --name desktop --network-name "Home Files" --cache-limit 100GiB

# CachyOS/Arch
./scripts/install-cachyos.sh \
  ~/.local/share/dfs/repository ~/dfs_storage ./bin/dfs

# macOS
./scripts/install-macos.sh \
  ~/.local/share/dfs/repository ~/dfs_storage ./bin/dfs
```

The installer copies the binary and creates a user service whose name contains
the filesystem's 12-character ID. systemd or launchd starts the mount at login,
restarts it after failure, records logs, and uses DFS health reporting.

A manual foreground mount is also available for development:

```sh
./bin/dfs --repo ~/.local/share/dfs/repository mount ~/dfs_storage
```

Press Ctrl-C to unmount it cleanly, or run `dfs unmount ~/dfs_storage` in
another terminal.

## Add another peer

On an existing mounted peer, create a one-use invitation. It expires after ten
minutes by default:

```sh
dfs --repo ~/.local/share/dfs/repository pair invite
```

Send the resulting `dfs2_...` secret through a trusted channel. On the new
device, from the DFS source checkout, run:

```sh
./bin/dfs setup --name laptop --cache-limit 50GiB
```

Paste the invitation at the prompt and approve the displayed filesystem once.
`dfs setup` joins the repository, completes reciprocal mesh registration,
installs the platform user service, mounts the default `~/dfs_storage`, and
verifies health. Reading the invitation from the prompt keeps it out of shell
history.

If setup is interrupted, continue or roll it back:

```sh
./bin/dfs setup --resume
./bin/dfs setup --abort
```

Setup progress, locks, and pairing state are private and repository-specific.
For a non-default repository, pass the same `--repository` to the initial,
resume, or abort command. `--mountpoint` selects another mountpoint and
`--pair-port` selects a local managed transport port. Otherwise setup uses the
first free TCP/UDP port beginning at 7843.

`dfs network discover`, `dfs network join`, `dfs network complete`, and the
installer scripts are the lower-level controls for manual recovery and
diagnostics. `dfs setup` currently joins an existing filesystem; creation of
the first peer still uses `dfs init` and a platform installer.

### Pairing and membership security

The invitation contains a random bearer secret and the inviter's TLS
certificate fingerprint. DFS pins that identity, exchanges dedicated Ed25519
device keys and SSH host keys, clones the repository, and registers
deterministic remotes in both directions. Pairing prefers QUIC and can fall back
to the pinned HTTPS endpoint.

Each admitted peer publishes a signed record in the dedicated
`refs/heads/dfs-membership` Git metadata ref. The approving member signs the
admission and endorses its roster, so one approval establishes the new peer's
full mesh. Periodic synchronization verifies the signature chain and repairs
missing remotes, trust pins, and repository-restricted SSH authorizations.
`peer remove` publishes a signed revocation; revocations accepted locally remain
sticky even if shared history is later altered.

Private keys, trust pins, invitations, runtime state, cache data, staging, and
recovery data stay below `.git/dfs`. Replicated membership and revocation state
stays in dedicated Git metadata refs. DFS internal state never enters the Git
worktree or mounted logical filesystem; `.dfs/` and `.gitattributes` are not
used as internal-state transports.

Changing a machine's hostname invalidates its DFS identity. DFS refuses to
mount that repository rather than silently transferring the old identity; the
renamed machine must be removed and admitted as a new peer. Letter case and the
`.local` suffix are normalized and do not count as a hostname change.

Useful administration commands are:

```sh
dfs --repo ~/.local/share/dfs/repository network discover
dfs --repo ~/.local/share/dfs/repository network set-name "Home Files"
dfs --repo ~/.local/share/dfs/repository network complete
dfs --repo ~/.local/share/dfs/repository pair list
dfs --repo ~/.local/share/dfs/repository pair revoke <invitation-id>
dfs --repo ~/.local/share/dfs/repository peer list
dfs --repo ~/.local/share/dfs/repository peer remove dfs-peer-<id>
dfs --repo ~/.local/share/dfs/repository transport probe <peer-id>
```

Restart the managed mount after changing its advertised network name. Removing
a peer also removes its repository-restricted authorization locally and
publishes its signed revocation, but cannot erase content it already copied.
If reciprocal registration was interrupted after cloning, `network complete`
retries it without consuming a new invitation.

For advanced or non-discovery deployments, `dfs join <git-url> <repository>`
clones an existing repository and `dfs peer add <name> <ssh-url>` registers a
direct SSH remote. These lower-level commands do not replace the admission and
verification performed by guided setup.

## Run multiple filesystems on one host

Use a distinct repository and mountpoint for each filesystem. For example, a
second peer can join with:

```sh
./bin/dfs setup \
  --repository ~/.local/share/dfs/repository-archive \
  --mountpoint ~/dfs_archive \
  --name laptop-archive --cache-limit 25GiB
```

Each managed instance receives a filesystem-specific systemd unit or launchd
label, separate health/runtime data, separate logs, and the first available
transport port. Installing or uninstalling one instance does not replace the
others. Rerun the installer once for every repository after building a new DFS
binary so every active daemon is upgraded.

## Work with files and local storage

Use normal filesystem operations through the mount:

```sh
cp report.pdf ~/dfs_storage/Documents/
mv ~/dfs_storage/Documents/report.pdf ~/dfs_storage/Archive/
rm ~/dfs_storage/Archive/old.pdf
```

Control local content placement explicitly:

```sh
dfs --repo ~/.local/share/dfs/repository fetch Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository pin Photos/Vacation
dfs --repo ~/.local/share/dfs/repository unpin Photos/Vacation
dfs --repo ~/.local/share/dfs/repository evict Movies/large.mkv
dfs --repo ~/.local/share/dfs/repository cache status
dfs --repo ~/.local/share/dfs/repository cache set-limit 75GiB
dfs --repo ~/.local/share/dfs/repository cache prune
```

`fetch` caches content; `pin` also protects it from automatic eviction.
`evict` delegates safety checks to git-annex and refuses to remove pinned
content. Open or pinned files and copy-safety rules can temporarily keep usage
above the configured limit.

## Inspect history and recover a path

```sh
dfs --repo ~/.local/share/dfs/repository status
dfs --repo ~/.local/share/dfs/repository history Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository restore <commit> Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository conflicts
```

Restore creates a new commit and never rewrites shared history. Git history
records namespace versions, but does not by itself retain old annex objects.
Copy versions to durable storage before allowing their final content copy to be
dropped.

## Check health and mesh connectivity

```sh
dfs --repo ~/.local/share/dfs/repository health
dfs --repo ~/.local/share/dfs/repository health --json
dfs --repo ~/.local/share/dfs/repository doctor
dfs --repo ~/.local/share/dfs/repository doctor --mesh
```

`health` validates the private `.git/dfs/health.json` heartbeat, owner process,
and mountpoint. `doctor` checks local dependencies. `doctor --mesh` additionally
asks every known mounted peer to test every configured peer in both directions:
`desktop -> laptop` and `laptop -> desktop` are separate rows. Each read-only
probe tests mutually authenticated QUIC, the repository-restricted SSH
fallback, and ordinary passwordless non-interactive SSH.

Important mesh statuses are:

- `OK`: managed QUIC and the directed transport work.
- `SSH_FALLBACK`: QUIC is unavailable but restricted SSH works.
- `PASSWORDLESS_SSH_FAILED`: DFS transport works, but ordinary passwordless SSH
  is missing for that exact source-to-destination direction.
- `NOT_CONFIGURED`: the source peer has no direct remote for the destination.
- `FAILED`: both managed transport paths failed.
- `UNREPORTED`: no diagnostic report could be obtained, or the peer runs an
  incompatible DFS transport version.
- `ONLY_LOCAL_PEER`: no other member or discovered peer was available, so there
  were no directed edges to test.

`PASSWORDLESS_SSH_FAILED`, `NOT_CONFIGURED`, `FAILED`, and `UNREPORTED` make the
command fail. `SSH_FALLBACK` is a healthy but degraded result. Adjust bounded
probes when needed:

```sh
dfs --repo ~/.local/share/dfs/repository doctor --mesh \
  --discovery-timeout 5s --peer-timeout 10s
```

## Services, logs, and recovery

For a repository, derive the managed instance ID with:

```sh
DFS_INSTANCE=$(git -C ~/.local/share/dfs/repository rev-list --max-parents=0 HEAD | sort | head -1 | cut -c1-12)
```

On systemd:

```sh
systemctl --user status "dfs-mount-$DFS_INSTANCE"
journalctl --user -u "dfs-mount-$DFS_INSTANCE" -f
systemctl --user restart "dfs-mount-$DFS_INSTANCE"
```

On launchd:

```sh
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
launchctl kickstart -k "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
tail -F "$HOME/Library/Logs/DFS/mount-$DFS_INSTANCE.stderr.log"
```

Remove one managed service while retaining its repository and the shared
installed binary with:

```sh
./scripts/install-cachyos.sh --uninstall ~/.local/share/dfs/repository
./scripts/install-macos.sh --uninstall ~/.local/share/dfs/repository
```

Managed services use JSON `info` logging. Manual mounts default to structured
text at `error` level; use `--log-level info`, `--log-level debug`,
`--log-file <path>`, or the very noisy `--fuse-debug` as needed.

Mount startup holds an exclusive repository session and performs conservative
recovery. Interrupted staging payloads, partial annex transfers, and uncertain
Git state are moved under `.git/dfs/recovery/<timestamp>/`. A stale session from
another hostname is never overridden automatically; after verifying that mount
is gone, use `dfs mount --recover-stale-session`. A crashed same-host session
and disconnected FUSE endpoint are recovered automatically.

## Network and firewall requirements

mDNS normally remains within one LAN and can be blocked by guest isolation or
VLANs. A default-drop firewall must allow UDP 5353 for discovery, both UDP and
TCP for each instance's managed port, and TCP 22 if the SSH fallback is wanted.
The first instance normally uses 7843, the next 7844, and so on. DFS continues
mounting with a warning if discovery or the listener cannot start.

Use `dfs mount --discovery=false` when a peer must not advertise or accept
pairing. Already configured remotes continue to work.

## Optional metadata relay

A bare relay lets peers publish Git namespace and annex-location metadata at
different times. It does not store file content:

```sh
ssh server.example.com 'git init --bare /srv/dfs.git'
dfs --repo ~/.local/share/dfs/repository relay set \
  ssh://server.example.com/srv/dfs.git
dfs --repo ~/.local/share/dfs/repository sync --metadata-only
```

## Optional S3 durability

git-annex reads the standard AWS credential environment variables:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...

dfs --repo ~/.local/share/dfs/repository storage add-s3 archive \
  --bucket my-dfs-bucket --region eu-central-1
dfs --repo ~/.local/share/dfs/repository storage copy archive Documents
```

After metadata synchronization, another peer can enable the same remote with:

```sh
dfs --repo ~/.local/share/dfs/repository storage enable archive
```

## Filesystem semantics

Writable opens use private copy-on-write files under `.git/dfs/staging`. DFS
publishes a verified transaction only after the final writable handle closes.
`flush` preserves the transaction until that close because macOS may share one
FUSE handle across multiple application descriptors. `fsync` durably checkpoints
a transaction without ending the open
handle; later writes may advance it. A no-op writable open produces no commit.
Advisory locks, atomic rename-overwrite, open-then-unlink, and renaming an open
writer follow POSIX behavior supported by FUSE.

Automatic metadata synchronization runs after completed transactions and every
30 seconds. Each remote is probed and synchronized independently; an unavailable
peer enters a bounded retry backoff without holding the repository lock or
blocking synchronization and content hydration through healthy peers. Peers can
commit while disconnected. On reconnect, deterministic `.variant-*` paths
retain competing contents, including edit/edit, move/rename, and modify/delete
cases; repeated synchronization converges on the same Git tree.

## Architecture and state boundaries

```text
applications / Finder
        |
        v
Go FUSE mounted logical namespace
        |
        v
DFS transactions, synchronization, and quota scheduler
        |-- Git: namespace, history, membership metadata refs
        |-- git-annex: content hashes, locations, safe copies
        |-- SQLite: peer-local pins, access, and filesystem metadata
        |-- mDNS + pinned TLS: discovery and pairing
        |-- mutually authenticated QUIC: primary peer transport
        |-- restricted SSH: compatibility fallback
        `-- S3: optional durable content
```

The underlying Git worktree is an implementation detail. A locked git-annex
symlink is presented as a regular file; opening missing content runs
`git annex get`, and opening it for writing hydrates a private transaction.

## Current limitations

- mDNS discovery is LAN-local. Routed discovery and relay-assisted invitations
  are not implemented.
- Creating the first filesystem is still a separate `init` plus installer flow;
  guided `dfs setup` currently covers joining an existing filesystem.
- The conflict command reports conflicts but has no complete resolution UI.
- Git history does not automatically retain old annex content.
- Permissions, ownership, timestamps, and extended attributes are peer-local;
  cross-peer POSIX metadata and ACL replication are not implemented.
- Memory mapping, sparse files, large creative applications, and broader
  cross-platform compatibility need more stress testing.
- There is no GUI, package, or stable release yet; administration is currently
  source-built and command-line driven.
- Windows mounting and storage providers beyond S3-compatible git-annex remotes
  are not implemented.

## Development and live verification

```sh
make test
make test-integration
```

The test suite covers repository synchronization, conflict convergence,
transaction publication and recovery, locks, `fsync`, rename/unlink behavior,
metadata persistence, case-only renames, Unicode normalization, and native FUSE
operations. After any source change that can affect mounted behavior, also run
the live cross-peer suite on each available macOS and Arch/CachyOS peer:

```sh
make test-mount MOUNTPOINT=~/dfs_storage \
  PEERS='user@peer1.local:/path/to/mount user@peer2.local:/path/to/mount'
```

It verifies local operations plus exact cross-peer filenames, content updates,
renames, and deletions within a bounded deadline. See [INSTALL.md](INSTALL.md)
for prerequisites and full instructions.

## License

MIT
