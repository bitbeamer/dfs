# Internal Go core API

`internal/core.API` is the stable in-process boundary between DFS core behavior and frontends. It is intentionally a Go interface, not an RPC protocol: callers pay no serialization, authentication, IPC, or process-hop cost. A future frontend may place its own protocol above this boundary when its platform requires another process.

The interface uses logical relative paths and neutral value types. It does not expose a repository path, Git or git-annex concepts, FUSE objects, transport identities, peer endpoints, or `.git/dfs` storage. `APIVersion` changes only for incompatible semantics; additive methods and fields retain the current major version.

## Behavioral contract

- Every operation that may wait accepts a context. Cancellation stops waiting and remote range transfer, but cannot undo a mutation whose atomic publish already completed.
- An `API` and independent read handles are concurrency-safe. A write transaction serializes calls on its own handle; separate transactions may run concurrently and their commits are atomic per path.
- Mutations accept an optional operation ID. Repeating the identical completed request returns its original result without applying it again. Reusing an ID for different input, or while the first write transaction is still active, returns `conflict`. Empty IDs request ordinary non-idempotent behavior.
- A write transaction is invisible until `Commit`, which atomically replaces the destination. `Abort` and `Close` discard unpublished bytes. `WriteAt`, `ReadAt`, and `Truncate` report partial progress according to the standard Go I/O contracts.
- `ReadHandle` provides caller-paced `io.ReaderAt` access. This supplies backpressure without a mandatory buffer or goroutine. Cached content exposes a borrowed local file descriptor so an in-process adapter can retain direct reads and zero-copy opportunities; remote content does not. An opaque content version lets a frontend refresh an already-open annex handle after an atomic backing replacement without learning annex paths.
- Events have one strictly increasing cursor per filesystem instance. Subscriptions resume strictly after a cursor. Cursor zero starts at the oldest retained event. A consumer that falls behind retention receives `event_gone` and must rescan the namespace before resubscribing; publishers never wait for consumers.
- Directory pages are ordered by normalized name. `After` is the last returned name, and `Next` is the cursor for the following page. A metadata-only rename does not read or hydrate file content.
- Advisory byte-range locks are scoped by logical path and kernel owner. Nonblocking conflicts return `conflict`; blocking acquisition observes context cancellation and resumes after the conflicting range is unlocked.
- Error codes are stable frontend-facing categories. `errors.As` retrieves `*core.Error`; wrapped operating-system and transport detail remains available for diagnostics. Frontends translate the code, not diagnostic strings, into platform errors.
- A `Service` is scoped to one filesystem. Instances have independent events, operation IDs, namespace, policy, health, and content state, even when several run in one process.

The contract and race tests live in `internal/core`. The pre-refactor performance gate is documented in [performance-baseline.md](performance-baseline.md).
