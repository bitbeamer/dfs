# TODO

This backlog is ordered by risk. The first items prove that DFS preserves data and converges correctly under realistic Linux/macOS use; convenience features come later.

1. [ ] Implement transactional writes with actual dirty tracking and copy-on-write staging. Publish a file only after a successful `Write`, `Truncate`, or metadata mutation and atomic close; a read/write open that performs no mutation must not unlock, commit, or trigger sync. This is also required before enabling macFUSE's FSKit backend.
2. [ ] Add crash and interruption recovery. On mount startup, detect and recover or quarantine unfinished writes, interrupted annex operations, incomplete commits, stale locks, and abrupt unmounts without losing the last valid version.
3. [ ] Prove two-peer convergence under concurrent edits. Exercise Linux and macOS changing, renaming, moving, and deleting the same paths while online and offline; retain both versions on conflict and provide deterministic convergence after reconnecting.
4. [ ] Run real mounted-filesystem integration tests on macOS. Cover both Apple Silicon and Intel where practical, using Finder plus common CLI operations; keep FSKit disabled until item 1 is complete and tested separately.
5. [ ] Implement and test essential filesystem semantics. Prioritize advisory locks, `fsync`, flush/error propagation, atomic rename-overwrite, open-then-unlink, permissions, timestamps, extended attributes, case-only renames, and Unicode normalization across Linux and macOS.
6. [ ] Guarantee content durability before eviction. Define a replication policy, verify annex objects after transfer, prevent removal of the last required copy, and ensure historical versions remain retrievable from a peer or durable storage.
7. [ ] Make transfers robust to unreliable peers. Support resume, timeout/cancellation, integrity verification, clear offline behavior, and automatic retry without leaving misleading annex location metadata.
8. [ ] Harden concurrent mount and sync behavior. Prevent sync, quota pruning, hydration, and user writes from racing; reject or coordinate multiple DFS processes operating on the same repository.
9. [ ] Stress-test quota enforcement. Ensure open, pinned, newly written, partially fetched, and historically required content is never evicted incorrectly, including when the cache starts above its limit.
10. [ ] Secure peer enrollment and storage configuration. Add explicit peer identity verification, safe SSH defaults, secret handling, remote URL validation, and a documented threat model for peers, relays, and S3.
11. [ ] Add end-to-end fault-injection tests and a repeatable demo. Test network loss, peer shutdown, disk-full conditions, corrupt objects, process kills, and restart recovery; provide a scripted Linux/macOS MVP acceptance scenario.
12. [ ] Improve conflict inspection and resolution. Show the competing peers and versions, allow preview/export, and resolve by choosing, renaming, or preserving both without rewriting shared history.
13. [ ] Run mounts as managed background services. Add clean startup/shutdown, structured logs, health reporting, stale-mount cleanup, and launchd/systemd service definitions.
14. [ ] Improve diagnostics and observability. Make `status` and `doctor` report sync state, unavailable content, pending writes, conflicts, replication health, quota pressure, peer reachability, and actionable recovery commands.
15. [ ] Simplify peer pairing and discovery. Add invitation tokens or a pairing flow, automatic reciprocal peer registration, and optional mDNS discovery on trusted local networks.
16. [ ] Produce signed, versioned packages. Add reproducible release builds, checksums, Homebrew and Linux packages, macOS signing/notarization, upgrade guidance, and compatibility checks.
17. [ ] Add additional git-annex storage providers and replication-policy management after S3 durability is proven.
18. [ ] Consider a small GUI for mount state, peer health, conflicts, pinning, and cache usage after the CLI and data model stabilize.
