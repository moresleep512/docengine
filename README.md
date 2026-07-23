# Docengine

[简体中文](README_zh.md) · [Development architecture and roadmap (Chinese)](develop.md) ·
[Implementation history and design decisions (Chinese)](modules_develop.md)

Docengine is an experimental Go core for editing large local UTF-8 documents
without loading the complete document into memory. It was extracted from the
TypeMD backend and is now an independent module:

```go
module github.com/moresleep512/docengine
```

## Position

Docengine is a local document orchestration kernel, not an editor, renderer, or
application server. The core understands bytes, UTF-8, ranges, revisions, and
immutable snapshots. It must not understand Markdown, JSON, source-code syntax,
or any other document format.

It currently provides:

- a disk-backed persistent Piece Tree with immutable snapshots;
- revision-checked atomic edit batches and disk-backed undo/redo;
- a v2 append-only crash journal whose only durable edit unit is a batch;
- full-file UTF-8 validation and SHA-256 base identity;
- UTF-8-boundary-safe transactional edits and recovery replay;
- streaming, conflict-checked atomic saves;
- POSIX parent-directory synchronization and Windows `ReplaceFileW` with
  write-through replacement plus bounded transient-error retry;
- symlink-target pinning and explicit post-commit fault handling;
- revision-bound byte/line/rune coordinate indexes with bounded LRU query
  caching and proven ChangeMap-driven checkpoint prefix/suffix reuse;
- sequential ChangeMaps and affinity-aware Anchors returned by edits,
  undo, and redo;
- bounded Session-managed ChangeMap history, linear chain composition,
  forward/reverse revision queries, lineage-checked index refresh, and
  cancellable atomic batch Anchor/range transforms with opaque annotations;
- bounded, resumable Session event streams with precise slow-consumer loss
  reporting, save progress, recovery-WAL durability transitions, and a
  concurrent close barrier;
- resolved Session resource limits, journal sync cadence, and explicit shared
  or owned runtime-directory policies;
- one cross-generation budget for host-owned Snapshot, coordinate Index, and
  virtual Pager leases, with atomic lifecycle statistics;
- cancellable save/commit/undo/redo APIs and a timeout-aware close barrier that
  continues cleanup after a caller stops waiting;
- lock-protected reclamation of stale owned Session directories;
- safe first-generation Piece/undo compaction and explicit save-checkpoint
  journal rebasing;
- a 4 GiB default recovery-journal hard limit, exact growth statistics, and
  explicitly enabled automatic save checkpoints with failure backoff;
- revision-bound UTF-8 logical Page virtualization with hard page/window
  budgets, bounded LRU caching, and concurrent-task backpressure;
- atomically published, format-neutral Fragment generations with explicit
  indexed watermarks, fixed-point Measure indexes, three anchor types,
  asymmetric overscan, and giant-Fragment continuation Pages.

It does not yet provide full-text search, multi-source composition,
collaboration, remote storage, UI, or a stable 1.0 API. Those capabilities and
their format-neutral boundaries are specified in [develop.md](develop.md).

## Relationship to TypeMD

The original implementation lived in TypeMD private backend packages. During
extraction, Markdown block scanning, format-specific metadata, SQLite search,
index publication, Wails bindings, and editor layout policy were removed.

Docengine and TypeMD do not synchronize automatically. TypeMD must explicitly
migrate to this module before it receives changes made here.

## Architecture

```text
Future host: desktop / CLI / service / format adapter
                         |
                         v
                   document.Session
       revision, transaction, history, save
       /          |          |            |             \
      v           v          v             v              v
 document/store recovery document/save document/coordinate document/virtual
  Piece Tree   v2 WAL    atomic replace  index/ChangeMap   Page/Fragment
       \          |          /             /              /
        +---------+---------+-------------+--------------+
                           |
                           v
                OS files and io.ReaderAt
```

