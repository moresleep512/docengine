# Docengine module notes

This repository now contains only the format-neutral local document core. The
Markdown structure pipeline, SQLite search pipeline, index publication
orchestration, and editor-layout virtualization code were removed because their
public contracts encoded TypeMD-specific document and UI policy.

The source tree is intentionally not a complete standalone Go module yet. Its
internal imports assume a future module path named `docengine`, but no `go.mod`
is present.

## Dependency direction

```text
document/store ----+
document/save -----+--> document
recovery ----------+
```

The lower-level packages do not import `document`. The `document` package is the
coordination layer that owns their lifecycle.

## `document/store`

### Responsibility

`document/store` represents a logical document as immutable pieces that point
into external `io.ReaderAt` sources. The initial document points into the base
file; inserted text points into the recovery journal. Editing changes metadata
nodes instead of copying the full document.

### Data model

- `SourceID` identifies a byte source. The built-in IDs are the base file and
  the journal.
- `Piece` stores source ID, source offset, logical length, and optional newline
  metadata.
- `Tree` owns the current root and the live source map.
- `Snapshot` captures an immutable root plus a copy of the source map.

### Algorithm

The tree is a persistent randomized treap. Each node caches total bytes, piece
count, and newline information for its subtree. `ReplacePiece` splits the tree
at the replacement boundaries, discards the deleted middle, and merges an
optional replacement piece between the surviving sides.

Nodes on changed paths are cloned, so earlier roots remain readable. Average
edit and coordinate traversal cost is proportional to tree height rather than
document byte length.

### Concurrency and lifetime

`Tree` protects its mutable root and source map with an RW mutex. A `Snapshot`
does not own file handles; the higher-level generation lease must keep every
referenced source open while the snapshot is in use.

### Important limitations

- Splitting a piece invalidates that piece's cached newline count; this package
  does not rescan it.
- The package validates logical ranges and source presence, but trusts source
  offsets and lengths supplied by callers.
- Source IDs are a small fixed integer namespace rather than a general source
  registry contract.

## `recovery`

### Responsibility

`recovery` stores unsaved replacements in an append-only journal and reconstructs
the edit sequence after a crash.

### File format

- A 72-byte file header contains format magic, version, base size, base
  modification time, normalized-path hash, and a CRC.
- Each edit has a 64-byte frame header followed by inserted bytes.
- Frame CRC uses CRC-32C over the header prefix and payload.
- Replace frames record revision, edit group, start, deleted length, and
  inserted length.
- Root frames remain for legacy replay semantics, although normal editing now
  records grouped replacements.

The magic values are now `DOCLOG01` and `DOCJNL01`; journals written by TypeMD's
old `TMD...` format are deliberately incompatible.

### Replay and repair

`Replay` walks frames sequentially. An incomplete header, invalid frame,
oversized payload, incomplete payload, or CRC mismatch marks the remaining tail
as truncated. The valid prefix is returned to the caller, which may call
`RepairTail` to truncate the file to the last verified frame.

### Durability and concurrency

Journal methods are serialized with a mutex. Appending does not sync every
frame; `document.Session` periodically calls `Sync`, trading at most a small
window of recent edits for lower foreground latency.

### Important limitations

- The base identity is size, modification time, and a lower-cased absolute-path
  hash; it is not a content hash.
- Same-size external edits with a preserved timestamp can evade stale-journal
  detection.
- Lower-casing paths assumes case-insensitive path identity and needs review for
  case-sensitive filesystems.

## `document/save`

### Responsibility

`document/save` streams an immutable snapshot into a same-directory temporary
file and replaces the original without holding the complete document in memory.

### Save sequence

1. Create `.docengine-save-*.tmp` beside the target.
2. Apply the original permission bits.
3. Write an optional prefix, such as a UTF-8 BOM.
4. Stream snapshot content.
5. Sync and close the temporary file.
6. Run an optional last-moment external-change check.
7. Replace the original path.

Uncommitted temporary files are removed by a deferred cleanup.

### Platform behavior

- Non-Windows builds use `os.Rename`.
- Windows uses `ReplaceFileW` with write-through replacement semantics. The
  base file is opened with `FILE_SHARE_DELETE`, allowing old snapshot readers
  to keep their handle while the path is replaced.

### Important limitations

- The POSIX path syncs file content but does not sync the containing directory
  after rename.
- There are no package-local tests for the save package; it is exercised
  indirectly through session tests.

## `document`

### Responsibility

