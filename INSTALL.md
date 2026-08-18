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

For a normal development checkout on each peer, use the automated local
workflow:

```sh
cd ~/dfs
./scripts/dev-upgrade.sh --dry-run
./scripts/dev-upgrade.sh
```

The script refuses dirty source state, updates directly from `origin/main`,
builds a native candidate, previews the transaction, upgrades every local DFS
service, and verifies the installed services and local health. It supports
both ordinary Git checkouts and the project's local Jujutsu workflow. Use
`--no-fetch` only to install the already checked-out commit.

Build the new binary in the DFS source checkout before replacing the running
version:

```sh
cd ~/dfs
git pull --ff-only
make build
./bin/dfs --version
./bin/dfs health
```

Do not copy `bin/dfs` over the installed binary. Preview and perform one
transactional host-wide upgrade instead:

```sh
./bin/dfs upgrade --from ./bin/dfs --dry-run
./bin/dfs upgrade --from ./bin/dfs --yes
```

The installed executable is shared, while every filesystem has its own service
pair. Upgrade inventories all of them, validates and atomically stages a
different candidate, preserves running and enabled state, verifies restarted
cores and mounts, and restores the previous executable and definitions on
failure. Use `dfs service repair --filesystem <selector>` when only service
definitions need repair.

After upgrade, verify every local service and the selected filesystem:

```sh
dfs service list
dfs health
dfs health --scope cluster
```

The platform installers are retained for initial setup and controlled recovery;
normal upgrades and service-definition repairs go through the CLI transaction.

DFS persists the logical filesystem ID in `.git/dfs/config.json`; service names
remain stable even if administrators later perform Git history maintenance.

The installer puts the daemon in a signed `DFS.app` helper with a stable bundle
identifier and keeps `~/Library/Application Support/DFS/bin/dfs` as a
compatibility link. On first install, allow **DFS** under **System Settings →
Privacy & Security → Local Network** so discovery can use `_dfs._udp`.
Service repair and upgrade reuse each filesystem's existing managed transport
port automatically. The installer-level port option is an internal setup and
recovery interface.

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

If no DFS filesystem exists yet, create it through the same recoverable setup flow used to join later peers. Use a unique repository and mountpoint for every logical filesystem:

```sh
./bin/dfs setup create --filesystem-name "Home Files" \
  --peer-name desktop --cache-limit 100GiB
```

The installer creates a filesystem-specific core service and mount frontend,
starts the core first, and waits for both health and mount readiness. The core
advertises the filesystem to nearby peers. The transaction can be resumed with `dfs setup resume` or rolled back with `dfs setup abort` if creation or installation is interrupted.

The mount frontend is a thin FUSE adapter over DFS's in-process Go core API.
There is no RPC or second process in the mounted read path, so cached annex
content remains a direct local read. The separate core daemon owns background
synchronization, QUIC service, pin hydration, cache maintenance, and health;
unmounting the frontend does not stop those services.

## 3. Join and approve

On the new peer, from the cloned DFS source directory, run the transactional
setup command:

```sh
./bin/dfs setup join --peer-name laptop --cache-limit 50GiB
```

Setup prints the discovery window and elapsed progress, then lists filesystems by display name. Select one of them. DFS resolves the history author identity from `--author-name` and `--author-email`, the author environment, or global Git configuration; interactive setup collects missing values and stores them only in the joined repository. It then prints a short request ID and waits. On any existing online member, verify the peer name and ID, then approve the request:

```sh
dfs peer requests
dfs peer approve <request-id>
```

DFS then clones over authenticated QUIC, reconciles reciprocal membership, installs the platform service, mounts `~/dfs_storage`, and verifies every directed connection between responding members. Unavailable accepted members are recorded as `PENDING` and reconcile when they return. No long invitation or bearer secret is copied between machines.

The default setup repository is `~/.local/share/dfs/repository`. After setup,
ordinary commands such as `dfs health`, `dfs peer optimize --scope cluster`, and
`dfs content pin Photos` select an enclosing repository or mounted path, or the
only installed filesystem. With several filesystems, use
`--filesystem <name|id|mountpoint|repository>` or `DFS_FILESYSTEM`; DFS never
chooses an arbitrary default.

Every completed step is recorded privately and atomically. If setup is
interrupted or a dependency is temporarily unavailable, retry it without a new
join request while the recorded request is still valid:

```sh
./bin/dfs setup resume
```

To deliberately undo an incomplete setup, including the repository that this
transaction created, run:

```sh
./bin/dfs setup abort
```