`document/store` is the lowest layer. It represents logical content as Pieces
referencing external `io.ReaderAt` byte ranges. A persistent randomized treap
provides structural sharing, average logarithmic edits, bounded range reads,
and immutable roots. Its zero-value maintenance policy automatically coalesces
contiguous same-Source Pieces at a 4,096-Piece threshold; an unproductive pass
backs off by another threshold. `store.Options` can change or disable that
policy, and `Tree.Stats` exposes the next trigger and completed automatic
compactions without reading document bytes.

`recovery` stores each logical transaction as one checksummed v2 batch. Its
96-byte `DOCLOG02` header binds the journal to the normalized resolved path and
complete base SHA-256; `DOCJNL02` batches are exposed only after the complete
header, operation table, payload, and CRC-32C validate. Invalid tails are
repairable without exposing a partial transaction.
`Journal.Size`, `BatchEncodedSize`, and `BatchAppendResult.EndOffset` expose
exact physical growth without decoding or allocating a candidate batch.

`document/save` streams a Snapshot into a same-directory temporary file, syncs
it, performs a final full-content conflict check, and atomically replaces the
target. If replacement commits but POSIX directory sync fails, it returns a
typed `DurabilityError` so callers do not mistake a committed write for a
failed replacement.

For a save that overlaps newer edits, Session builds and syncs the replacement
base's journal, including its parent directory entry, before replacing the
base. The final identity check, journal preparation, and replacement share one
short mutation barrier. A process exit therefore leaves either the old base
with its old journal or the new base with its already-durable journal. Even a
save with no concurrent edit prepares an empty replacement journal for this
crash window. On reopen, exactly one candidate must match the current complete
base fingerprint; retired candidates are quarantined, while zero or multiple
matches still block the open.

`document.Session` owns the Piece Tree, revision history, recovery, source
generations, save rebasing, and lifecycle. `OpenContext` scans the complete
file once to validate UTF-8, compute SHA-256, and collect newline metadata.
Before and after that single pass it compares the OS change generation (`ctime`
or Windows `ChangeTime`), so a same-length rewrite with restored mtime is
rejected without reading the file twice.
Requested and resolved paths are both reported; saves remain pinned to the
resolved target. A failure after replacement puts the Session into a readable
but permanently non-mutating fault state instead of continuing unsafely.

`OpenOptions` resolves zero-valued limits to documented defaults: 256
operations per batch, 1 MiB per insertion, 256 MiB of undo storage, 256 retained
events, 256 retained ChangeMaps, 65,536 Anchors per batch, a 4 GiB recovery
journal hard limit, and a one-second journal sync interval. Automatic journal
checkpointing is disabled unless `AutoCheckpointJournalBytes` is set; enabling
it explicitly authorizes background saves once physical journal growth crosses
the threshold. Failed automatic checkpoints back off by another threshold.
`Session.RecoveryStats` reports current and next thresholds, physical bytes,
queued work, and completed automatic checkpoints. Host-facing Snapshot,
coordinate Index, and virtual Pager ownership shares a default limit of 1,024
leases across current and retired source generations. `Session.LifecycleStats`
reports active/peak leases, save waiters, active persistence, automatic work,
and closing/closed state. Explicit directories are shared by default; an omitted
Session directory is unique and owned. Undo files use collision-free temporary
names and are removed on close. Owned directories are removed only when they
are actual empty directories, while dirty recovery journals and unknown host
files are preserved. Owned Session directories carry a locked v1 marker;
automatic and explicit `ReclaimStaleSessionDirectories` cleanup removes only
unlocked, valid Docengine artifacts and never recursively deletes unknown
content. `Session.Config` reports the resolved policy.

`Session.Subscribe` atomically joins retained history to live events. Each
subscriber has a bounded queue and never blocks a transaction; when its queue
overflows, the newest event replaces stale pending events and reports the exact
omitted count in `Dropped`. `AfterSequence` resumes a consumer when history is
still available and also reports any history gap. Open, recovery, committed
Apply/Undo/Redo changes, save start/progress/completion/failure, recovery-WAL
Sync failure/restoration, and close are published. Progress events correlate
through `PersistenceProgress.OperationID`; post-commit failure is distinguishable
from pre-commit failure. Concurrent `Close` callers wait for the same
resource-retirement barrier and receive the same result.

