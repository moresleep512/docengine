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
- streaming, conflict-checked atomic saves;
- POSIX parent-directory synchronization and Windows `ReplaceFileW` with
  write-through replacement plus bounded transient-error retry;
- symlink-target pinning and explicit post-commit fault handling.

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
             /             |              \
            v              v               v
   document/store       recovery       document/save
    Piece Tree       v2 batch WAL     atomic replace
             \             |              /
              +------------+-------------+
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
contains nine Go fuzz targets:

- Piece Tree reference-model and concurrent snapshot/edit fuzzers;
- v2 header, operation decoder, replay-resilience, and stateful journal fuzzers;
- Session state-machine, concurrent save/edit, and crash-recovery fuzzers.

Tests cover malformed and byte-truncated batches, state publication rollback,
same-size/same-mtime external modification, full-file and boundary-split UTF-8,
symlink retargeting, concurrent edit/save/recovery, platform durability faults,
and the post-commit read-only state.

The v0.3.0 release suite was run on native Windows and Debian under WSL 2 using
a native Linux temporary directory. On both platforms every package reached
100% statement coverage, `-race -shuffle=on -count=3` passed, and each fuzz
target ran for at least 30 seconds without a failing implementation input.

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
```

Windows race builds require a GCC-compatible MinGW-w64 toolchain; MSVC-target
`cl.exe` or `clang-cl.exe` is not sufficient for Go's Windows race build.

## Current limitations

- Session limits, sync cadence, undo quota, and transient-directory ownership
  are still partly hard-coded.
- A post-replacement rebind failure deliberately stops mutation; an explicit
  reopen is required.
- External-change checking still has the unavoidable final hash-to-replace
  race unless a host provides stronger file locking.
- Piece/journal/undo compaction, stable coordinate maps, events, indexing,
  virtualization, and composition are not implemented.
- The API and on-disk formats remain unstable until 1.0.

## Next work

The next required foundation is configurable Session lifecycle plus
byte/line/rune coordinates and cross-revision ChangeMap. Format-neutral logical
Page/Fragment virtualization follows that foundation, then built-in persistent
search and multi-source composition. The decision-complete target architecture,
readiness assessment, edge cases, and v0.4–v1.0 milestones are in
[develop.md](develop.md).
