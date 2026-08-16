# DFS

DFS is an experimental, quota-aware distributed filesystem for Linux and macOS. It combines Git's shared namespace and history with git-annex's content-addressed storage and exposes the result as a regular FUSE drive.

Every peer sees the complete file and directory tree. File content is downloaded only when opened, explicitly fetched, or pinned. Moves and renames therefore work even when the content is not stored locally.

> [!WARNING]
> This is an MVP. Use a separate test dataset and keep an independent backup. Linux and macOS are exercised with native FUSE integration tests, but broader application-compatibility testing is still needed.

## What works

- A complete Git-backed namespace on every peer.
- Direct Git/git-annex peers over SSH or a local path.
- Local-network discovery with authenticated, one-use peer pairing.
- Authenticated full-mesh configuration and connectivity diagnostics.
- Optional bare Git metadata relay.
- On-demand hydration when an annexed file is opened.
- Writes committed only after all writable mount handles have closed.
- Advisory locks, durable `fsync`, atomic rename-overwrite, and open-then-unlink behavior.
- Persistent permissions, timestamps, and extended attributes on Linux and macOS.
- Case-preserving names and NFC-normalized Unicode paths.
- Pin, fetch, unpin, safe eviction, and LRU quota enforcement.
- Git history and non-destructive restore commits.
- Encrypted S3-compatible git-annex storage.
- Managed user services with health reporting on systemd and launchd.
- One Go codebase for Linux and macOS.

## Requirements

Each peer needs:

- Go 1.26 or newer to build;
- Git and git-annex;
- OpenSSH and rsync for SSH peers;
- FUSE 3 on Linux or macFUSE on macOS.

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
brew install go git git-annex
```

Install the current macFUSE package from [macfuse.io](https://macfuse.io/). Direct SSH access to a macOS peer also requires enabling Remote Login. A device that should send content directly to another paired device needs an SSH server reachable on the LAN; pairing installs repository-restricted keys but does not enable the operating system's SSH service.

## Build

```sh
make build
./bin/dfs doctor
```

The binary uses the native Go FUSE protocol implementation and does not link against libfuse. FUSE or macFUSE is still required at runtime.

## Managed mount service

The recommended setup runs the foreground mount process under the operating system's user service manager. The service manager handles login startup, restart after failure, SIGTERM shutdown, and log collection; DFS continues to own FUSE unmounting and repository recovery.

Build DFS first, then install the service with the repository and mountpoint for that peer.

### CachyOS/systemd

```sh
make build
./scripts/install-cachyos.sh \
  ~/.local/share/dfs/repository ~/DFS ./bin/dfs
```

This installs the shared binary as `~/.local/bin/dfs` and creates one service
whose name contains the filesystem's 12-character ID. A second repository gets
a second independent service instead of replacing the first. Determine the
instance name and inspect it with:

```sh
DFS_INSTANCE=$(git -C ~/.local/share/dfs/repository rev-list --max-parents=0 HEAD | sort | head -1 | cut -c1-12)
systemctl --user status "dfs-mount-$DFS_INSTANCE"
journalctl --user -u "dfs-mount-$DFS_INSTANCE" -f
systemctl --user restart "dfs-mount-$DFS_INSTANCE"
systemctl --user stop "dfs-mount-$DFS_INSTANCE"
dfs --repo ~/.local/share/dfs/repository health
```

User services normally start when the user logs in. To allow this mount to start during boot before login, an administrator can enable lingering once:

```sh
sudo loginctl enable-linger "$USER"
```

### macOS/launchd

```sh
make build
./scripts/install-macos.sh \
  ~/.local/share/dfs/repository ~/DFS ./bin/dfs
```

This installs one shared signed helper at `~/Library/Application Support/DFS/DFS.app`
and creates a filesystem-specific launch agent. Multiple launch agents can run
the shared executable concurrently with distinct repositories, mountpoints,
logs, and transport ports. Allow **DFS** under **System Settings → Privacy &
Security → Local Network** when macOS prompts. Inspect or control an instance with:

```sh
DFS_INSTANCE=$(git -C ~/.local/share/dfs/repository rev-list --max-parents=0 HEAD | sort | head -1 | cut -c1-12)
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
launchctl kickstart -k "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
dfs --repo ~/.local/share/dfs/repository health
tail -F "$HOME/Library/Logs/DFS/mount-$DFS_INSTANCE.stderr.log"
```

Rerun the platform installer after building a new DFS version. To disable and remove a service definition while retaining the installed binary and repository data:

```sh
./scripts/install-cachyos.sh --uninstall ~/.local/share/dfs/repository
./scripts/install-macos.sh --uninstall ~/.local/share/dfs/repository
```

Packagers can use the templates under `packaging/systemd` and `packaging/launchd`. The installers generate equivalent definitions with absolute, safely escaped local paths.

## Discover and pair a peer

The mounted DFS process advertises its network over mDNS/DNS-SD as `_dfs._tcp.local`. Give the network a recognizable name before starting or restarting the mount:

```sh
dfs init ~/.local/share/dfs/repository \
  --name desktop --network-name "Home Files" --cache-limit 100GiB
