# Install DFS on a New Peer

DFS is experimental. Start with test data and keep an independent backup.

## 1. Install and build

Each peer needs Go 1.26+, Git, git-annex, and a FUSE runtime. DFS peer
communication and cluster diagnostics use authenticated QUIC.

### CachyOS/Arch Linux prerequisites

```sh
sudo pacman -S --needed go git git-annex fuse3
```

### macOS prerequisites

Install the command-line dependencies with Homebrew.

```sh
brew install go git git-annex
```

Installing `git-annex` may print a suggestion to run
`brew services start git-annex`. Do not start that optional service for a DFS
repository: it runs `git-annex assistant`, while DFS already controls annex
operations and synchronization.

Install the current macFUSE package from the
[official macFUSE site](https://macfuse.io/) by opening its downloaded installer.
The macFUSE project recommends its signed installer over third-party package
managers. Homebrew's cask is an alternative:

```sh
brew install --cask macfuse
```

DFS currently uses macFUSE's VFS/kernel backend, not its optional FSKit
backend. Its go-fuse integration expects the traditional FUSE device transport,
and FSKit also restricts mountpoints to `/Volumes`, whereas the examples below
use a mountpoint in the user's home directory. Do not select
`backend=fskit`. On the first mount, follow macOS's prompt to allow macFUSE under
**System Settings → Privacy & Security**, then restart when requested. On an
Apple Silicon Mac whose security policy has never allowed third-party kernel
extensions, macOS may first direct you to Recovery: choose **Reduced Security**,
enable **Allow user management of kernel extensions from identified
developers**, restart, approve macFUSE in Privacy & Security, and restart once
more. Do not disable Gatekeeper or System Integrity Protection. See macFUSE's
[current installation instructions](https://github.com/macfuse/macfuse/wiki/Getting-Started)
for the dialogs used by your macOS version.

Verify that the runtime helper is installed:

```sh
test -x /Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse \
  && echo "macFUSE is installed"
```

### Build DFS

After installing the platform prerequisites:

```sh
git clone https://github.com/bitbeamer/dfs.git
cd dfs
make build
./bin/dfs health
```

## Updating a managed installation from source

Build the new binary in the DFS source checkout before replacing the running
version:

```sh
cd ~/dfs
git pull --ff-only
make build
./bin/dfs --version
./bin/dfs health
```

Do not copy `bin/dfs` over the installed binary while a daemon is running.
Rerun the installer for the current platform. It stops the mount frontend and
core daemon, installs the binary, reloads both service definitions, starts the
core before the mount, and waits for health and mount readiness.

The installed executable is shared, but every active filesystem has its own
service. On a host with multiple DFS filesystems, rerun the installer once for
each repository and mountpoint. This updates every service definition and
restarts every daemon on the new executable; updating only one instance can
leave another active process running the old code.

On CachyOS/systemd:

```sh
cd ~/dfs
./scripts/install-cachyos.sh --pair-port 7843 \
  ~/.local/share/dfs/repository ~/dfs_storage ./bin/dfs

DFS_INSTANCE=$(sed -n 's/.*"filesystem_id": *"\([0-9a-f]*\)".*/\1/p' ~/.local/share/dfs/repository/.git/dfs/config.json | cut -c1-12)
systemctl --user --no-pager --full status "dfs-core-$DFS_INSTANCE" "dfs-mount-$DFS_INSTANCE"
~/.local/bin/dfs --repo ~/.local/share/dfs/repository health
```

On macOS/launchd:

```sh
cd ~/dfs
./scripts/install-macos.sh --pair-port 7843 \
  ~/.local/share/dfs/repository ~/dfs_storage ./bin/dfs

DFS_INSTANCE=$(sed -n 's/.*"filesystem_id": *"\([0-9a-f]*\)".*/\1/p' ~/.local/share/dfs/repository/.git/dfs/config.json | cut -c1-12)
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.core.$DFS_INSTANCE"
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
"$HOME/Library/Application Support/DFS/bin/dfs" \
  --repo ~/.local/share/dfs/repository health
```

DFS persists the logical filesystem ID in `.git/dfs/config.json`; service names
remain stable even if administrators later perform Git history maintenance.

The installer puts the daemon in a signed `DFS.app` helper with a stable bundle
identifier and keeps `~/Library/Application Support/DFS/bin/dfs` as a
compatibility link. On first install, allow **DFS** under **System Settings →
Privacy & Security → Local Network** so discovery can use `_dfs._udp`.
When updating, reuse each instance's existing `--pair-port`; choosing another
port changes its advertised endpoint, and omitting the option defaults a manual
installer invocation to 7843.

Homebrew is normally added to interactive macOS shells automatically. For a
non-interactive build environment, make it explicit first:

```sh
export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
```

To make Homebrew available to non-interactive zsh commands permanently,
add the following to `~/.zshenv` on the Mac:

```sh
typeset -U path PATH
path=(/opt/homebrew/bin /usr/local/bin $path)
export PATH
```

`dfs health` also adds the standard Apple Silicon and Intel Homebrew binary
directories to its own dependency search path, so diagnostics do not depend on
interactive shell startup files.

The existing peer and the new peer must reach each other over managed QUIC. On
a default-drop firewall, allow UDP 5353 and UDP for each filesystem instance's
selected managed port from the trusted LAN. The first instance
normally uses 7843; subsequent automatic setups select the next free port and
print it.

## 2. Create a new filesystem (first peer only)

If no DFS filesystem exists yet, initialize one and install its managed mount.
Use a unique repository and mountpoint for every logical filesystem:

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

The installer creates a filesystem-specific core service and mount frontend,
starts the core first, and waits for both health and mount readiness. The core
advertises the filesystem to nearby peers. `dfs setup` currently joins an existing filesystem;
creation of the first peer is the explicit `init` plus installer flow above.

The mount frontend is a thin FUSE adapter over DFS's in-process Go core API.
There is no RPC or second process in the mounted read path, so cached annex
content remains a direct local read. The separate core daemon owns background
synchronization, QUIC service, pin hydration, cache maintenance, and health;
unmounting the frontend does not stop those services.

## 3. Join and approve

On the new peer, from the cloned DFS source directory, run the transactional
setup command:

```sh
./bin/dfs setup --name laptop --cache-limit 50GiB
```

Select one of the discovered filesystems. DFS prints a short request ID and
waits. On any existing online member, verify the peer name and ID, then approve
the request:

```sh
dfs --repo ~/.local/share/dfs/repository pair requests
dfs --repo ~/.local/share/dfs/repository pair approve <request-id>
```

DFS then clones over authenticated QUIC, reconciles reciprocal membership,
installs the platform service, mounts `~/dfs_storage`, and verifies health. No
long invitation or bearer secret is copied between machines.

Every completed step is recorded privately and atomically. If setup is
interrupted or a dependency is temporarily unavailable, retry it without a new
join request while the recorded request is still valid:

```sh
./bin/dfs setup --resume
```

To deliberately undo an incomplete setup, including the repository that this
transaction created, run:

```sh
./bin/dfs setup --abort
```

Use `--repository` and `--mountpoint` on the initial command to override their
defaults. `--filesystem` selects an unambiguous discovered name or ID without an
interactive menu. Setup state and locks are repository-specific, so multiple incomplete
or completed filesystem transactions do not collide. Resume and abort select
the transaction using `--repository`; use the same non-default repository path
you supplied initially. `--pair-port` selects an explicit local UDP port;
otherwise DFS uses the first free port beginning at 7843. The
lower-level `network join` and installer scripts remain available for debugging
and manual deployments.

To join a second active DFS filesystem on the same host, choose another
repository and mountpoint. The new instance receives a distinct service, logs,
health state, and managed port:

```sh
./bin/dfs setup \
  --repository ~/.local/share/dfs/repository-archive \
  --mountpoint ~/dfs_archive \
  --name laptop-archive --cache-limit 25GiB
```

Verify the peer:

```sh
dfs --repo ~/.local/share/dfs/repository health
dfs --repo ~/.local/share/dfs/repository health --cluster
dfs --repo ~/.local/share/dfs/repository peer list
```

`health` checks required commands and the platform FUSE dependency, then reports
the independent core plus filesystem identity, logical size and file count,
instance port, role, repository and metadata size, physical cache and disk use, content
holdings, pins, reconciliation, and timestamped peer observations.
`health --cluster` actively collects those details from all responding members,
compares namespace convergence, and verifies every directed peer edge. Use
`--json` with either form for complete machine-readable diagnostics; the normal
terminal view shortens transport errors and avoids repeating them per peer.

Pinning needs no manual file access. `dfs pin <path>` saves a peer-local policy;
`dfs pin --cluster <path>` saves a signed replicated policy for all current and
future peers. A running daemon hydrates the selected content automatically in
the background. Offline peers apply cluster pins after reconnecting, and
`health --cluster` reports each peer's pin scope and hydration, missing-content,
or capacity-constrained state. Use the corresponding `unpin` command with or
without `--cluster`; the two scopes are independent.

Ordinary reads of uncached annex files do not wait for full hydration. DFS
serves requested byte ranges over mutually authenticated QUIC and keeps
resumable sparse partials under the repository's private
`.git/dfs/range-cache`. Concurrent reads of duplicate annex content coalesce;
complete content is verified before atomic git-annex promotion. Explicit fetch
and pin operations still hydrate the entire selected content.

Health counts locally available content by logical path, while cache usage is
physical and deduplicated: it combines unique annex objects with only the byte
ranges retained in sparse partials and displays both components. Duplicating an
already-local file therefore adds another logical path without consuming the
file's full size again.

`health --cluster` verifies authenticated QUIC for every directed peer pair.
An unavailable member is reported without delaying healthy-peer operations or
trying another transport.

### Hostname changes create a new peer

DFS binds each peer identity to the machine hostname recorded during `init` or
`network join`. Changing that hostname invalidates the pairing; DFS refuses to
open or mount the repository and reports the old peer ID. It never silently
transfers the old identity to the renamed machine. Re-add it as a new peer:

1. Uninstall its managed mount with `scripts/install-macos.sh --uninstall <repository>`
   or `scripts/install-cachyos.sh --uninstall <repository>`.
2. On another cluster member, run
   `dfs --repo ~/.local/share/dfs/repository peer remove dfs-peer-<old-id>`.
3. Move the renamed machine's old repository aside as a backup, create a new
   `dfs setup` against an existing member into the original repository path.
   The join generates a new peer ID.
4. Reinstall the managed mount and verify it with `health --cluster` and the
   mounted-volume acceptance test.

The `.local` DNS suffix and hostname letter case are normalized, so `zeus`,
`ZEUS`, and `zeus.local` are the same hostname; an actual name change is not.

## Mounted-volume acceptance test

After installing or updating DFS, run the same live-mount acceptance suite on
every available macOS and Arch/CachyOS peer:

```sh
cd ~/dfs
make test-mount MOUNTPOINT=~/dfs_storage \
  PEERS='zeus.local:/Users/christian/dfs_storage iris.local:/Users/christian/dfs_storage'

# Equivalent direct invocation with a 45-second propagation deadline:
./scripts/test-mounted-volume.sh --sync-timeout 45 ~/dfs_storage \
  zeus.local:/Users/christian/dfs_storage \
  iris.local:/Users/christian/dfs_storage
```

The suite creates a uniquely named `.dfs-mount-test-*` directory inside the
mount, exercises common file and metadata operations, and removes that test
directory on success, failure, or interruption. For every supplied peer it
also verifies that an exact filename and its content are created, renamed,
updated, and deleted before the propagation deadline. The output reports the
observed number of seconds for every peer and operation. It does not modify
existing files. The default 45-second deadline allows one pass of DFS's
30-second synchronization schedule plus network latency. The peers must be
reachable through an independently configured administrative remote shell.
That channel only launches the cross-host test; DFS itself never uses it. Arch
requires the `attr` package for its extended-attribute check:

```sh
sudo pacman -S --needed attr
```

Run this suite after every source change that can affect mounted filesystem
behavior, as well as after replacing a managed daemon. If no second peer is
available, local filesystem checks can be run explicitly with
`TEST_MOUNT_FLAGS=--local-only`; this mode reports the synchronization checks as
skipped and must not be treated as a complete cross-peer acceptance test.

Run the cluster check while all peers you expect to validate are mounted and
advertising. Configured signed membership remains authoritative when mDNS is
unavailable. The command exits unsuccessfully if any directed authenticated
QUIC path is missing or fails.

See [README.md](README.md) for platform-specific dependencies, manual mounting, troubleshooting, and advanced configuration.