Use the advanced `--data-dir` and normal `--mountpoint` on the initial command to override their
defaults. `--filesystem` selects an unambiguous discovered name or ID without an
interactive menu. Setup state and locks are repository-specific, so multiple incomplete
or completed filesystem transactions do not collide. Resume and abort select
the transaction using `--data-dir`; use the same non-default data path
you supplied initially. `--transport-port` selects an explicit local UDP port;
otherwise DFS uses the first free port beginning at 7843. The
`--verification-timeout` flag controls how long setup waits for online members to acknowledge the topology; acknowledgement state survives a timeout and `dfs setup resume` retries only the remaining verification. Low-level repository, pairing, mount, and transport commands are hidden runtime and recovery interfaces.

To join a second active DFS filesystem on the same host, choose another
repository and mountpoint. The new instance receives a distinct service, logs,
health state, and managed port:

```sh
./bin/dfs setup join \
  --data-dir ~/.local/share/dfs/repository-archive \
  --mountpoint ~/dfs_archive \
  --peer-name laptop-archive --cache-limit 25GiB
```

Verify the peer:

```sh
dfs health
dfs health --scope cluster
dfs peer list
```

Administer every installed filesystem from its actual systemd or launchd definition:

```sh
dfs service list
dfs service show --filesystem "Home Files"
dfs service stop --filesystem "Home Files"
dfs service restart --filesystem "Home Files"
dfs service repair --filesystem "Home Files" --dry-run
dfs upgrade --from ./bin/dfs --dry-run
dfs upgrade --from ./bin/dfs --yes
dfs service uninstall --filesystem "Home Files"
```

Service selectors accept an unambiguous filesystem display name, peer name, ID
prefix, mountpoint, or repository path. Upgrade validates and stages a different
shared executable, preserves running and enabled state, verifies restarted
services, and rolls back failures. Repair only reinstalls service definitions.
Uninstall retains repository data, cached content, peer identity, membership,
and the shared executable.

`health` checks required commands and the platform FUSE dependency, then reports
the independent core plus filesystem identity, logical size and file count,
instance port, role, repository and metadata size, physical cache and disk use, content
holdings, pins, reconciliation, and timestamped peer observations.
`health --scope cluster` actively collects those details from all responding members,
compares namespace convergence, and verifies every directed peer edge. Use
`--output json` with either form for complete machine-readable diagnostics, including
the result plus a `HEALTH_DEGRADED` error on a nonzero degraded-health exit; the normal
terminal view shortens transport errors and avoids repeating them per peer.

Pinning needs no manual file access. `dfs content pin <path> --scope local` saves a peer-local policy;
`dfs content pin <path> --scope cluster` saves a signed replicated policy for all current and
future peers. A running daemon hydrates the selected content automatically in
the background. Offline peers apply cluster pins after reconnecting, and
`health --scope cluster` reports each peer's pin scope and hydration, missing-content,
or capacity-constrained state. Use the corresponding `unpin` command with or
without `--cluster`; the two scopes are independent.

Ordinary reads of uncached annex files do not wait for full hydration. DFS
serves requested byte ranges over mutually authenticated QUIC and keeps
resumable sparse partials under the repository's private
`.git/dfs/range-cache`. Concurrent reads of duplicate annex content coalesce;
complete content is verified before atomic git-annex promotion. Explicit fetch
and pin operations still hydrate the entire selected content.

When several peers can supply content, measure and persist source priorities
after installation or after a material network change:

```sh
dfs peer optimize --scope local
dfs peer optimize --scope cluster --yes
```

With local scope, only the current peer benchmarks its eligible sources and
updates its private result. With cluster scope, every responding peer performs
the same directed QUIC measurements and updates its own result. DFS retains an
interactive range-read order and a bulk hydration order until the corresponding
command is run again. Benchmarks use validated in-memory bytes and create no
user, Git, or annex content. `health` displays the local orders and timestamp;
`health --scope cluster` displays them for all responding peers and marks stale,
offline, or unmeasured sources.

Health counts locally available content by logical path, while cache usage is
physical and deduplicated: it combines unique annex objects with only the byte
ranges retained in sparse partials and displays both components. Duplicating an
already-local file therefore adds another logical path without consuming the
file's full size again.

`health --scope cluster` verifies authenticated QUIC for every directed peer pair.
An unavailable member is reported without delaying healthy-peer operations or
trying another transport.

### Hostname changes create a new peer

DFS binds each peer identity to the machine hostname recorded during setup.
Changing that hostname invalidates the pairing; DFS refuses to
open or mount the repository and reports the old peer ID. It never silently
transfers the old identity to the renamed machine. Re-add it as a new peer:

1. Uninstall its managed mount with `scripts/install-macos.sh --uninstall <repository>`
   or `scripts/install-cachyos.sh --uninstall <repository>`.
2. On another cluster member, run
   `dfs peer remove <old-id> --yes`.
3. Move the renamed machine's old repository aside as a backup, create a new
   `dfs setup` against an existing member into the original repository path.
   The join generates a new peer ID.
4. Reinstall the managed mount and verify it with `health --scope cluster` and the
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