dfs --repo ~/.local/share/dfs/repository mount ~/DFS
```

In another terminal on that peer, create a one-use invitation. It expires after ten minutes by default:

```sh
dfs --repo ~/.local/share/dfs/repository pair invite
```

The command prints an invitation beginning with `dfs2_`. Send it to the user of
the new device through a trusted channel. On the new device, use the recommended
transactional setup:

```sh
dfs setup --name laptop --cache-limit 50GiB
```

Paste the invitation at the prompt and approve the filesystem once. The command
joins the network, installs the platform user service, mounts the default
`~/dfs_storage` volume, and verifies it. It records progress privately, so an
interrupted attempt continues with `dfs setup --resume`; `dfs setup --abort`
removes an incomplete transaction and the repository it created. Reading the
invitation from the prompt keeps its bearer secret out of shell history.
Setup transactions are keyed by repository path, so separate filesystems can
be joined or resumed independently. Each managed instance uses the first free
TCP/UDP port beginning at 7843; pass `--pair-port` to select one explicitly.

`dfs network discover`, `dfs network join`, and the installer scripts remain as
lower-level diagnostic and manual controls.

The invitation contains a random bearer secret and the existing peer's TLS certificate fingerprint. DFS discovers the matching filesystem ID, pins that certificate, exchanges dedicated Ed25519 device keys and SSH host keys, clones the repository, and registers deterministic remotes in both directions. Pairing itself prefers QUIC and falls back to the pinned HTTPS endpoint. After admission, mutually authenticated QUIC is the primary transport for Git metadata, annex content, diagnostics, and membership traffic. Pairing also adds `restrict,command="... peer serve"` entries to each user's `~/.ssh/authorized_keys`; this repository-scoped SSH path remains the automatic fallback when QUIC cannot be established. The forced command delegates only Git and git-annex operations to `git-annex-shell` with `GIT_ANNEX_SHELL_DIRECTORY` fixed to that DFS repository and accepts one fixed, read-only diagnostic command. It does not grant a general shell. Private keys, pinned host keys, invitations, and runtime discovery state stay under `.git/dfs` with private permissions.

Each admitted device also publishes a signed record in the dedicated
`refs/heads/dfs-membership` Git metadata ref with its stable ID, hostname, role,
generation, signing key, and transport endpoint. Membership and revocation data
never enters the user worktree or mounted filesystem; private keys and local
trust pins remain under `.git/dfs` and are never replicated.
The approving member signs the admission and endorses its current roster, so a
single approval establishes the new device's full mesh. Periodic sync verifies
the signature chain and automatically reconciles missing remotes, pinned host
keys, and repository-restricted authorizations on online peers; offline peers
do the same after their next sync. `peer remove` publishes an administrator-
signed revocation, and accepted revocations remain sticky in private local
state even if a later Git change removes the shared revocation file.

Useful invitation and naming commands are:

```sh
dfs --repo ~/.local/share/dfs/repository pair list
dfs --repo ~/.local/share/dfs/repository pair revoke <invitation-id>
dfs --repo ~/.local/share/dfs/repository network complete
dfs --repo ~/.local/share/dfs/repository network set-name "Archive"
dfs --repo ~/.local/share/dfs/repository peer list
dfs --repo ~/.local/share/dfs/repository peer remove dfs-peer-<id>
```

Restart the mount after changing the advertised network name. Removing a paired remote also removes its matching repository-restricted authorization on that device. Revocation cannot erase data that a peer already downloaded.

Pairing records the completion token privately after the clone. If the final reciprocal-registration request is interrupted, the joined repository remains usable and the command reports the exact `network complete` command needed to resume safely.

mDNS normally stays within one LAN and may be blocked by guest Wi-Fi isolation or VLAN boundaries. A currently mounted inviter embeds its `.local` pairing endpoint in the invitation as a fallback. `pair invite --clone-url <url>` can override the authenticated clone URL for routed or advanced setups; the manual join flow remains available.

A default-drop host firewall must allow UDP 5353 for mDNS, both UDP and TCP for
every instance's managed port, and the SSH server port (normally TCP 22) from
the trusted LAN. The first instance normally uses 7843, the next automatic
setup normally uses 7844, and so on. `dfs setup` prints its selected port;
manual service installs set it with `--pair-port`. DFS logs a warning and
continues mounting when discovery or the pairing listener cannot start.

Use `dfs mount --discovery=false` when a peer must not advertise the filesystem or run a pairing endpoint. Manual configured remotes continue to work.

## Manual two-peer quick start

On Linux:

```sh
dfs init ~/.local/share/dfs/repository --name linux --cache-limit 100GiB
dfs --repo ~/.local/share/dfs/repository mount ~/DFS
```

On macOS, clone the Linux repository over SSH:

```sh
dfs join ssh://linux.local/home/alice/.local/share/dfs/repository \
  ~/.local/share/dfs/repository --name mac --cache-limit 50GiB