`document/coordinate` builds an immutable index for one Snapshot revision.
Checkpoints are placed only at UTF-8 boundaries, so byte/line/rune queries read
at most one bounded checkpoint window. Each Index owns a byte-bounded LRU of
those immutable windows: the default is 1 MiB, the hard maximum is 256 MiB,
and caching can be disabled. Stats report resident bytes, entries, hits, and
misses. `ChangeMap` transforms Anchors and ranges across the sequential
replacements in one committed edit, including explicit before/after insertion
affinity. A Session coordinate index owns its Snapshot lease until `Close`.

`coordinate.Rebuild` and `RebuildOwned` accept the exact ChangeMap chain from a
previous Index to a new immutable Source. They validate both revisions and
lengths and inherit checkpoint/cache policy. They retain the prefix ending
before every edit, derive the untouched old/new suffix boundary from the
sequential map, scan only to the first useful old suffix checkpoint, then
translate the remaining byte/rune/line/column states from the newly scanned
seam. EOF alone is not reported as useful suffix reuse. This proof relies on
the API contract that the new Source is exactly the result described by the
ChangeMap; a mismatched Source is invalid input, not a reason to guess.
`Session.RebuildCoordinateIndex` supplies the current Snapshot lease. Stats
separate prefix/suffix checkpoint reuse and actual decoded bytes.

Session-created indexes carry an opaque lineage that cannot be replaced through
caller Options. `Session.RefreshCoordinateIndex` verifies that lineage, obtains
the retained map chain atomically with the current Snapshot, and rejects expired
history rather than silently rebuilding from an unrelated prefix.
`ChangesBetween` supports forward and reverse observable revision boundaries;
atomic-batch interior revisions are rejected. `TransformAnchors` and
`TransformRanges` apply that map to bounded batches without returning partial
output; their Context variants allow cancellation during work. One map or
composed history is capped at 1,048,576 edits, and one batch transform at
16,777,216 edit-by-anchor steps. `ComposeAll` validates a chain and copies its
edits once, avoiding quadratic retained-history composition.
`coordinate.Annotation[T]` carries an opaque host value whose meaning is never
interpreted by the core.

`document/virtual` builds a deterministic logical Page table for one immutable
UTF-8 Source revision. Page boundaries prefer LF after a target size and are
forced at a UTF-8 boundary before a hard maximum. Fragment publications use a
generation compare-and-swap, and `IndexedThrough` distinguishes analyzed gaps
from an unindexed suffix. Windows can be addressed by byte, Fragment ID, or
host-defined non-negative fixed-point `Measure`; every result is bounded by
bytes, pages, distinct Fragments, and Measure. Giant Fragments become
continuation Pages without guessing how their Measure is distributed.
`Session.VirtualPager` owns its Snapshot lease until `Close`.

`Session.Compact` coalesces only contiguous same-source Pieces and rewrites the
undo store with live references. `CompactOptions.CheckpointJournal` explicitly
persists a selected revision before rebasing the append-only journal; an
uncommitted WAL is never rewritten in place because doing so would break crash
atomicity or revision identity. Existing immutable Snapshots remain readable.

See [MODULES.md](MODULES.md) for implementation invariants and file-format
details.

## v0.3.0 breaking changes

- Recovery v1, single-replacement frames, root frames, and their exported APIs
  were removed.
- v2 uses `.docengine-journal-v2`, `DOCLOG02`, and `DOCJNL02`; old journals are
  outside the v2 namespace and are neither read nor migrated.
- `recovery.Fingerprint` now contains base length, resolved-path SHA-256, and
  complete-content SHA-256.
- `ReplayResult` returns atomic batches rather than legacy logical frames.
- `document.OpenContext`, `Metadata.ResolvedPath`, durability/fault metadata,
  `Session.Fault`, `document.ErrFaulted`, and `save.DurabilityError` were added.
- `SaveContext`, `CommitAtLeastContext`, `UndoContext`, `RedoContext`, and
  `CloseContext` define cancellation boundaries; `CloseContext` may stop
  waiting while the one shared cleanup barrier continues.

