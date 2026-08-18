# DFS

DFS is an experimental, quota-aware distributed filesystem for Linux and
macOS. It presents the same complete file and directory tree on every device
while allowing each device to keep only a quota-limited subset of file content.
Git stores the namespace and history, git-annex stores content, and FUSE exposes
the result as a regular mounted drive.

Requested byte ranges of missing content are streamed when a file is opened;
explicit fetches and pins hydrate the complete object. Moving, renaming, and
organizing files therefore does not require their content to be present locally.

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
- Fetch content temporarily, pin it on one peer or across the cluster against
  automatic eviction, or safely evict local copies while retaining namespace
  entries. Pinning schedules hydration automatically on every targeted peer.
- Let LRU pruning enforce the configured target without dropping open or pinned
  files or violating git-annex copy-safety rules.

### Add and operate trusted devices

- Discover nearby DFS filesystems over mDNS/DNS-SD.
- Select a discovered filesystem, submit a signed expiring join request, and
  obtain one explicit approval from any online member without copying a token.
- Resume or abort an interrupted `dfs setup` transaction safely.
- Reconcile signed full-mesh membership, remotes, and trust pins automatically,
  including after an offline peer returns.
- Transfer Git metadata, git-annex content, membership, and diagnostics only
  over mutually authenticated QUIC.
- Stream requested ranges of uncached files over authenticated QUIC, retaining
  resumable sparse partials privately and promoting only verified complete
  objects into git-annex.
- Measure directed QUIC performance with `dfs peer optimize` and retain stable
  interactive-read and bulk-hydration source priorities locally; use
  `--scope cluster` to optimize every responding peer.
- Diagnose dependencies and every directed authenticated QUIC path with
  `health` and `health --scope cluster`.
- Inspect service, namespace, repository, cache, disk, content-policy,
  membership, and peer health with `dfs health`; use `--scope cluster` for an active
  whole-cluster check and namespace-convergence comparison. Health output lists
  every pinned file or directory with its scope, hydration status, logical size,
  and missing-file count.
- Run more than one active DFS filesystem on the same host. Each repository has
  independent state, mountpoint, service, logs, and UDP transport port.

### Protect and recover data

- Inspect Git history and restore an older path as a new, non-destructive commit.
- Preserve the last durably published file after interrupted writes or crashes
  and quarantine uncertain private staging state for inspection.
- Add optional encrypted S3-compatible git-annex storage for durable content.
- Add an optional bare Git relay so peers can exchange metadata without being
  online at the same time.

## Requirements and build

Each peer needs Go 1.26 or newer to build, Git, git-annex, and a FUSE runtime.
DFS peer communication uses authenticated QUIC and does not require a remote
login service or a file-copy utility.

Arch Linux/CachyOS:

```sh
sudo pacman -S --needed go git git-annex fuse3
```

Debian/Ubuntu:

```sh
sudo apt install golang-go git git-annex fuse3
```

macOS:

```sh
brew install go git git-annex
```