dfs --repo ~/.local/share/dfs/repository mount ~/DFS
```

For transfers in both directions, also register macOS on Linux:

```sh
dfs --repo ~/.local/share/dfs/repository peer add mac \
  ssh://mac.local/Users/alice/.local/share/dfs/repository
```

The mounted drive accepts normal filesystem operations:

```sh
cp report.pdf ~/DFS/Documents/
mv ~/DFS/Documents/report.pdf ~/DFS/Archive/
rm ~/DFS/Archive/old.pdf
```

The mount process runs automatic metadata sync after completed transactions and every 30 seconds. When running it manually, press Ctrl-C to cleanly unmount and stop it, or use `dfs unmount ~/DFS` from another terminal. SIGTERM from systemd or launchd uses the same clean shutdown path. If another process has its working directory inside the mount, shutdown falls back to a lazy/forced detach rather than hanging on `EBUSY`. A later service or manual start automatically detaches a disconnected DFS/FUSE endpoint left behind by a crashed process; it does not replace a healthy existing mount.

### Transactional writes

Writable opens use copy-on-write files under the repository's private `.git/dfs/staging` directory. Reads through the mount see staged content, while the locked git-annex entry is left untouched until the final writable handle successfully flushes or closes. DFS publishes a dirty staging file with a same-filesystem atomic rename and then schedules annexing and synchronization. A writable open that performs no write, truncate, or handle-level metadata mutation discards its staging copy without changing Git or triggering sync.

`fsync` first synchronizes the staging descriptor, then atomically publishes and directory-syncs a checkpoint without ending the open transaction. A later write can advance that checkpoint; after a crash, the last successfully synchronized destination remains valid and any newer unfinished staging payload is quarantined by startup recovery. Flush and `fsync` errors are returned to the application, mark the transaction failed, and prevent an unverified payload from being published on release.

DFS forwards advisory record locks to FUSE and preserves POSIX open-file behavior across unlink and rename-overwrite. Renaming an open write transaction moves its eventual publication target; unlinking it detaches the pathname so closing the descriptor cannot recreate the deleted file.

DFS preserves the mounted file's visible inode and timestamps while git-annex replaces the published regular file with its internal symlink. This prevents editors such as Vim from reporting a false external change during save.

Permissions, ownership, timestamps, and extended attributes are stored in the peer's private DFS state database so they survive git-annex relocking and remounting without mutating content-addressed annex objects. They currently remain peer-local metadata and are not replicated through Git. Names created through DFS are normalized to Unicode NFC while preserving case; case-only renames are supported on both case-sensitive and case-insensitive hosts.

### Concurrent changes

Peers may commit while disconnected. After they reconnect, concurrent edits retain both contents using deterministic `.variant-*` names; a modification concurrent with deletion is retained as a variant, and competing rename/move destinations are both kept. DFS disables Git's heuristic rename pairing during synchronization so independent operations on different paths with identical annex content cannot be mistaken for one another and discard a valid version. Repeated synchronization reaches the same Git tree on every peer.

Mount startup holds an exclusive repository session and performs conservative recovery before exposing the filesystem. Interrupted write payloads, legacy staging files, partial annex transfers, and stale Git index locks are moved under `.git/dfs/recovery/<timestamp>/`; the last published destination is left untouched. Durable transaction manifests let DFS remove only proven empty placeholders from interrupted creates and recognize writes whose atomic rename completed before a crash. Pending Git index changes are committed, then Git and git-annex receive fast consistency checks. In-progress merges, rebases, cherry-picks, reverts, and bisects are copied to the recovery directory and block mounting for manual resolution rather than being destructively reset.

A session recorded by another hostname is assumed active because DFS cannot safely inspect arbitrary remote processes. After verifying that the reported mount is no longer active, pass `--recover-stale-session` to `dfs mount`. DFS preserves the displaced session record in `.git/dfs/recovery/` before continuing. The option never overrides a live mount on the current host.

### Mount logging and debugging

Mount logging uses Go's structured `log/slog` text format. The default `error` level remains quiet unless an operation fails. Use `info` to see mount lifecycle, filesystem changes, hydration, synchronization, pin refresh, and cache-prune activity:

```sh
dfs --repo ~/.local/share/dfs/repository mount \
  --log-level info ~/DFS