No compatibility promise applies before 1.0.

## Testing

The repository requires 100% statement coverage for every current package and
contains twenty-four Go fuzz targets:

- Piece Tree reference-model, concurrent snapshot/edit, compaction/Snapshot
  preservation, and automatic-compaction policy fuzzers;
- v2 header, operation decoder, replay-resilience, and stateful journal fuzzers;
- Session state-machine, concurrent save/edit, crash-recovery, journal-quota
  atomicity, cross-generation lifecycle-budget, and UTF-8 edit boundary fuzzers;
- resumable event-history, subscriber-overflow, and close state-machine fuzzing;
- bounded ChangeMap-history retention, expiry, reverse-query, and composition
  state-machine fuzzing;
- UTF-8 coordinate-reference, ChangeMap composition, and incremental-versus-
  full-index equivalence fuzzers;
- logical Page partition, UTF-8 reconstruction, Fragment-window reference, and
  Pager generation state-machine fuzzers.

Tests cover malformed and byte-truncated batches, state publication rollback,
same-size/same-mtime external modification, full-file and boundary-split UTF-8,
symlink retargeting, concurrent edit/save/recovery, platform durability faults,
the post-commit read-only state, configured resource limits, concurrent shared
runtime directories, marker-lock orphan reclamation, conservative cleanup,
live undo remapping, Snapshot-safe Piece compaction, exact journal quota
rejection, automatic-checkpoint backoff, and real child-process exits before
and after replacement with and without concurrent edits. Session lifecycle
tests additionally cover exact lease saturation, 64-way acquisition races,
queued-save wakeup, pre-commit cancellation at every checkpoint, timed close
continuation, and recovery after canceling an automatic checkpoint. Event tests additionally
cover exact loss accounting, replay cursors, save progress and failure phase,
journal Sync failure/restoration, a final close event under queue overflow,
concurrent publish/unsubscribe, and multiple callers waiting on one close barrier.

The v0.3.0 release suite was run on native Windows and Debian under WSL 2 using
a native Linux temporary directory. On both platforms every package reached
100% statement coverage, `-race -shuffle=on -count=3` passed, and three core
fuzz targets ran for at least 30 seconds without a failing implementation input.

The completed v0.4 release suite was run on native Windows and in a WSL
native-Linux directory: all five packages remained at 100% statement coverage,
three shuffled race runs passed, and all nine affected Session, event,
change-history, and coordinate fuzz targets passed 10-second runs on both
platforms.

The initial v0.5.0 implementation was verified on native Windows, and all six
Linux test binaries cross-compiled successfully. The v0.5.1 correctness suite
was then run in full on native Windows and Debian under WSL 2 from a native
Linux `/tmp` directory. On both platforms all six packages reached 100%
statement coverage, the complete repository passed three shuffled race runs,
and four virtualization fuzz targets plus the Session/Pager lifecycle fuzz
target each passed a 10-second run.

The v0.5.2 Piece Tree maintenance suite was run on native Windows and Debian
under WSL 2 from a native Linux `/tmp` directory. All six packages retained
100% statement coverage and three shuffled race runs passed. The four Piece
Tree fuzz targets each ran for 30 seconds on both platforms; automatic
compaction boundary tests also passed 100 consecutive runs, and the four
committed store benchmarks executed on both systems.

The v0.5.3 Recovery/Save suite was run on native Windows and Debian under
WSL 2 from a native Linux `/tmp` directory. All six packages retained 100%
statement coverage and three shuffled race runs passed. Four recovery fuzzers,
concurrent-save, crash-recovery, and journal-quota fuzzers each ran for 30
seconds on both systems. The real child-process crash matrix passed 20
consecutive runs per platform, checkpoint/quota boundary tests passed 100
consecutive runs, and the Recovery/Save/Session benchmarks executed on both.