`document` is the public coordination layer for opening, reading, editing,
recovering, snapshotting, undoing, redoing, and saving a local UTF-8 text file.

### Opening a session

`Open` resolves an absolute regular-file path, opens the base file, samples the
first 64 KiB for UTF-8 validity, detects an optional BOM and newline style, and
constructs the initial piece tree. It then finds the newest matching recovery
journal, replays valid frames, repairs a truncated tail, opens the disk-backed
undo store, and starts a once-per-second journal sync loop.

Default transient storage is under the system temporary directory in
`docengine/recovery` and `docengine/sessions`.

### Revisions and editing

- Every replacement increments the session revision.
- `ApplyBatch` rejects a caller whose expected revision is stale.
- A batch is limited to 256 operations.
- Each inserted string must be valid UTF-8 and at most 1 MiB.
- Deleted and inserted bytes are recorded in a disk-backed undo store.
- Every accepted replacement is appended to the recovery journal before the
  piece tree starts referencing its payload.

The current implementation mutates one operation at a time. If a later
operation fails or its context is cancelled, earlier operations in the same
batch remain applied even though `ApplyBatch` returns an error. There is no
rollback snapshot, and the incomplete batch is not appended as a normal undo
entry. This is the highest-priority semantic defect in the retained core.

### Undo storage

`undo_store.go` writes history text into `undo.store` inside the session
directory instead of retaining large deleted ranges in memory. Its default
quota is 256 MiB. Exceeding the quota clears both history stacks, truncates the
store, and begins a new history epoch.

The session closes the undo file but does not remove its directory; the future
host layer must own transient-directory cleanup.

### Snapshot generations

`generation.go` couples an immutable tree snapshot to the base and journal file
handles it references. Each snapshot lease increments the generation reference
count. Saving may install a new base file and retire the old generation, but old
handles are closed and the old journal is deleted only after every lease has
been released.

This makes long-running readers independent of subsequent edits and saves, but
callers must always close leases.

### Saving with concurrent edits

`CommitAtLeast` serializes saves separately from editing:

1. Capture a target revision and lease its immutable snapshot.
2. Release the session lock while streaming the snapshot.
3. Check size and modification-time identity before and immediately before
   replacement.
4. Reopen the newly saved base as a new generation.
5. If edits arrived during streaming, copy only the newer journal operations
   into a new journal rooted at the saved file and rebuild their piece-tree
   overlay.
6. Retire the old generation after outstanding readers finish.

The result may therefore be clean or may remain dirty at a revision newer than
the committed snapshot.

### Metadata and policy still present

The package is text-document-specific rather than binary-document-generic. It
still defines UTF-8 insertion policy, BOM preservation, newline-style metadata,
revision and batch limits, undo quota, default temporary directories,
and file identity policy. These are generic local text-engine policies, not
TypeMD/Markdown business behavior, but they may eventually become injected
configuration.

### Important limitations

- Only the first 64 KiB of an opened file is validated as UTF-8.
- External-change detection relies on size and modification time, not content.
- `historyText` handles quota exhaustion but does not propagate other undo-store
  write errors cleanly.
- `Close` preserves a dirty recovery journal but leaves session-directory
  cleanup to its host.

## Removed modules

### `document/blocks`

Removed because it combined Markdown syntax classification, semantic block
identity, fragmentation, layout-height estimation, checkpointing, and the
binary block-index format in one contract. The height fields also tied the
storage representation to a particular editor layout model.

### `search`

Removed because the SQLite FTS index accepted `blocks.Meta`, persisted Markdown
block kind, and used product-specific fragment limits. A future search package
should accept a format-neutral fragment interface and define explicit
completeness and candidate-selection semantics.

### `indexing`

Removed because it hard-wired the Markdown scanner, block-index writer, and FTS
writer into one build pipeline. A future orchestration layer should receive
scanner and index-writer policies from its host.

### `virtualization`

Removed because its public API directly exposed block metadata, estimated pixel
height, overscan policy, and render response limits. Those decisions belong in
an editor or presentation adapter, not the document persistence core.

## Next structural decisions

1. Choose and declare the real Go module path.
2. Decide whether `document.Session` is the final public facade or whether a
   smaller engine facade should hide recovery paths and generation management.
3. Make batch application atomic before treating it as a transaction API.
4. Move quotas, temporary paths, sync intervals, and identity policy into
   configuration.
5. Add host-neutral cleanup ownership and explicit journal compatibility policy.
6. Reintroduce structure, search, and presentation only through format-neutral
   interfaces after their boundaries are designed.
