# Docengine

[简体中文](README_zh.md) · [Development architecture and roadmap (Chinese)](develop.md)

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
- revision-bound byte/line/rune coordinate indexes with bounded reads and
  conservative ChangeMap-driven checkpoint-prefix reuse;
- sequential ChangeMaps and affinity-aware Anchors returned by edits,
  undo, and redo;
- bounded Session-managed ChangeMap history, forward/reverse revision queries,
  lineage-checked index refresh, and atomic batch Anchor transforms;
- bounded, resumable Session event streams with precise slow-consumer loss
  reporting and a concurrent close barrier;
- resolved Session resource limits, journal sync cadence, and explicit shared
  or owned runtime-directory policies.

It does not yet provide full-text search, Page/Fragment virtualization,
multi-source composition, collaboration, remote storage, UI, or a stable 1.0
API. Those capabilities and their format-neutral boundaries are specified in
[develop.md](develop.md).

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
       /          |          |            \
      v           v          v             v
 document/store recovery document/save document/coordinate
  Piece Tree   v2 WAL    atomic replace  index / ChangeMap
       \          |          /             /
        +---------+---------+-------------+
                           |
                           v
                OS files and io.ReaderAt
```

`document/store` is the lowest layer. It represents logical content as Pieces
referencing external `io.ReaderAt` byte ranges. A persistent randomized treap
provides structural sharing, average logarithmic edits, bounded range reads,
and immutable roots.

`recovery` stores each logical transaction as one checksummed v2 batch. Its
96-byte `DOCLOG02` header binds the journal to the normalized resolved path and
complete base SHA-256; `DOCJNL02` batches are exposed only after the complete
header, operation table, payload, and CRC-32C validate. Invalid tails are
repairable without exposing a partial transaction.

`document/save` streams a Snapshot into a same-directory temporary file, syncs
it, performs a final full-content conflict check, and atomically replaces the
target. If replacement commits but POSIX directory sync fails, it returns a
typed `DurabilityError` so callers do not mistake a committed write for a
failed replacement.

`document.Session` owns the Piece Tree, revision history, recovery, source
generations, save rebasing, and lifecycle. `OpenContext` scans the complete
file once to validate UTF-8, compute SHA-256, and collect newline metadata.
Requested and resolved paths are both reported; saves remain pinned to the
resolved target. A failure after replacement puts the Session into a readable
but permanently non-mutating fault state instead of continuing unsafely.

`OpenOptions` resolves zero-valued limits to documented defaults: 256
operations per batch, 1 MiB per insertion, 256 MiB of undo storage, 256 retained
events, 256 retained ChangeMaps, 65,536 Anchors per batch, and a one-second
journal sync interval. Explicit directories are shared by default; an omitted
Session directory is unique and owned. Undo files use collision-free temporary
names and are removed on close. Owned directories are removed only when they
are actual empty directories, while dirty recovery journals and unknown host
files are preserved. `Session.Config` reports the resolved policy.

`Session.Subscribe` atomically joins retained history to live events. Each
subscriber has a bounded queue and never blocks a transaction; when its queue
overflows, the newest event replaces stale pending events and reports the exact
omitted count in `Dropped`. `AfterSequence` resumes a consumer when history is
still available and also reports any history gap. Open, recovery, committed
Apply/Undo/Redo changes, and close are currently published. Concurrent `Close`
callers wait for the same resource-retirement barrier and receive the same
result.

`document/coordinate` builds an immutable index for one Snapshot revision.
Checkpoints are placed only at UTF-8 boundaries, so byte/line/rune queries read
at most one bounded checkpoint window. `ChangeMap` transforms Anchors and
ranges across the sequential replacements in one committed edit, including
explicit before/after insertion affinity. A Session coordinate index owns its
Snapshot lease until `Close`.

`coordinate.Rebuild` and `RebuildOwned` accept the exact ChangeMap chain from a
previous Index to a new immutable Source. They validate both revisions and
lengths, inherit the checkpoint interval, reuse only the prefix ending at or
before every sequential edit, and rescan the remaining new content. This avoids
unsafe suffix shifting when line/column state cannot be proved unchanged.
`Session.RebuildCoordinateIndex` supplies the current Snapshot lease; Stats
reports reused checkpoints and scanned bytes.

Session-created indexes carry an opaque lineage that cannot be replaced through
caller Options. `Session.RefreshCoordinateIndex` verifies that lineage, obtains
the retained map chain atomically with the current Snapshot, and rejects expired
history rather than silently rebuilding from an unrelated prefix.
`ChangesBetween` supports forward and reverse observable revision boundaries;
atomic-batch interior revisions are rejected. `TransformAnchors` applies that
map to a bounded batch without returning partial output.

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

No compatibility promise applies before 1.0.

## Testing

The repository requires 100% statement coverage for every current package and
contains fifteen Go fuzz targets:

- Piece Tree reference-model and concurrent snapshot/edit fuzzers;
- v2 header, operation decoder, replay-resilience, and stateful journal fuzzers;
- Session state-machine, concurrent save/edit, crash-recovery, and UTF-8 edit
  boundary fuzzers;
- resumable event-history, subscriber-overflow, and close state-machine fuzzing;
- bounded ChangeMap-history retention, expiry, reverse-query, and composition
  state-machine fuzzing;
- UTF-8 coordinate-reference, ChangeMap composition, and incremental-versus-
  full-index equivalence fuzzers.

Tests cover malformed and byte-truncated batches, state publication rollback,
same-size/same-mtime external modification, full-file and boundary-split UTF-8,
symlink retargeting, concurrent edit/save/recovery, platform durability faults,
the post-commit read-only state, configured resource limits, concurrent shared
runtime directories, and owned-directory cleanup.
Event tests additionally cover exact loss accounting, replay cursors, a final
close event under queue overflow, concurrent publish/unsubscribe, and multiple
callers waiting on one close barrier.

The v0.3.0 release suite was run on native Windows and Debian under WSL 2 using
a native Linux temporary directory. On both platforms every package reached
100% statement coverage, `-race -shuffle=on -count=3` passed, and three core
fuzz targets ran for at least 30 seconds without a failing implementation input.

The current v0.4 coordinate, lifecycle, and event foundations were verified on
native Windows and in a WSL native-Linux directory: all five packages remained
at 100% statement coverage, three shuffled race runs passed, and all nine
affected Session, event, and coordinate fuzz targets passed 10-second runs on
both platforms.

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
go test ./recovery -run=^$ -fuzz=FuzzJournalDecoders -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalStateMachine -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalBatchOperationsDecode -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalReplayResilience -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionConcurrentSaveEdit -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionCrashRecovery -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzUTF8ReplacementBoundaries -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzEventHubStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzChangeHistoryStateMachine -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIndexMatchesUTF8Reference -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzChangeMapBoundsAndComposition -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIncrementalIndexMatchesFullBuild -fuzztime=30s
```

