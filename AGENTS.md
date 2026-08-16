# Repository agent instructions

- After finishing each task, verify the task's changes and commit them with Jujutsu (`jj`) using a concise, descriptive commit message.
- Keep unrelated user changes out of the task commit.
- Do not leave completed task changes in the working copy unless the user explicitly asks for them to remain uncommitted.
- After source changes that can affect mounted filesystem behavior, run
  `make test-mount MOUNTPOINT=<live-dfs-mount> PEERS='<host:remote-mount> ...'`
  on the current platform and on each available macOS/Arch peer, in addition
  to the Go and FUSE integration tests. Cross-peer synchronization checks are
  mandatory unless `TEST_MOUNT_FLAGS=--local-only` is explicitly supplied.
  The script creates and removes only its own isolated test directory.