The v0.5.4 Session lifecycle suite passed on native Windows and Debian under
WSL 2 from native Linux `/tmp`: all six packages remained at 100% statement
coverage and three shuffled race runs passed. Lifecycle-budget, Session-state,
concurrent-save, crash-recovery, and Session/Pager fuzzers each ran for 30
seconds per platform. Core lease/close/cancellation races passed 100 repeated
runs and the detailed pre-commit matrix passed 30. Snapshot lease acquisition
measured about 447–467 ns on Windows and 347–353 ns on Linux (368 B and four
allocations); the 4 MiB Session save benchmark measured about 49–50 ms and
10–11 ms respectively.

The v0.5.5 Coordinate/ChangeMap suite passed the same native Windows and WSL
Linux matrix: all six packages remained at 100% statement coverage, the full
repository passed three shuffled race runs, and all three coordinate fuzzers
ran for 30 seconds per platform. Cache/suffix/cancellation/maximum-history
boundaries passed 100 normal and ten race-enabled repetitions. A cached
64 KiB-window query retained zero allocations versus one roughly 72 KiB
allocation when disabled. A 4 MiB middle edit rebuilt in about 0.39–0.47 ms on
Windows and 0.323–0.339 ms on Linux, versus 27–29 ms and 20.6–21.9 ms for a
full build. A 256-map `ComposeAll` used one allocation versus 256 for pairwise
composition.

Run the normal checks:

```bash
go mod verify
gofmt -l .
go vet ./...
go test ./...
go test -race -shuffle=on -count=3 ./...
```

Run the fuzz targets:

```bash
go test ./document/store -run=^$ -fuzz=FuzzTreeMatchesReference -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeConcurrentReadDuringEdits -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeCompactionPreservesSnapshots -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeAutoCompactionMatchesReference -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalDecoders -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalStateMachine -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalBatchOperationsDecode -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalReplayResilience -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionConcurrentSaveEdit -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionCrashRecovery -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionJournalQuotaIsAtomic -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionLifecycleBudgets -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzUTF8ReplacementBoundaries -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzEventHubStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzChangeHistoryStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzVirtualPagerSessionLifecycle -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIndexMatchesUTF8Reference -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzChangeMapBoundsAndComposition -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIncrementalIndexMatchesFullBuild -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzLogicalPagePartition -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzLogicalPagesPreserveUTF8 -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzFragmentWindowsRespectRanges -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzPagerGenerationStateMachine -fuzztime=30s
```

Windows race builds require a GCC-compatible MinGW-w64 toolchain; MSVC-target
`cl.exe` or `clang-cl.exe` is not sufficient for Go's Windows race build.

## Current limitations

- Stale Session reclamation deliberately recognizes only valid Docengine
  marker/undo entries. Unknown files, malformed markers, symlinks, and live
  locks are preserved for safety.
- A post-replacement rebind failure deliberately stops mutation; an explicit
  reopen is required.
- External-change checking still has the unavoidable final hash-to-replace
  race unless a host provides stronger file locking.
- Session-managed ChangeMap history is bounded by retained transactions; an
  expired revision requires a full rebuild. Suffix reuse requires the new
  Source to be the exact ChangeMap result. Coordinate cache budgets cover
  resident windows, not a concurrent miss's transient read buffer.
- File-watcher candidates and future indexing/virtualization progress events
  are not implemented; save and recovery-WAL persistence transitions are.
- Journal compaction requires an explicit save checkpoint. Search indexing,
  search-index compaction, and composition are not implemented.
- Fragment metadata and logical Page tables are bounded by configured page,
  Fragment, key, task, and cache limits. Cache limits exclude transient copies
  held by active tasks; simultaneous in-core Window payloads are bounded by
  `MaximumTasks × Window.Bytes`. Hosts control how long returned copies remain
  live and must include those retained results in their own memory budgets.
- The API and on-disk formats remain unstable until 1.0.

## Next work

The v0.5.x maintenance line next closes event/compaction and Virtual
refresh/progress/performance gaps. After every existing module is closed, the
known Windows journal-durability CI race is fixed and released separately.
Only then does v0.6 start format-neutral search. The target architecture and
remaining milestones are in [develop.md](develop.md).