Windows race builds require a GCC-compatible MinGW-w64 toolchain; MSVC-target
`cl.exe` or `clang-cl.exe` is not sufficient for Go's Windows race build.

## Current limitations

- Process crashes can leave orphaned ephemeral Session directories; they are
  collision-safe but automatic stale-process reclamation is not implemented.
- A post-replacement rebind failure deliberately stops mutation; an explicit
  reopen is required.
- External-change checking still has the unavoidable final hash-to-replace
  race unless a host provides stronger file locking.
- Session-managed ChangeMap history is bounded by retained transactions; an
  expired revision requires a full rebuild. Incremental indexes conservatively
  rescan from the earliest affected checkpoint; automatic cache ownership and
  proven suffix reuse are not implemented.
- Save, fault, external change, and background-progress event kinds are not yet
  published.
- Piece/journal/undo compaction, search indexing, virtualization, and
  composition are not implemented.
- The API and on-disk formats remain unstable until 1.0.

## Next work

The next v0.4 work is stale-session reclamation, the remaining
persistence/progress event kinds, generic retained interval/annotation
transforms, and the first compaction policy.
Format-neutral logical Page/Fragment virtualization then builds on these
foundations, followed by persistent search and multi-source composition. The
decision-complete target architecture, readiness assessment, edge cases, and
v0.4–v1.0 milestones are in [develop.md](develop.md).
