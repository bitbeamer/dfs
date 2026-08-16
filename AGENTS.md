# Repository agent instructions

- After finishing each task, verify the task's changes and commit them with Jujutsu (`jj`) using a concise, descriptive commit message.
- Keep unrelated user changes out of the task commit.
- Do not leave completed task changes in the working copy unless the user explicitly asks for them to remain uncommitted.
- Keep `README.md` and `INSTALL.md` synchronized with user-visible behavior.
  Organize README capabilities by DFS's main user use cases, not by the order in
  which features were implemented, and remove superseded limitations when a
  feature ships.
- Never put DFS internal state in the user-visible repository worktree or the
  mounted logical filesystem. Peer-local private/runtime state belongs under
  `.git/dfs`; replicated control-plane state belongs in dedicated Git metadata
  refs. Do not use tracked worktree paths such as `.dfs/` or `.gitattributes`
  rules as an internal-state transport.
- After source changes that can affect mounted filesystem behavior, run
  `make test-mount MOUNTPOINT=<live-dfs-mount> PEERS='<host:remote-mount> ...'`
  on the current platform and on each available macOS/Arch peer, in addition
  to the Go and FUSE integration tests. Cross-peer synchronization checks are
  mandatory unless `TEST_MOUNT_FLAGS=--local-only` is explicitly supplied.
  The script creates and removes only its own isolated test directory.
