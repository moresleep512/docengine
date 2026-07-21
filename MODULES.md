# Docengine module notes

Docengine is a format-neutral local UTF-8 document core. No core package may
depend on Markdown, another file format, a renderer, Wails, or UI layout policy.
The module path is `github.com/moresleep512/docengine` and the current toolchain
target is Go 1.26.

## Dependency direction

```text
document/store ----+
document/save -----+--> document
recovery ----------+
```

The lower packages never import `document`. `document.Session` is the current
coordination facade and owns their lifecycle.

## `document/store`

The store represents logical content as immutable Pieces referencing byte
ranges in external `io.ReaderAt` sources. The initial Piece references the base
file; inserted Pieces reference journal payloads.

A persistent randomized treap caches subtree byte length, Piece count, and
optional newline totals. `ReplacePiece` splits at byte boundaries and clones
only changed paths. Earlier roots therefore remain readable and average edit or
coordinate traversal cost follows tree height rather than document length.

Important invariants:

- negative or overflowing ranges and missing sources are rejected before root
  publication;
- a no-op replacement preserves the root;
- `Snapshot` captures both root and Source bindings;
- split Pieces inherit the original treap priority;
- callers must keep all referenced Source handles alive for a Snapshot.

`document.sourceGeneration` provides that ownership through `SnapshotLease`.
The store itself does not close files.

## `recovery`

Recovery v2 is an append-only atomic-batch journal. There is no v1 decoder,
migration path, single-replacement frame, or root frame.

### Fingerprint

`Fingerprint` contains:

- base byte length;
- SHA-256 of the normalized resolved path;
- SHA-256 of the complete on-disk content, including a BOM.

Windows path hashing is case-insensitive; POSIX path hashing preserves case.
Modification time is not a durable identity field.

### File header

The fixed 96-byte `DOCLOG02` header is little-endian:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | magic `DOCLOG02` |
| 8 | 4 | version 2 |
| 12 | 4 | header size 96 |
| 16 | 8 | base byte length |
| 24 | 32 | normalized resolved-path SHA-256 |
| 56 | 32 | complete-content SHA-256 |
| 88 | 4 | reserved, zero |
| 92 | 4 | CRC-32C of bytes 0–91 |

Journal filenames use `.docengine-journal-v2`. Old suffixes are outside the
search namespace and are neither read nor modified.

### Batch record

Each durable transaction has a 64-byte `DOCJNL02` header followed by a payload:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | magic `DOCJNL02` |
| 8 | 2 | version 2 |
| 10 | 2 | flags, zero |
| 12 | 4 | header size 64 |
| 16 | 8 | first revision |
| 24 | 8 | nonzero edit group |
| 32 | 4 | operation count |
| 36 | 4 | operation record size 24 |
| 40 | 8 | payload length |
| 48 | 8 | reserved, zero |
| 56 | 4 | CRC-32C of header bytes 0–55 plus payload |
| 60 | 4 | reserved, zero |

The payload begins with one 24-byte `(start, delete length, insert length)`
record per operation, followed by inserted bytes in operation order. A batch is
limited to 256 operations and 1 GiB total payload. Replay publishes a `Batch`
only after the complete header, CRC, operation table, revision range, lengths,
and payload cursor validate.

An invalid or incomplete tail returns the last verified offset and never
exposes part of a transaction. `document` repairs that tail. A corrupt header,
base mismatch, or ambiguous set of v2 journals is quarantined and reported as a
typed `RecoveryOpenError` instead of silently discarded.

## `document/save`

Atomic save performs:

1. create a same-directory `.docengine-save-*.tmp`;
2. copy target permission bits;
3. write the optional BOM and stream the immutable Snapshot;
4. sync and close the temporary file;
5. run the final strong-content conflict check;
6. atomically replace the target;
7. on POSIX, open and sync the parent directory.

Windows uses `ReplaceFileW` with `REPLACEFILE_WRITE_THROUGH`. Base handles use
`FILE_SHARE_DELETE`, so old Snapshot readers survive replacement. Explicit
sharing, lock, and replacement-transient errors receive a bounded exponential
retry; permanent errors are returned immediately.

On POSIX, a rename can succeed before parent-directory sync fails. That result
is a `DurabilityError`: content is committed, but power-loss durability is
uncertain. `Session` records this state and a later clean `Save` retries the
directory sync. Uncommitted temporary files are removed at every earlier fault
boundary.

## `document`

### Opening

`Open` wraps `OpenContext(context.Background(), ...)`. Opening resolves and
pins the real target, opens a regular file, then scans the complete file once in
256 KiB chunks. The scan:

- validates UTF-8 across chunk boundaries;
- detects and excludes only the initial BOM from logical content;
- hashes all on-disk bytes;
- counts all newline styles;
- checks Context cancellation;
- verifies before/after handle and path identity.

`Metadata.Path` is the requested absolute path and `ResolvedPath` is the pinned
target. A symlink later redirected elsewhere does not change the save target.

### Transactions and history

Every replacement increments revision. `ApplyBatch` checks expected revision,
validates and stages at most 256 sequential operations on an isolated tree,
appends one recovery batch, builds a second tree using durable payload offsets,
and only then publishes content, pending operations, revisions, and one undo
entry. Validation, cancellation, journal, tree, or undo-store failures publish
nothing; post-append failures repair the journal to its previous batch boundary.

Inserted strings must be valid UTF-8 and no larger than 1 MiB. Deleted and
inserted history text is stored in `undo.store`; the current default quota is
256 MiB. Quota exhaustion clears both history stacks and starts a new epoch.

### Snapshot generations

A generation owns its base and journal handles. Snapshot leases increment the
generation reference count. Save can install a new generation while old leases
continue reading the retired one. Handles close and committed journals are
removed only after the last lease releases.

### Saving and conflicts

Save captures an immutable target revision without blocking subsequent edits.
Before streaming it rejects an obvious length change; immediately before
replacement it stably reads and hashes the complete current target. A different
hash returns `ErrExternalChange`, including same-length changes with a preserved
mtime. A timestamp-only change with identical bytes is allowed.

After replacement, the committed file is reopened as a new generation. Edits
that arrived during streaming are copied in their original groups into a new
v2 journal rooted at the saved content and replayed onto the new Tree.

If replacement committed but stat, reopen, Tree construction, new journal, or
rebase fails, `CommittedRevision` is still advanced and the Session enters a
permanent read-only fault state. `ReadAt`, `Snapshot`, `Metadata`, `Fault`, and
`Close` remain usable; edit, undo, redo, and save return `ErrFaulted` joined with
the cause. This prevents continued mutation on a partially rebound generation.

### Remaining policy

The core intentionally retains generic text policy—UTF-8, BOM, newline
metadata, revisions, ranges, and byte-oriented search foundations—but no format
semantics. Limits, sync interval, transient-directory ownership, compaction,
events, coordinate maps, virtualization, and indexing still require later
work. See [develop.md](develop.md).

## Verification

Every current package is held at 100% statement coverage. Tests include
platform-specific replacement and directory-sync faults, complete UTF-8 and
identity boundaries, every recovery batch truncation, transaction rollback,
concurrent save/rebase, post-commit fault behavior, snapshot lifetime, integer
overflow, randomized reference models, race runs, and nine fuzz targets.

The v0.3.0 release suite was completed on native Windows and Debian under WSL 2
on a native Linux temporary directory: every package reported 100% statement
coverage, three shuffled race runs passed, and all three fuzz targets ran for
at least 30 seconds on each platform.