```

Use `debug` to additionally record Git and git-annex subprocesses and their durations. `--log-file` appends the same output to a mode `0600` file while continuing to write it to the terminal:

```sh
dfs --repo ~/.local/share/dfs/repository mount \
  --log-level debug --log-file ~/dfs-mount.log ~/DFS
```

For low-level kernel/FUSE request tracing, add `--fuse-debug`. This is very noisy and automatically enables debug-level logging:

```sh
dfs --repo ~/.local/share/dfs/repository mount \
  --fuse-debug --log-file ~/dfs-fuse.log ~/DFS
```

Accepted log levels are `debug`, `info`, `warn`, and `error`.

Managed services use `--log-format json --log-level info`. systemd sends these records to the user journal; launchd writes them under `~/Library/Logs/DFS`. Manual mounts default to the more readable structured text format. Either mode can explicitly select `--log-format text` or `--log-format json`.

### Health and recovery

While mounting, DFS atomically updates `.git/dfs/health.json` with its lifecycle state, PID, hostname, mountpoint, start time, heartbeat time, and last startup/shutdown error. It is private peer state and never appears in the mounted volume or Git history. Check it with:

```sh
dfs --repo ~/.local/share/dfs/repository health
dfs --repo ~/.local/share/dfs/repository health --json
```

The command succeeds only when the report is current, the owning process is alive on this host, and the mountpoint responds. States progress through `starting`, `ready`, and `stopping`/`stopped`; startup failures remain recorded as `failed`. systemd additionally waits for the `ready` transition and receives a watchdog heartbeat.

On restart, DFS removes a dead same-host session record and detaches Linux `ENOTCONN` or macOS `ENXIO` FUSE endpoints before mounting. A session claiming to belong to another host still requires explicit `--recover-stale-session` after verifying that remote mount is gone; service supervision never overrides that safety check.

## Content placement

```sh
dfs --repo ~/.local/share/dfs/repository fetch Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository pin Photos/Vacation
dfs --repo ~/.local/share/dfs/repository unpin Photos/Vacation
dfs --repo ~/.local/share/dfs/repository evict Movies/large.mkv
dfs --repo ~/.local/share/dfs/repository cache set-limit 75GiB
dfs --repo ~/.local/share/dfs/repository cache prune
```

`fetch` caches content, while `pin` also protects it from automatic eviction. `evict` delegates safety checks to git-annex and refuses to remove pinned content. The configured limit is enforced after transactions; open or pinned files and git-annex copy-safety rules can temporarily keep usage above it.

## Metadata relay

A central relay is optional. Direct peers work without it, but both peers must overlap online. A relay lets peers publish metadata at different times.

Create an empty bare repository on an SSH-accessible server:

```sh
ssh server.example.com 'git init --bare /srv/dfs.git'
dfs --repo ~/.local/share/dfs/repository relay set \
  ssh://server.example.com/srv/dfs.git
dfs --repo ~/.local/share/dfs/repository sync --metadata-only
```

The relay stores Git namespace/history and annex location logs. It does not need to store file content.

## S3 durability

git-annex reads the standard AWS credential environment variables. Add encrypted S3 storage and copy content to it with:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...

dfs --repo ~/.local/share/dfs/repository storage add-s3 archive \
  --bucket my-dfs-bucket --region eu-central-1
dfs --repo ~/.local/share/dfs/repository storage copy archive Documents
```

On another peer, synchronize metadata, provide credentials, and run:

```sh
dfs --repo ~/.local/share/dfs/repository storage enable archive
```

## History and diagnostics

```sh
dfs --repo ~/.local/share/dfs/repository status
dfs --repo ~/.local/share/dfs/repository history Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository restore <commit> Documents/report.pdf
dfs --repo ~/.local/share/dfs/repository conflicts
dfs --repo ~/.local/share/dfs/repository doctor
dfs --repo ~/.local/share/dfs/repository doctor --mesh
```

