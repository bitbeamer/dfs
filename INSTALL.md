# Install DFS on a New Peer

DFS is experimental. Start with test data and keep an independent backup.

## 1. Install and build

Each peer needs Go 1.26+, Git, git-annex, OpenSSH, rsync, and a FUSE
runtime.

### CachyOS/Arch Linux prerequisites

```sh
sudo pacman -S --needed go git git-annex openssh rsync fuse3
```

Enable the SSH server on a peer that will receive direct connections from
other peers:

```sh
sudo systemctl enable --now sshd
```

### macOS prerequisites

Install the command-line dependencies with Homebrew. macOS supplies the SSH
client and server.

```sh
brew install go git git-annex rsync
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

For direct inbound connections, enable **Remote Login** under **System Settings
→ General → Sharing** for the DFS account.

### Build DFS

After installing the platform prerequisites:

```sh
git clone https://github.com/bitbeamer/dfs.git
cd dfs
make build
./bin/dfs doctor
```

## Updating a managed installation from source

Build the new binary in the DFS source checkout before replacing the running
version:

```sh
cd ~/dfs
git pull --ff-only
make build
./bin/dfs --version
./bin/dfs doctor
```

Do not copy `bin/dfs` over the installed binary while the daemon is running.
Rerun the installer for the current platform. The installer stops the managed
mount, installs the newly built binary, reloads the service definition, starts
the mount, and waits for DFS to report healthy. Let the command finish; if it is
interrupted after stopping the old daemon, rerun the same installer command.

On CachyOS/systemd:

```sh
cd ~/dfs
./scripts/install-cachyos.sh \
  ~/.local/share/dfs/repository ~/DFS ./bin/dfs

DFS_INSTANCE=$(git -C ~/.local/share/dfs/repository rev-list --max-parents=0 HEAD | sort | head -1 | cut -c1-12)
systemctl --user --no-pager --full status "dfs-mount-$DFS_INSTANCE"
~/.local/bin/dfs --repo ~/.local/share/dfs/repository health
```

On macOS/launchd:

```sh
cd ~/dfs
./scripts/install-macos.sh \
  ~/.local/share/dfs/repository ~/DFS ./bin/dfs

DFS_INSTANCE=$(git -C ~/.local/share/dfs/repository rev-list --max-parents=0 HEAD | sort | head -1 | cut -c1-12)
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
"$HOME/Library/Application Support/DFS/bin/dfs" \
  --repo ~/.local/share/dfs/repository health
```

The installer puts the daemon in a signed `DFS.app` helper with a stable bundle
identifier and keeps `~/Library/Application Support/DFS/bin/dfs` as a
compatibility link. On first install, allow **DFS** under **System Settings →
Privacy & Security → Local Network** so discovery can use `_dfs._tcp`.

Homebrew is normally added to interactive macOS shells automatically. When
building through a non-interactive SSH command, make it explicit first:

```sh
export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
```

To make Homebrew available to every non-interactive zsh command permanently,
add the following to `~/.zshenv` on the Mac:

```sh
typeset -U path PATH
path=(/opt/homebrew/bin /usr/local/bin $path)
export PATH
```

`dfs doctor` also adds the standard Apple Silicon and Intel Homebrew binary
directories to its own dependency search path, so diagnostics do not depend on
interactive shell startup files.

The existing peer and the new peer must be able to reach each other over managed
QUIC and the SSH fallback. On a default-drop firewall, allow UDP 5353, both UDP
and TCP for each filesystem instance's selected managed port, and the SSH server
port (normally TCP 22) from the trusted LAN. The first instance normally uses
7843; subsequent automatic setups select the next free port and print it.

## 2. Create a new filesystem (first peer only)

If no DFS filesystem exists yet, initialize and mount one:

```sh
./bin/dfs init ~/.local/share/dfs/repository \
  --name desktop --network-name "Home Files" --cache-limit 100GiB
