# DFS

DFS is an experimental, quota-aware distributed filesystem for Linux and macOS. It combines Git's shared namespace and history with git-annex's content-addressed storage and exposes the result as a regular FUSE drive.

Every peer sees the complete file and directory tree. File content is downloaded only when opened, explicitly fetched, or pinned. Moves and renames therefore work even when the content is not stored locally.

> [!WARNING]
> This is an MVP. Use a separate test dataset and keep an independent backup. Linux is exercised by the integration suite; the macOS build is compile-checked but still needs broader application-compatibility testing.

## What works

- A complete Git-backed namespace on every peer.
- Direct Git/git-annex peers over SSH or a local path.
- Optional bare Git metadata relay.
- On-demand hydration when an annexed file is opened.
- Writes committed only after all writable mount handles have closed.
- Pin, fetch, unpin, safe eviction, and LRU quota enforcement.
- Git history and non-destructive restore commits.
- Encrypted S3-compatible git-annex storage.
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
sudo apt install golang-go git git-annex openssh-client rsync fuse3
```

macOS:

```sh
brew install go git git-annex
```

Install the current macFUSE package from [macfuse.io](https://macfuse.io/). Direct SSH access to a macOS peer also requires enabling Remote Login.

## Build

```sh
make build
./bin/dfs doctor
```

The binary uses the native Go FUSE protocol implementation and does not link against libfuse. FUSE or macFUSE is still required at runtime.

## Two-peer quick start

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

The mount process runs automatic metadata sync after completed transactions and every 30 seconds. Keep it running in a terminal for the MVP. Press Ctrl-C to cleanly unmount and stop it, or use `dfs unmount ~/DFS` from another terminal. SIGTERM uses the same clean shutdown path. A later `dfs mount` automatically detaches a disconnected DFS/FUSE endpoint left behind by a crashed process; it does not replace a healthy existing mount.

### Transactional writes

Writable opens use copy-on-write files under the repository's private `.dfs/staging` directory. Reads through the mount see staged content, while the locked git-annex entry is left untouched until the final writable handle successfully flushes or closes. DFS publishes a dirty staging file with a same-filesystem atomic rename and then schedules annexing and synchronization. A writable open that performs no write, truncate, or handle-level metadata mutation discards its staging copy without changing Git or triggering sync.

DFS preserves the mounted file's visible inode and timestamps while git-annex replaces the published regular file with its internal symlink. This prevents editors such as Vim from reporting a false external change during save. Unfinished staging files are not yet recovered automatically after a crash; that remains separate recovery work.

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
```

Restore creates a new commit and does not rewrite shared history.

## Architecture

```text
applications / Finder
        │
        ▼
Go FUSE mounted view
        │ open, close, rename, delete
        ▼
DFS transaction and quota scheduler
        ├── Git: namespace, history, merges
        ├── git-annex: hashes, locations, safe copies
        ├── SQLite: pins and last-access times
        ├── SSH/rsync: direct peer content
        └── S3: optional durable content
```

The underlying Git working tree is an implementation detail. `.git` and `.dfs` are hidden from the mounted view. A locked git-annex symlink is presented as a normal file; opening missing content runs `git annex get`, and opening it for writing first hydrates it into a private copy-on-write transaction.

## MVP limitations

- Peer discovery and pairing are manual; mDNS is not implemented yet.
- The conflict command lists conflicts but does not provide a full conflict-resolution UI.
- History does not itself retain old annex objects. Replicate versions to durable storage before allowing their last copy to be dropped.
- Open-file locking, memory mapping, sparse files, case-only renames, Unicode normalization, and large creative applications need more cross-platform stress testing.
- The mount process is foreground-only and there is no GUI or service installer yet.
- Cloud storage is S3-compatible only in the CLI; additional providers can be added through git-annex special remotes later.

## Development

```sh
make test
make test-integration  # mounts a temporary FUSE filesystem
```

The normal test suite includes a real two-peer Git/git-annex flow. The integration target additionally verifies copy-on-write publication, no-op writable opens, multiple writable handles, stable visible metadata across annex sync, and repeated Vim saves through a real FUSE mount.

## License

MIT