Restore creates a new commit and does not rewrite shared history.

Plain `doctor` checks the required Git, git-annex, SSH, rsync, and platform
FUSE commands or devices. `doctor --mesh` runs those local checks first, then
adds the repository-aware mesh validation below.

`doctor --mesh` discovers mounted peers in the same DFS filesystem and asks
each paired peer to verify every configured peer transport in both directions.
It evaluates the complete directed graph among peers discovered by the
initiating peer: `desktop → laptop` and `laptop → desktop` are separate checks.
Each probe is read-only: it checks mutually authenticated QUIC first, exercises
the repository-restricted SSH fallback when needed, and separately verifies
ordinary passwordless SSH without fetching or changing repository refs.

```text
FROM                    TO                      STATUS          DETAIL
desktop (a1b2c3d4e5f6)  laptop (112233445566)   OK
laptop (112233445566)   desktop (a1b2c3d4e5f6)  NOT_CONFIGURED  paired remote is missing
```

`OK` means managed QUIC and the directed transport work. `SSH_FALLBACK` means
QUIC is unavailable but the restricted SSH alternative works. `NOT_CONFIGURED` means the source
peer has no direct remote for the discovered destination. `FAILED` means that
remote exists but both transport paths failed. `UNREPORTED` means the initiating
peer could not obtain a diagnostic report, for example because both transports failed or
the remote DFS version is too old. `ONLY_LOCAL_PEER` means no other peer was
discovered, so there were no directed edges to test.

The command exits unsuccessfully if any edge is neither `OK` nor the healthy,
degraded `SSH_FALLBACK` state. Configured membership/remotes remain
authoritative even when mDNS is blocked; discovery supplements them with nearby
offers. Adjust the bounded discovery and connection probes when necessary:

```sh
dfs --repo ~/.local/share/dfs/repository doctor --mesh \
  --discovery-timeout 5s --peer-timeout 10s
```

All peers must run a compatible DFS transport protocol; older peers are
reported as `UNREPORTED` until they are upgraded.

## Architecture

```text
applications / Finder
        │
        ▼
systemd / launchd supervision and health
        │
        ▼
Go FUSE mounted view
        │ open, close, rename, delete
        ▼
DFS transaction and quota scheduler
        ├── Git: namespace, history, merges
        ├── git-annex: hashes, locations, safe copies
        ├── SQLite: pins and last-access times
        ├── mDNS + pinned TLS: discovery and pairing
        ├── restricted SSH: Git metadata, git-annex content, mesh diagnostics
        └── S3: optional durable content
```

The underlying Git working tree is an implementation detail. DFS keeps its peer-local configuration, database, mount session, staging, and recovery data under `.git/dfs`, so none of it is tracked by Git or exposed through the mount. A locked git-annex symlink is presented as a normal file; opening missing content runs `git annex get`, and opening it for writing first hydrates it into a private copy-on-write transaction.

## MVP limitations

- mDNS discovery is LAN-local and depends on multicast support. Routed discovery and relay-assisted invitations are not implemented yet.
- Pairing currently uses the operating system's SSH service for Git/git-annex transport after securely exchanging restricted device keys; a DFS-managed transport is not implemented yet.
- Membership is configured between the two paired devices. Signed network-wide membership, administrator roles, and automatic reconciliation with every existing peer are not implemented yet.
- The conflict command lists conflicts but does not provide a full conflict-resolution UI.
- History does not itself retain old annex objects. Replicate versions to durable storage before allowing their last copy to be dropped.
- Memory mapping, sparse files, ACL replication, cross-peer POSIX-metadata replication, and large creative applications need more cross-platform stress testing.
- There is no GUI; service installation and peer administration are command-line workflows.
- Cloud storage is S3-compatible only in the CLI; additional providers can be added through git-annex special remotes later.

## Development

```sh
make test
make test-integration  # mounts a temporary FUSE filesystem
```

The normal test suite includes real two-peer Git/git-annex flows for connected operations, disconnected edit/edit, rename/move, modify/delete, identical-content edge cases, retained content, and stable tree convergence. The integration target additionally verifies copy-on-write publication, no-op and multiple writable handles, advisory locks, `fsync`, flush/error propagation, atomic rename-overwrite, open rename/unlink behavior, persistent permissions/timestamps/xattrs, case-only renames, NFC normalization, stable visible metadata across annex sync, and repeated Vim saves through a real FUSE mount.

## License

MIT