./bin/dfs --repo ~/.local/share/dfs/repository mount ~/DFS
```

Keep the mount running. It now advertises the filesystem to nearby peers. Run the next step in another terminal.

## 3. Create an invitation

On an existing, mounted DFS peer:

```sh
dfs --repo ~/.local/share/dfs/repository pair invite
```

Send the resulting `dfs2_...` invitation to the new peer through a trusted channel. It is a temporary secret.

## 4. Join and mount

On the new peer, from the cloned DFS source directory, run the transactional
setup command:

```sh
./bin/dfs setup --name laptop --cache-limit 50GiB
```

Paste the invitation when prompted and approve the displayed filesystem once.
DFS then prepares the repository, reconciles the reciprocal peer, installs the
platform service, mounts `~/dfs_storage`, and verifies health. The invitation
is read from standard input instead of appearing in shell history.

Every completed step is recorded privately and atomically. If setup is
interrupted or a dependency is temporarily unavailable, retry it without a new
invitation:

```sh
./bin/dfs setup --resume
```

To deliberately undo an incomplete setup, including the repository that this
transaction created, run:

```sh
./bin/dfs setup --abort
```

Use `--repository` and `--mountpoint` on the initial command to override their
defaults. Setup state and locks are repository-specific, so multiple incomplete
or completed filesystem transactions do not collide. Resume and abort select
the transaction using `--repository`; use the same non-default repository path
you supplied initially. `--pair-port` selects an explicit local TCP/UDP port;
otherwise DFS uses the first free port beginning at 7843. The
lower-level `network join` and installer scripts remain available for debugging
and manual deployments.

Verify the peer:

```sh
dfs --repo ~/.local/share/dfs/repository health
dfs --repo ~/.local/share/dfs/repository peer list
dfs --repo ~/.local/share/dfs/repository doctor --mesh
```

`doctor --mesh` verifies managed QUIC, the repository-restricted SSH fallback,
and ordinary passwordless, non-interactive SSH for every directed peer pair. A
working fallback with unavailable QUIC is shown as `SSH_FALLBACK`; a missing
account key or `known_hosts` entry is reported on the exact `FROM` → `TO` row as
`PASSWORDLESS_SSH_FAILED`. Verify ordinary SSH separately
with `ssh -o BatchMode=yes <peer>.local true` when repairing that status.

### Hostname changes create a new peer

DFS binds each peer identity to the machine hostname recorded during `init` or
`network join`. Changing that hostname invalidates the pairing; DFS refuses to
open or mount the repository and reports the old peer ID. It never silently
transfers the old identity to the renamed machine. Re-add it as a new peer:

1. Uninstall its managed mount with `scripts/install-macos.sh --uninstall <repository>`
   or `scripts/install-cachyos.sh --uninstall <repository>`.
2. On another mesh member, run
   `dfs --repo ~/.local/share/dfs/repository peer remove dfs-peer-<old-id>`.
3. Move the renamed machine's old repository aside as a backup, create a new
   invitation on an existing member, and run `network join` into the original
   repository path. The join generates a new peer ID.
4. Reinstall the managed mount and verify it with `doctor --mesh` and the
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
reachable through non-interactive SSH. Arch
requires the `attr` package for its extended-attribute check:

```sh
sudo pacman -S --needed attr
```

Run this suite after every source change that can affect mounted filesystem
behavior, as well as after replacing a managed daemon. If no second peer is
available, local filesystem checks can be run explicitly with
`TEST_MOUNT_FLAGS=--local-only`; this mode reports the synchronization checks as
skipped and must not be treated as a complete cross-peer acceptance test.

Run the mesh check while all peers you expect to validate are mounted and
advertising. It exits unsuccessfully if any discovered peer lacks a direct
remote or any directed Git/SSH connection fails.

See [README.md](README.md) for platform-specific dependencies, manual mounting, troubleshooting, and advanced configuration.
