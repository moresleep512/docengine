# Docengine

[简体中文](README_zh.md)

Docengine is an experimental Go core for editing large local UTF-8 text files
without loading the complete document into memory.

It began as an extraction of the backend document engine from
[TypeMD](https://github.com/moresleep512/TypeMD). The extracted code is now an
independent Go module and Git repository, with TypeMD- and Markdown-specific
indexing, search, and presentation policy removed from the core.

```go
module github.com/moresleep512/docengine
```

## Project position

Docengine is a **local text-document engine**, not an application server or a
complete editor.

Its responsibility is to provide the storage and persistence foundation for a
host such as a desktop application, CLI, language tool, or future service:

- disk-backed editing through a persistent Piece Tree;
- immutable snapshots that remain readable while editing and saving continue;
- revision-checked replacements and disk-backed undo/redo history;
- append-only crash recovery;
- streaming, conflict-checked atomic saves;
- bounded foreground memory use as document size grows.

The repository currently does **not** provide:

- an HTTP, RPC, or WebSocket server;
- a desktop or web UI;
- Markdown parsing or rendering;
- full-text search or indexing;
- collaboration, OT, or CRDT;
- remote or database-backed storage;
- a stable versioned public API.

The current status is **early-stage/experimental**. The low-level Piece Tree has
been heavily hardened, but transaction, recovery, and save semantics still need
additional work before the module should be treated as production-ready.

## Relationship to TypeMD

The original implementation lived inside TypeMD's private backend packages. The
initial Docengine snapshot was copied out so the document core could evolve
without remaining coupled to Wails, the TypeMD frontend, Markdown block models,
or editor layout policy.

During the first cleanup:

- Markdown block scanning and the block metadata index were removed;
- SQLite FTS search was removed;
- the hard-wired indexing publication pipeline was removed;
- editor virtualization and estimated layout-height contracts were removed;
- TypeMD-specific paths, suffixes, persistence magic, JSON bridge tags, and
  import paths were replaced or removed.

Docengine and TypeMD are not automatically synchronized. Changes in this
repository do not affect TypeMD until TypeMD explicitly migrates to this module.
The current `DOCLOG01`/`DOCJNL01` recovery format is deliberately incompatible
with the former TypeMD journal magic.

## Architecture

```text
Future host: CLI / desktop / HTTP / RPC
                    |
                    v
              document.Session
        revision, edit, undo/redo, save
          /          |             \
         v           v              v
 document/store   recovery      document/save
  Piece Tree       journal      atomic replace
         \           |              /
          +----------+-------------+
                     |
                     v
          OS files and io.ReaderAt sources
```

### `document/store`

The lowest data-structure layer. It represents the logical document as Pieces
that refer to byte ranges in external `io.ReaderAt` sources. A persistent
randomized Treap provides structural sharing, logarithmic average edit
operations, immutable roots, and bounded range reads.

### `recovery`

An append-only recovery journal with file fingerprints, revisions, grouped
replacement frames, payload CRC-32C validation, replay, and corrupt-tail repair.

### `document/save`

Streams an immutable snapshot to a same-directory temporary file, syncs it,
checks the original file for external changes, and atomically replaces the
target. Windows uses `ReplaceFileW`; other platforms use `os.Rename`.

### `document`

The current public coordination layer. `Session` owns revisions, the Piece Tree,
recovery, disk-backed undo/redo, snapshot generations, concurrent save handling,
UTF-8/BOM/EOL policy, and resource retirement.

See [MODULES.md](MODULES.md) for implementation-level design notes, invariants,
file formats, limitations, and removed module rationale.

## What has been completed

### Repository foundation

- Independent Git repository and Go 1.26 module.
- Public module path: `github.com/moresleep512/docengine`.
- Linux and Windows CI.
- Formatting, vet, unit-test, race, and fuzz-smoke jobs.
- TypeMD product dependencies removed from Go source.

### Piece Tree hardening

- Checked constructors now reject invalid base Pieces.
- Replacement validation covers negative ranges, invalid offsets, missing
  sources, newline metadata, source-range overflow, and total-length overflow.
- No-op replacements preserve the existing root instead of fragmenting a Piece.
- `Restore` now restores both the immutable root and captured source bindings.
- Internal Piece splits preserve Treap priority and therefore heap order.
- Tests inspect cached byte, Piece, and newline summaries on every subtree.
- Snapshot isolation is tested across edits, source replacement, source removal,
  and restore.

### Local toolchain validation

The current development environment has been verified with MinGW-w64 GCC using
`CGO_ENABLED=1`, allowing the Windows Go race detector to run locally.

## Testing status

At the current milestone the repository contains:

- 26 conventional tests;
- 1 Go fuzz target;
- 100% statement coverage for `document/store`;
- a randomized byte-slice reference-model test;
- 10,000 sequential-insert balance coverage;
- concurrent snapshot readers during edits;
- invalid range, integer overflow, short source, and error-propagation tests.

Verified locally:

```text
go mod verify                                  PASS
go vet ./...                                   PASS
go test ./...                                  PASS
go test -race ./...                            PASS
go test -race -shuffle=on -count=3 ./...       PASS
```

A 30-second local fuzz run completed 407,827 executions without finding a
failure. CI also runs a short fuzz smoke test on every change.

Run the main checks:

```bash
go test ./...
go vet ./...
go test -race ./...
```

Run the Piece Tree fuzz target:

```bash
go test ./document/store \
  -run=^$ \
  -fuzz=FuzzTreeMatchesReference \
  -fuzztime=30s
```

Windows race builds require a GCC-compatible MinGW-w64 toolchain rather than
MSVC-target `cl.exe` or `clang-cl.exe`.

## Current limitations

- `ApplyBatch` is not yet truly atomic: an error in a later operation can leave
  earlier operations applied.
- The journal does not yet have a committed batch frame, so recovery cannot
  provide all-or-nothing replay for multi-operation transactions.
- Only the first 64 KiB of an opened file is checked for valid UTF-8.
- File identity is based on path, size, and modification time rather than a
  strong content fingerprint.
- POSIX atomic replacement does not yet sync the containing directory.
- Session-directory cleanup and most limits are still host-owned or hard-coded.
- `document/save` is exercised through session tests but lacks focused
  package-local fault-injection tests.
- No release, semantic-versioning promise, or compatibility guarantee exists
  yet.

## Roadmap / TODO

### P0: transactional correctness

- Make `ApplyBatch` all-or-nothing in memory.
- Add an atomic journal batch format, such as a single batch frame or explicit
  begin/commit records.
- Ignore incomplete batches during recovery.
- Add cancellation and partial-write fault-injection tests.

### P1: recovery and persistence

- Fuzz journal headers, frames, payload lengths, CRC failures, and replay.
- Strengthen base-file identity and define a compatibility/migration policy.
- Add focused atomic-save tests for write, sync, permission, conflict, and
  replace failures.
- Review POSIX directory durability and Windows replacement edge cases.

### P1: session policy and lifecycle

- Validate the complete opened document as UTF-8, preferably by streaming.
- Make undo quota, insertion limits, sync interval, and temporary paths
  configurable.
- Define explicit ownership and cleanup of session directories.
- Improve propagation of undo-store write failures.

### P2: public API

- Decide whether `document.Session` is the final public facade.
- Add package documentation and runnable examples.
- Stabilize error types and cancellation behavior.
- Establish releases and semantic versioning after the API settles.

### P2: optional higher-level capabilities

- Reintroduce structure scanning through a format-neutral interface.
- Build search on generic fragments rather than Markdown block metadata.
- Keep rendering and viewport virtualization in host/presentation adapters.

## Development

Requirements:

- Go 1.26 or later;
- a GCC-compatible MinGW-w64 compiler for Windows race builds.

Clone and verify:

```bash
git clone https://github.com/moresleep512/docengine.git
cd docengine
go test ./...
```

The API is still evolving. Pin a commit when experimenting with the module, and
do not assume compatibility between early revisions.