Install the current macFUSE package from [macfuse.io](https://macfuse.io/).
DFS currently uses macFUSE's VFS/kernel backend, not FSKit. Do not run the
optional `brew services start git-annex`; DFS itself controls synchronization.

Build and check the local dependencies:

```sh
make build
./bin/dfs health
```

The binary uses the native Go FUSE protocol implementation and does not link
against libfuse, but FUSE 3 or macFUSE is still required at runtime. See
[INSTALL.md](INSTALL.md) for complete platform setup, macOS permissions,
upgrading managed daemons, firewalls, and live acceptance testing.

## Create the first filesystem

Create, install, mount, and verify the first peer in one recoverable setup transaction:

```sh
./bin/dfs setup create --filesystem-name "Home Files" \
  --peer-name desktop --cache-limit 100GiB
```

Setup invokes the platform installer, which copies the binary and creates two filesystem-specific user
services: an independent core daemon and a FUSE frontend. systemd or launchd
starts the core first, then the mount, and restarts either after failure.

The service manager starts the independent core and mount frontend in order.
Stopping only the frontend does not stop synchronization, QUIC, pin hydration,
cache maintenance, or health.

## Add another peer

On the new device, from the DFS source checkout, run:

```sh
./bin/dfs setup join --peer-name laptop --cache-limit 50GiB
```

`dfs setup join` shows elapsed progress while it scans, lists nearby filesystems by display name, and asks you to select one. It resolves the history author identity from `--author-name` and `--author-email`, the author environment, or global Git configuration; if either value is missing, interactive setup collects it and stores it only in this DFS repository. Setup then prints a short request ID and waits. On any existing online member, review and approve that exact signed request:

```sh
dfs peer requests
dfs peer approve <request-id>
```

The new peer then joins over QUIC, completes reciprocal registration, installs the platform user service, mounts the default `~/dfs_storage`, and verifies every directed connection between online members. Each responding member must acknowledge the complete online topology; unavailable members are recorded as `PENDING` and reconcile when they return. No invitation secret is copied between machines.

Ordinary commands select an enclosing repository or mounted path, or the only
installed filesystem. If several are installed, select one explicitly with
`--filesystem <name|id|mountpoint|repository>` or `DFS_FILESYSTEM`; DFS never
silently chooses an arbitrary default.

If setup is interrupted, continue or roll it back:

```sh
./bin/dfs setup resume
./bin/dfs setup abort
```

Setup progress, locks, and pairing state are private and repository-specific.
For a non-default private data directory, pass the same advanced `--data-dir`
to the initial, resume, or abort command. `--mountpoint` selects another mountpoint and
`--transport-port` selects a local managed transport port. Otherwise setup uses the
first free UDP port beginning at 7843. `--verification-timeout` controls how long setup waits for online members to acknowledge the new topology; a timeout preserves the acknowledgements and resumes from verification with `dfs setup resume`.

Low-level repository, pairing, mount, and transport commands are private
runtime/recovery interfaces. They are intentionally absent from normal help.

### Pairing and membership security

The discovered advertisement supplies a certificate fingerprint. The new peer
pins it, sends a signed, peer-bound, expiring request, and polls the discovered
members for explicit approval. Approval creates an internal single-use secret
that is never copied by the user. DFS clones the repository over QUIC and
registers deterministic authenticated remotes in both directions.

Each admitted peer publishes a signed record in the dedicated
`refs/heads/dfs-membership` Git metadata ref. The approving member signs the
admission and endorses its roster, so one approval establishes the new peer's
full mesh. Periodic synchronization verifies the signature chain and repairs
missing remotes and trust pins.
`peer remove` publishes a signed revocation; revocations accepted locally remain
sticky even if shared history is later altered.

Private keys, trust pins, invitations, runtime state, cache data, staging, and
recovery data stay below `.git/dfs`. Replicated membership and revocation state
stays in dedicated Git metadata refs. DFS internal state never enters the Git
worktree or mounted logical filesystem; `.dfs/` and `.gitattributes` are not
used as internal-state transports.

The logical filesystem ID is persisted in that peer-private configuration when
the repository is initialized or joined. It therefore remains stable across
Git history maintenance and continues to select the same managed service.

Changing a machine's hostname invalidates its DFS identity. DFS refuses to
mount that repository rather than silently transferring the old identity; the
renamed machine must be removed and admitted as a new peer. Letter case and the
`.local` suffix are normalized and do not count as a hostname change.

Useful administration commands are:

```sh
dfs peer requests
dfs peer approve <request-id>
dfs peer reject <request-id>
dfs peer list
dfs peer check <peer-id>
dfs peer check --scope cluster
dfs peer remove <peer-id> --dry-run
dfs peer remove <peer-id> --yes
```

`peer list` reads the signed membership roster rather than incidental Git
remotes. Removing a peer previews and publishes its signed revocation, removes
the local transport, and cannot erase content the peer already copied.

## Run multiple filesystems on one host

Use a distinct repository and mountpoint for each filesystem. For example, a
second peer can join with:

```sh
./bin/dfs setup join \
  --data-dir ~/.local/share/dfs/repository-archive \
  --mountpoint ~/dfs_archive \
  --peer-name laptop-archive --cache-limit 25GiB
```

Each local filesystem receives filesystem-specific core and mount services,
separate health/runtime data, separate logs, and the first available transport
port. Installing or uninstalling one instance does not replace the others.

Update a source-built development peer directly from `origin/main` with:

```sh
./scripts/dev-upgrade.sh --dry-run
./scripts/dev-upgrade.sh
```

This builds natively, upgrades every local filesystem service transactionally,
and verifies local service and health state. Run it independently on each peer.

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

`service list` discovers actual systemd or launchd definitions. Selectors accept
an unambiguous filesystem display name, peer name, ID prefix, mountpoint, or
repository. `upgrade` validates and atomically stages a different shared
executable, preserves running and enabled state, verifies restarted services,
and rolls back failures. `service repair` only reinstalls definitions.
Uninstall retains repositories, cached content, peer identity, and membership.

## Work with files and local storage

Use normal filesystem operations through the mount:

```sh
cp report.pdf ~/dfs_storage/Documents/
mv ~/dfs_storage/Documents/report.pdf ~/dfs_storage/Archive/
rm ~/dfs_storage/Archive/old.pdf
```

Control local or cluster-wide content placement explicitly:

```sh
dfs content fetch Documents/report.pdf
dfs content pin Photos/Vacation --scope local
dfs content pin Shared/Reference --scope cluster --yes
dfs content unpin Photos/Vacation --scope local
dfs content unpin Shared/Reference --scope cluster --yes
dfs content evict Movies/large.mkv --dry-run
dfs cache show
dfs cache limit 75GiB
dfs cache prune --dry-run
```

`content fetch` caches content temporarily. A local pin creates a peer-private
policy; cluster scope writes a signed policy to DFS's replicated Git metadata ref.
Both forms notify the daemon and hydrate matching files automatically in the
background—opening or copying them manually is unnecessary. An offline peer
applies a cluster pin when it reconnects. `health` shows each pin as `LOCAL` or
`CLUSTER` and as `READY`, `HYDRATING`, or `CAPACITY-CONSTRAINED`; `health
--scope cluster` also exposes offline and incomplete peers. Local and cluster scopes
are independent, so removing one does not remove the other. `evict` delegates
safety checks to git-annex and refuses to remove content protected by either
scope. Open or pinned files and copy-safety rules can temporarily keep usage
above the configured limit.

### Optimize content sources

DFS uses two stable source orders when more than one peer may hold content:
`interactive` favors low time-to-first-byte for ordinary range reads, while
`bulk` favors sustained throughput for explicit fetches and automatic pin
hydration. Run optimization only on this peer, or coordinate it across every
responding peer:

```sh
dfs peer optimize --scope local
dfs peer optimize --scope cluster --yes
dfs peer optimize --scope cluster --yes --output json
```

The command performs repeated, bounded, authenticated QUIC measurements using
deterministic in-memory data. It does not read or hydrate user content and does
not create benchmark files. Local optimization replaces only the current
peer's private `.git/dfs/optimization.json`; cluster optimization asks each
responding peer to replace its own result. The terminal view shows live sample
progress, median and tail measurements, the directed cluster matrix, and final
orders. Offline or unmeasured trusted peers remain deterministic last-resort
sources at the bottom of both profiles.

Source orders do not adapt silently: reads, restarts, failures, and a peer
returning online leave them unchanged until the next explicit optimization.
Membership or endpoint changes mark a result stale and append new eligible
sources deterministically without rewriting it. Revoked peers are excluded,
known non-holders are temporarily skipped, failed transfers safely try the
next source, and an explicit `content fetch --source` selection is still honored.

## Inspect history and recover a path

```sh
dfs filesystem show
dfs history list Documents/report.pdf
dfs history restore <commit> Documents/report.pdf --dry-run
dfs history restore <commit> Documents/report.pdf
dfs history conflicts
```

Restore creates a new commit and never rewrites shared history. Git history
records namespace versions, but does not by itself retain old annex objects.
Copy versions to durable storage before allowing their final content copy to be
dropped.

## Check health and cluster connectivity

```sh
dfs health
dfs health --output json
dfs health --scope cluster
dfs health --scope cluster --output json
```

`health` checks all required commands and the platform FUSE dependency, validates
the private `.git/dfs/health.json` heartbeat, owner process, and mountpoint, then
displays the daemon's timestamped operational observation.
That observation includes network identity and role, instance port, logical file
count and size, repository/metadata/private-state sizes, physical cache and disk use,
local content holdings, pins, reconciliation state, outgoing reachability, and
actionable degraded-state guidance. When optimization has been run, it also
shows both source profiles, measurement status, and timestamp; cluster scope
shows those values for every responding peer. The daemon refreshes it periodically without
hydrating missing content. `health --scope cluster` actively queries every configured or
discovered member in parallel, reports the same metrics for each responding
peer, checks every directed connection, and compares online namespace tree IDs.
The human-readable view is a compact peer and connection summary with shortened,
deduplicated errors; `--output json` retains environment, service, and optional cluster
objects with complete diagnostic details. A degraded JSON response retains that
result alongside a stable `HEALTH_DEGRADED` error while exiting unsuccessfully. The command exits unsuccessfully when
dependencies are missing or the cluster is incomplete or inconsistent.

Content holdings count logical paths whose annex object is locally available;
duplicate paths can therefore share one deduplicated object. Cache usage is
physical unique annex-object content plus the byte ranges actually retained in
the sparse range cache, and reports both components separately. A duplicate of
an already-local file does not consume the file's logical size again.

`health --scope cluster` asks every known mounted peer to test every configured peer in both directions:
`desktop -> laptop` and `laptop -> desktop` are separate rows. Each read-only
probe tests mutually authenticated QUIC.

Important cluster statuses are:

- `OK`: authenticated QUIC works in that direction.
- `NOT_CONFIGURED`: the source peer has no direct remote for the destination.
- `FAILED`: the authenticated QUIC path failed.
- `UNREPORTED`: no diagnostic report could be obtained, or the peer runs an
  incompatible DFS transport version.
- `ONLY_LOCAL_PEER`: no other member or discovered peer was available, so there
  were no directed edges to test.

`NOT_CONFIGURED`, `FAILED`, and `UNREPORTED` make the command fail. Adjust
bounded probes when needed:

```sh
dfs health --scope cluster \
  --discovery-timeout 5s --peer-timeout 10s
```

## Services, logs, and recovery

For a repository, derive the managed instance ID with:

```sh
DFS_INSTANCE=$(sed -n 's/.*"filesystem_id": *"\([0-9a-f]*\)".*/\1/p' ~/.local/share/dfs/repository/.git/dfs/config.json | cut -c1-12)
```

On systemd (the core owns health and networking; the mount is a frontend):

```sh
dfs service show --filesystem "$DFS_INSTANCE"
journalctl --user -u "dfs-core-$DFS_INSTANCE" -f
dfs service restart --filesystem "$DFS_INSTANCE"
```

On launchd:

```sh
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.core.$DFS_INSTANCE"
launchctl print "gui/$(id -u)/io.bitbeamer.dfs.mount.$DFS_INSTANCE"
dfs service restart --filesystem "$DFS_INSTANCE"
tail -F "$HOME/Library/Logs/DFS/core-$DFS_INSTANCE.stderr.log"
```

Remove one managed service while retaining its repository and the shared
installed binary with:

```sh
dfs service uninstall --filesystem "$DFS_INSTANCE"
```

The command prints the retained repository path; cached content, peer identity,
and cluster membership are not deleted.

Managed services use JSON `info` logging. Runtime logging and FUSE-debug flags
belong to hidden service-manager and developer interfaces rather than the
public command surface.

Mount startup holds an exclusive repository session and performs conservative
recovery. Interrupted staging payloads, partial annex transfers, and uncertain
Git state are moved under `.git/dfs/recovery/<timestamp>/`. A stale session from
another hostname is never overridden automatically. A crashed same-host
session and disconnected FUSE endpoint are recovered automatically; use the
documented recovery procedure before invoking hidden runtime controls directly.

## Network and firewall requirements

mDNS normally remains within one LAN and can be blocked by guest isolation or
VLANs. A default-drop firewall must allow UDP 5353 for discovery and UDP for
each instance's managed port.
The first instance normally uses 7843, the next 7844, and so on. DFS continues
mounting with a warning if discovery or the listener cannot start.

## Optional metadata relay

A bare relay lets peers publish Git namespace and annex-location metadata at
different times. It does not store file content:

```sh
dfs peer relay set \
  https://git.example.com/dfs-metadata.git
dfs sync --mode metadata
```

## Optional S3 durability

git-annex reads the standard AWS credential environment variables:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...

dfs storage add s3 archive \
  --bucket my-dfs-bucket --region eu-central-1
dfs storage copy archive Documents
```

After metadata synchronization, another peer can enable the same remote with:

```sh
dfs storage enable archive
```

## Filesystem semantics

Writable opens use private copy-on-write files under `.git/dfs/transactions`.
DFS publishes a durable atomic transaction only after the final writable handle closes.
`flush` preserves the transaction until that close because macOS may share one
FUSE handle across multiple application descriptors. `fsync` durably checkpoints
a transaction without ending the open
handle; later writes may advance it. A no-op writable open produces no commit.
Advisory locks, atomic rename-overwrite, open-then-unlink, and renaming an open
writer follow POSIX behavior supported by FUSE.

Opening a file updates its cache-recency record once. Cached files read directly
from local git-annex storage. For an uncached file, FUSE requests only the byte
ranges the application reads over mutually authenticated QUIC, with bounded
read-ahead for sequential throughput. Finder and Quick Look can therefore read
headers, trailers, and previews without waiting for a complete large object.
Concurrent reads of names referencing the same annex key share one transfer.
Successful extents are retained as a resumable sparse file under
`.git/dfs/range-cache`, never in the logical filesystem. Once all extents are
present, DFS verifies the annex key and atomically promotes the object into
git-annex. Old inactive partials are evicted against the peer's cache limit.
Closing a streaming handle or stopping the mount frontend cancels its in-flight request,
and a failed source peer is skipped in favor of another trusted content holder.

After remote namespace changes, DFS invalidates kernel FUSE entries on every
platform. On KDE Plasma it also emits the standard `KDirNotify` directory
signals, so Dolphin refreshes remotely added, renamed, and deleted entries
without requiring a manual F5 reload.

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
Go FUSE protocol adapter
        |
        v
In-process core API (namespace, content, transactions, events, policy, health)
        |-- direct local reads: cached git-annex objects
        |-- range reads: cancellable authenticated QUIC streams
        |-- Git: namespace, history, signed membership and cluster-pin metadata refs
        |-- git-annex: content hashes, locations, safe copies
        `-- SQLite: peer-local pins, access, and filesystem metadata

Independent core daemon
        |-- synchronization, reconciliation, pins, cache, and health
        |-- mDNS + pinned TLS: discovery and approval
        |-- mutually authenticated QUIC: peer transport
        `-- S3: optional durable content
```

FUSE calls the Go core API in the mount process. Cached content therefore keeps
its direct local file-descriptor path, and no RPC, IPC, serialization, or
out-of-process hop sits in the mounted read path. The independently managed
daemon owns background synchronization and peer service lifecycle; mounting or
unmounting the adapter does not start or stop that daemon. The same core API is
the frontend boundary for future browser and native-platform adapters.

The underlying Git worktree is an implementation detail. A locked git-annex
symlink is presented as a regular file; opening missing content uses QUIC range
streaming when available and otherwise hydrates it, while a writable open uses
a private transaction.

## Current limitations

- mDNS discovery is LAN-local. Routed discovery and relay-assisted invitations
  are not implemented.
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

The test suite covers the frontend-neutral core API, repository synchronization,
conflict convergence, transaction publication and recovery, locks, `fsync`, rename/unlink behavior,
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
