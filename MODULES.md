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
document/coordinate+
document/virtual --+
```

The lower packages never import `document`. `document.Session` is the current
coordination facade and owns their lifecycle.

## `document/virtual`

The virtual package is a format-neutral layer above immutable `ReaderAt`
sources. `Build` scans a revision exactly once and creates deterministic
logical Pages. Boundaries prefer the first LF at or after
`TargetPageBytes`; a long line is forced at the last UTF-8 boundary no later
than `MaximumPageBytes`. Empty input still has one `[0,0)` Page. LF alone
advances the logical line, matching `document/coordinate`.

`Pager` is permanently bound to one revision. `BuildOwned` transfers a Source
lease, and every concurrent `Close` waits for the same task barrier and returns
the same release result. A Pager created through `Session.VirtualPager` owns a
Snapshot lease, so later Session edits or saves cannot change its content.

Fragment state is immutable and published with a generation compare-and-swap:

- `IndexedThrough` is a byte watermark; Pages below it are marked analyzed
  even when no Fragment covers that gap;
- Fragments are ordered, non-overlapping, UTF-8-aligned ranges with unique
  opaque IDs and DataKeys;
- key strings are cloned before publication, and their actual retained byte
  total is bounded independently of the Fragment count;
- `Complete` may become true only at EOF, while EOF may remain explicitly
  incomplete;
- Provider callbacks run without a Pager or Session lock. A slow result loses
  the generation CAS instead of overwriting a newer publication.

`Measure` is a non-negative `int64` fixed-point quantity whose scale belongs to
the host. Prefix sums are checked for overflow. A Fragment's Measure remains
atomic: continuation Pages repeat its interval instead of inventing a
byte-proportional layout. Byte, Fragment-ID, and Measure windows support
asymmetric overscan and enforce hard byte, Page, distinct-Fragment, and Measure
budgets. If a whole Fragment exceeds a request budget, callers can page through
it by byte or select an explicit continuation.

Page payloads use a strict byte-capacity LRU. Cache hits and returned values are
copied so callers cannot mutate cached content. `CacheBytes` covers resident
LRU payloads; transient active-task copies are bounded separately by
`MaximumTasks × Window.Bytes`; copies retained by the host after a call returns
belong to its own memory budget. The same task semaphore provides immediate
`ErrBusy` backpressure. Provider callbacks may inspect `Stats`, but must not
synchronously invoke task-bearing operations or `Close` on the same Pager.
Owned Source closers must not re-enter their Pager.

Every `PageKey` contains an opaque issuing-Pager identity in addition to its
revision, Fragment generation, index, and byte range. Copying a key preserves
its capability, while reconstructing one or passing it to a different Pager is
rejected; equal revision numbers are not treated as proof of equal Source
identity.

## `document/store`

The store represents logical content as immutable Pieces referencing byte
ranges in external `io.ReaderAt` sources. The initial Piece references the base
file; inserted Pieces reference journal payloads.

A persistent randomized treap caches subtree byte length, Piece count, and
optional newline totals. `ReplacePiece` splits at byte boundaries and clones
only changed paths. Earlier roots therefore remain readable and average edit or
coordinate traversal cost follows tree height rather than document length.

The zero-value `store.Options` enables structural maintenance at
`DefaultAutoCompactPieces` (4,096 Pieces). Reaching the trigger coalesces only
logical neighbors backed by contiguous ranges of the same Source. If a pass
cannot reclaim anything, the next trigger advances by another threshold, so a
permanently non-coalescible tree does not pay an O(Pieces) scan on every edit.
Dropping below the base threshold resets the trigger. Hosts can configure or
disable this policy through `NewWithOptions`/`NewWithBasePieceOptions`.
`Tree.Stats` atomically reports byte/Piece/line summaries, the effective and
next thresholds, and the automatic-compaction count.

Important invariants:

- negative or overflowing ranges and missing sources are rejected before root
  publication;
- a no-op replacement preserves the root;
- `Snapshot` captures both root and Source bindings;
- split Pieces inherit the original treap priority;
- callers must keep all referenced Source handles alive for a Snapshot.
- automatic and manual compaction publish a fresh root only after the complete
  replacement tree exists; previously issued roots and Source bindings remain
  unchanged.

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
base mismatch, or ambiguous set of matching v2 journals is quarantined and
reported as a typed `RecoveryOpenError` instead of silently discarded. Save may
temporarily leave old- and new-base journals under the same path namespace;
open reads every strong fingerprint and proceeds only when exactly one
candidate matches the current complete base. Proven retired candidates are
quarantined. Zero matches and multiple matching candidates still block open.

`Journal.Size` reports physical bytes, `BatchEncodedSize` performs allocation-
free exact growth validation, and `BatchAppendResult.EndOffset` reports the new
verified end. Replay allocates one 64 KiB CRC buffer per complete scan and
reuses it across batches.

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

## `document/coordinate`

The coordinate package is format-neutral and depends only on standard-library
I/O contracts. `Build` streams a UTF-8 `Source` into immutable checkpoints
bound to one revision; checkpoints fall only on rune boundaries and retain
byte, rune, line, and column totals. Query APIs convert byte offsets, rune
offsets, and line/column positions while reading at most one checkpoint window.
LF advances the logical line; CR is an ordinary rune, matching the core's
existing newline metadata without inventing visual-layout semantics.

`BuildOwned` transfers a Source's lifetime to the index. Session uses it with a
Snapshot lease, so an index remains readable across later edits or saves and
releases the retired generation on `Close`. Build and query paths are
Context-aware, validate source length/read consistency, reject non-boundary or
overflowing coordinates, and never cache complete document content.

`ChangeMap` is an immutable sequence of edits expressed in the coordinate
space produced by each preceding edit. It carries before/after revisions and
lengths, transforms Anchors with explicit before/after affinity, transforms
ranges, composes adjacent maps, and can be inverted for history traversal.
`ApplyBatch`, `Undo`, and `Redo` return the committed map in `ApplyResult`;
no-op batches return an identity map.

`Rebuild` creates a new immutable Index from a previous Index, a new Source,
and the exact ChangeMap chain between them. It verifies before/after revision
and length, finds the minimum start across sequential edits, copies checkpoints
only through the last checkpoint at or before that stable prefix, and scans the
new Source from there. It never shifts an old suffix whose rune, line, or column
state cannot be proved. The old and new Index own independent Source lifetimes.
`RebuildOwned` and `Session.RebuildCoordinateIndex` preserve the same lease and
failure-cleanup guarantees as full construction. Stats expose reuse and scan
extent so hosts can choose when a full rebuild is cheaper.

Indexes may carry an opaque `Lineage`. Session overrides caller Options with a
private lineage and requires it for both explicit rebuild and managed refresh,
preventing a same-revision foreign Index from seeding incorrect checkpoints.
`Session.RefreshCoordinateIndex` captures the retained ChangeMap and current
Snapshot under one Session read lock, then releases the lock before scanning.

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
- verifies before/after handle and path identity, including the OS change
  generation (`ctime` on supported POSIX systems and `ChangeTime` on Windows).

The change generation detects an in-place, same-length rewrite even when its
mtime is restored, without a second content pass. Linux/BSD reuse metadata
already returned by `stat`; Windows adds two constant-time handle queries.

`Metadata.Path` is the requested absolute path and `ResolvedPath` is the pinned
target. A symlink later redirected elsewhere does not change the save target.

### Configuration and directory ownership

`OpenOptions` resolves into an immutable `SessionConfig`. The configurable
limits are maximum operations per batch, bytes per insertion, undo-store bytes,
recovery-journal bytes, retained Session events, retained ChangeMaps, Anchors
per transform batch, and journal sync interval. The default hard journal limit
is 4 GiB. `AutoCheckpointJournalBytes` is separate and zero by default because
enabling it authorizes background saves; a nonzero threshold cannot exceed the
hard limit. Negative values and limits beyond the v2 journal or in-memory
envelopes are rejected before the base file is opened.

Explicit RecoveryDir and SessionDir paths are shared unless marked owned. An
omitted RecoveryDir uses the shared process-temporary recovery namespace; an
omitted SessionDir creates a unique owned directory. Every undo store is a
unique `.docengine-undo-*.store`, so concurrent Sessions may share a directory.
Close removes its undo file. Owned directories are removed only after `Lstat`
confirms a directory, `ReadDir` confirms it is empty, and all owned handles have
retired. Dirty journals and unknown entries are never recursively removed.

Owned Session directories also contain `.docengine-session-v1`. Its file lock
is held for the Session lifetime. Automatic startup cleanup scans only old
directories, while `ReclaimStaleSessionDirectories` accepts an explicit cutoff;
both require a valid marker, an acquirable lock, and exclusively recognized
regular undo entries. Active locks, symlinks, malformed markers, and unknown
files are preserved. Cleanup failures elsewhere in the shared root do not block
a new Session, while an active owner of the exact requested directory returns
`ErrSessionInUse`.

### Transactions and history

Every replacement increments revision. `ApplyBatch` checks expected revision,
validates and stages at most 256 sequential operations on an isolated tree,
appends one recovery batch, builds a second tree using durable payload offsets,
and only then publishes content, pending operations, revisions, and one undo
entry. Validation, cancellation, journal, tree, or undo-store failures publish
nothing; post-append failures repair the journal to its previous batch boundary.
The exact v2 encoded size is checked before creating or appending a journal, so
`MaxJournalBytes` rejection changes neither revision, content, nor filesystem.

Inserted strings must be valid UTF-8 and no larger than 1 MiB. Every edit start
and end must be a rune boundary in the state produced by preceding batch edits,
so a committed Session can never split a multibyte character. Recovery replay
checks the same invariant. Deleted and inserted history text is stored in
`undo.store`; the current default quota is 256 MiB. Quota exhaustion clears
both history stacks and starts a new epoch.

### Events and close barrier

`Session.Subscribe` returns a per-subscriber bounded channel. The event hub
serializes publication, atomically joins retained history to live delivery, and
never waits for a consumer. On overflow it drains stale pending deliveries,
keeps the newest event, and sets `Dropped` to the exact number of events between
the subscriber's last observed sequence and that delivery. Consumers must
rebuild derived state from a current Snapshot after any nonzero `Dropped`.

`AfterSequence` resumes after a stored cursor. If the cursor precedes retained
history, the first available delivery reports the missing prefix; a cursor in
the future is rejected. `FutureOnly` skips history. Successful open and journal
recovery, each non-empty Apply/Undo/Redo commit, and final close are published;
failed and no-op transactions publish nothing. Change events contain the same
immutable `ChangeMap` returned by the transaction.

An actual save attempt additionally publishes start, bounded byte progress,
and saved or failed events correlated by `PersistenceProgress.OperationID`.
The committed flag separates errors before atomic replacement from permanent
post-commit rebinding faults. A committed POSIX `DurabilityError` is carried by
`EventSaved`, not misreported as an uncommitted failure. Background and final
recovery-WAL Sync failures publish one transition event, repeated failures are
coalesced, and the first successful Sync or clean save publishes restoration.
`Metadata.RecoveryDurabilityUncertain` makes the current state reconstructible
even if the transition event was dropped. Final Sync failure is included in
the shared `Close` result.

The final close event survives subscriber overflow. Its channel is then closed.
The first `Session.Close` retires resources, while all concurrent Close callers
wait on one barrier and return the same joined cleanup result. Explicit
subscription close is idempotent and races safely with publication and Session
close.

When `AutoCheckpointJournalBytes` is enabled, accepted edits schedule a
background `CommitAtLeast` after the physical threshold. The one-slot request
queue coalesces work; a failed checkpoint moves the next trigger forward by a
full threshold rather than retrying in a hot loop. Automatic saves publish the
normal persistence events. `RecoveryStats` atomically exposes journal bytes,
the hard and automatic thresholds, queued/active work, and completed
checkpoints. Close stops scheduling before waiting for the shared save mutex,
so an active automatic checkpoint cannot deadlock resource retirement.

### Retained changes, ranges, and annotations

Session retains a dedicated bounded ring of committed Apply/Undo/Redo
ChangeMaps. The default is 256 transaction maps and the hard maximum is 4,096;
multi-operation batches remain one entry and their interior revisions are not
observable boundaries. Recovery starts a fresh window at the recovered
revision. Save changes Source generations but neither revision nor retained
maps.

`ChangesBetween` composes forward boundaries or returns an inverse map for a
reverse query. A revision older than the ring returns
`ErrChangeHistoryExpired`; a future or atomic-batch-interior revision returns
`ErrRevisionUnavailable`. Both use `ChangeHistoryError`, which records the
requested and retained windows. Stats and retained maps remain available after
Session close.

`TransformAnchors` and `TransformRanges` preserve input order and validate the
entire input before returning output. Session applies both a configured count
limit and a fixed checked work budget, and releases its lock before the pure
transformation loop. Invalid input, inverted endpoint affinity, and budget
failures return no partial result. `coordinate.Annotation[T]` associates an
opaque host payload with an anchored range; the core copies but never
interprets that value.

### First-generation compaction

`store.Tree.Compact` coalesces only logical neighbors backed by contiguous
bytes in the same Source. It does not read source bytes, mutate existing roots,
or invalidate immutable Snapshots. The undo store compactor copies unique live
references to a replacement temporary store, switches only after all copies
succeed, remaps both history stacks, and reports retired-file cleanup errors
without losing the valid replacement mapping.

`Session.Compact` always runs Piece and undo compaction. Journal compaction is
selected explicitly with `CompactOptions.CheckpointJournal`: the Session saves
the selected revision, installs a new Source generation, and retires the old
append-only WAL after Snapshot leases release. Docengine never rewrites an
uncommitted WAL in place because a collapsed batch could not preserve both
revision boundaries and crash atomicity.

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

After streaming but before replacement, Session takes a short mutation barrier,
performs the final identity scan, writes every edit newer than the target into
a journal fingerprinted to the replacement content, syncs that journal and its
parent directory, and then keeps the barrier through atomic replacement.
Even when there is no newer edit, an empty replacement journal is prepared.
After replacement, the committed file is reopened as a new generation and the
already-durable pending groups are attached to the new Tree.

This ordering closes the process-crash window between base replacement and
journal rebasing. A crash before replacement leaves the old base and old
journal as the unique matching pair; a crash after replacement leaves the new
base and prepared journal as the unique pair. A normal clean close removes the
empty current journal, and successful reopen quarantines the proven retired
candidate.

New-journal creation, source reads, append, file Sync, and recovery-directory
Sync now fail before replacement and leave the Session writable on its old
generation. If replacement commits but stat, reopen, Tree construction, or
prepared-journal installation fails, `CommittedRevision` is still advanced and
the Session enters a permanent read-only fault state. `ReadAt`, `Snapshot`,
`Metadata`, `Fault`, and `Close` remain usable; edit, undo, redo, and save return
`ErrFaulted` joined with the cause. This prevents continued mutation on a
partially rebound generation.

### Remaining policy

The core intentionally retains generic text policy—UTF-8, BOM, newline
metadata, revisions, ranges, and byte-oriented search foundations—but no format
semantics. Page/Fragment virtualization is now implemented in
`document/virtual`; the remaining higher layers are persistent search,
multi-source composition, file-watcher integration, and production
observability. See [develop.md](develop.md).

## Verification

All six current packages are held at 100% statement coverage. Tests include
platform-specific replacement and directory-sync faults, complete UTF-8 and
identity boundaries, every recovery batch truncation, transaction rollback,
concurrent save/rebase, post-commit fault behavior, snapshot lifetime, integer
overflow, randomized reference models, race runs, and twenty-three fuzz targets.
Event-specific tests exercise resumable history, exact overflow accounting,
save progress and failure phase, WAL Sync failure/restoration, concurrent
publish/unsubscribe, final-event delivery, and the close barrier. Lifecycle and
compaction tests cover marker locks, conservative orphan reclamation, cleanup
faults, live undo remapping, journal checkpoints, and Snapshot preservation.
Recovery/Save tests additionally cover exact quota rejection, automatic
checkpoint backoff, replacement-journal file/directory durability, and real
child-process exits on both sides of replacement with concurrent edits.
Virtualization tests cover UTF-8 Page partitioning, analyzed gaps, incomplete
watermarks, continuation Pages, byte/Fragment/Measure affinity, all four
budgets, cache ownership, task backpressure, generation races, provider
failures, and concurrent Close barriers. Four additional fuzz targets compare
Page reconstruction, Fragment windows, and generation state with reference
invariants. Incremental-index tests compare every byte, rune, and line coordinate with a
fresh full build across randomized sequential UTF-8 edits.
Change-history state-machine fuzzing covers bounded eviction, unavailable batch
interiors, forward/reverse maps, metadata, and Anchor equivalence.

The v0.3.0 release suite was completed on native Windows and Debian under WSL 2
on a native Linux temporary directory: every package reported 100% statement
coverage, three shuffled race runs passed, and all three fuzz targets ran for
at least 30 seconds on each platform.

The completed v0.4 release suite was checked on native Windows and in a WSL
native-Linux directory. All five packages retained 100% statement coverage,
three shuffled race runs passed, and the nine affected Session, event,
change-history, and coordinate fuzz targets passed 10-second runs on both
platforms.

The initial v0.5.0 implementation was checked on native Windows and all six
Linux test binaries cross-compiled. For v0.5.1, the full suite was run on
native Windows and Debian under WSL 2 from a native Linux `/tmp` directory:
all six packages reached 100% statement coverage, three shuffled race runs
passed across the repository, and four virtualization fuzz targets plus the
Session/Pager lifecycle fuzz target passed 10-second runs on both platforms.

For v0.5.2, native Windows and Debian under WSL 2 both retained 100% statement
coverage in all six packages and passed three shuffled race runs. The four
Piece Tree fuzz targets ran for 30 seconds each on both systems, the automatic
compaction boundary suite passed 100 consecutive Windows runs, and all four
store benchmarks executed on both platforms.

For v0.5.3, native Windows and Debian under WSL 2 both retained 100% statement
coverage in all six packages and passed three shuffled race runs. The four
recovery fuzzers plus concurrent-save, crash-recovery, and journal-quota
fuzzers each ran for 30 seconds on both systems. The real child-process crash
matrix passed 20 consecutive runs per platform, checkpoint/quota boundary
tests passed 100 consecutive runs, and all committed Recovery/Save/Session
benchmarks executed on both.
