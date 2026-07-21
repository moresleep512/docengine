package store

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// appendSource is a growing io.ReaderAt backing for the journal source during
// property tests. Bytes are appended to data, and reader() returns a fresh
// bytes.Reader over the current slice. Rebinding the tree source after each
// append (the same pattern as TestTreeRandomReplacementsMatchReference) keeps
// newly written offsets readable while retired snapshots retain the older,
// shorter ReaderAt they captured at creation time.
type appendSource struct {
	data []byte
}

func (s *appendSource) add(data []byte) int64 {
	offset := int64(len(s.data))
	s.data = append(s.data, data...)
	return offset
}

func (s *appendSource) reader() io.ReaderAt { return bytes.NewReader(s.data) }

// TestPropertyNoOpReplaceReturnsSameRoot checks that a replacement which
// neither deletes nor inserts returns the existing root pointer for both the
// before and after snapshots. This is the structural-sharing fast path and a
// regression here would silently fragment the tree on every no-op.
func TestPropertyNoOpReplaceReturnsSameRoot(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for iteration := 0; iteration < 200; iteration++ {
		base := make([]byte, rng.IntN(64))
		for i := range base {
			base[i] = byte('a' + rng.IntN(26))
		}
		tree := mustNewTree(t, base)
		tree.SetSource(SourceJournal, bytes.NewReader(base))
		start := rng.Int64N(tree.Len() + 1)
		root := tree.root
		pieces := tree.PieceCount()
		before, after, err := tree.ReplacePiece(start, 0, Piece{})
		if err != nil {
			t.Fatalf("iteration %d: no-op replace = %v", iteration, err)
		}
		if tree.root != root || before.root != root || after.root != root {
			t.Fatalf("iteration %d: no-op changed the root", iteration)
		}
		if tree.PieceCount() != pieces {
			t.Fatalf("iteration %d: no-op fragmented the tree", iteration)
		}
		assertTreeInvariants(t, tree)
		assertSnapshotInvariants(t, before)
		assertSnapshotInvariants(t, after)
	}
}

// TestPropertyReplaceDeleteInsertRoundTrip verifies that for any replace, the
// inverse replace (delete the inserted bytes, reinsert the deleted bytes at
// the same start) restores the original content exactly and leaves the tree
// structurally valid.
func TestPropertyReplaceDeleteInsertRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 37))
	for iteration := 0; iteration < 300; iteration++ {
		base := make([]byte, rng.IntN(64))
		for i := range base {
			base[i] = byte('a' + rng.IntN(26))
		}
		reference := append([]byte(nil), base...)
		tree := mustNewTree(t, base)
		var journal appendSource
		tree.SetSource(SourceJournal, journal.reader())

		start := rng.Int64N(int64(len(reference) + 1))
		maxDelete := int64(len(reference)) - start
		deleteLength := int64(0)
		if maxDelete > 0 {
			deleteLength = rng.Int64N(min(maxDelete, 16) + 1)
		}
		insert := make([]byte, rng.IntN(16))
		for i := range insert {
			insert[i] = byte('A' + rng.IntN(26))
		}
		// Capture the bytes that the forward edit will remove, before applying it.
		deleted := append([]byte(nil), reference[int(start):int(start)+int(deleteLength)]...)
		offset := journal.add(insert)
		tree.SetSource(SourceJournal, journal.reader())
		piece := Piece{}
		if len(insert) > 0 {
			piece = Piece{Source: SourceJournal, Offset: offset, Length: int64(len(insert))}
		}
		if _, _, err := tree.ReplacePiece(start, deleteLength, piece); err != nil {
			t.Fatalf("iteration %d: forward replace = %v", iteration, err)
		}
		reference = splice(reference, int(start), int(deleteLength), insert)
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)

		// Inverse: delete the inserted bytes, reinsert the deleted bytes.
		invOffset := journal.add(deleted)
		tree.SetSource(SourceJournal, journal.reader())
		invPiece := Piece{}
		if len(deleted) > 0 {
			invPiece = Piece{Source: SourceJournal, Offset: invOffset, Length: int64(len(deleted))}
		}
		if _, _, err := tree.ReplacePiece(start, int64(len(insert)), invPiece); err != nil {
			t.Fatalf("iteration %d: inverse replace = %v", iteration, err)
		}
		reference = splice(reference, int(start), len(insert), deleted)
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)
	}
}

// TestPropertySnapshotImmutabilityAfterManyEdits keeps a ring of up to 16
// snapshots and, after a sequence of random edits, verifies that every retired
// snapshot still produces exactly the bytes it captured at creation time.
// This mirrors the FuzzTreeMatchesReference snapshot ring but as a
// deterministic property test with a different seed and a larger edit count.
func TestPropertySnapshotImmutabilityAfterManyEdits(t *testing.T) {
	const maxSnapshots = 16
	rng := rand.New(rand.NewPCG(101, 103))
	base := bytes.Repeat([]byte{'.'}, 8)
	reference := append([]byte(nil), base...)
	tree := mustNewTree(t, base)
	var journal appendSource
	tree.SetSource(SourceJournal, journal.reader())

	type snapshotEntry struct {
		snapshot Snapshot
		content  []byte
	}
	ring := make([]snapshotEntry, 0, maxSnapshots)
	capture := func() {
		snap := tree.Snapshot()
		content, err := io.ReadAll(io.NewSectionReader(snap, 0, snap.Len()))
		if err != nil {
			t.Fatal(err)
		}
		if len(ring) < maxSnapshots {
			ring = append(ring, snapshotEntry{snapshot: snap, content: content})
		} else {
			ring[rng.IntN(maxSnapshots)] = snapshotEntry{snapshot: snap, content: content}
		}
	}
	capture()

	for edit := 0; edit < 500; edit++ {
		start := rng.Int64N(int64(len(reference) + 1))
		maxDelete := int64(len(reference)) - start
		deleteLength := int64(0)
		if maxDelete > 0 {
			deleteLength = rng.Int64N(min(maxDelete, 12) + 1)
		}
		insert := make([]byte, rng.IntN(12))
		for i := range insert {
			insert[i] = byte('a' + rng.IntN(26))
		}
		offset := journal.add(insert)
		tree.SetSource(SourceJournal, journal.reader())
		piece := Piece{}
		if len(insert) > 0 {
			piece = Piece{Source: SourceJournal, Offset: offset, Length: int64(len(insert))}
		}
		if _, _, err := tree.ReplacePiece(start, deleteLength, piece); err != nil {
			t.Fatalf("edit %d: %v", edit, err)
		}
		reference = splice(reference, int(start), int(deleteLength), insert)
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)
		if edit%7 == 0 {
			capture()
		}
	}

	// Every retired snapshot must still read its captured bytes unchanged.
	for index, entry := range ring {
		got, err := io.ReadAll(io.NewSectionReader(entry.snapshot, 0, entry.snapshot.Len()))
		if err != nil {
			t.Fatalf("snapshot %d read = %v", index, err)
		}
		if !bytes.Equal(got, entry.content) {
			t.Fatalf("snapshot %d mutated\n got: %q\nwant: %q", index, got, entry.content)
		}
		assertSnapshotInvariants(t, entry.snapshot)
	}
}

// TestPropertyNewlineMetadataConsistency verifies that after any sequence of
// edits, the tree's reported LineBreaks count never exceeds a manual scan of
// the full content, and equals it whenever the tree reports newlines as known.
func TestPropertyNewlineMetadataConsistency(t *testing.T) {
	rng := rand.New(rand.NewPCG(211, 223))
	base := []byte("a\nb\nc\r\nd")
	reference := append([]byte(nil), base...)
	tree := mustNewTree(t, base)
	var journal appendSource
	tree.SetSource(SourceJournal, journal.reader())

	for edit := 0; edit < 300; edit++ {
		start := rng.Int64N(int64(len(reference) + 1))
		maxDelete := int64(len(reference)) - start
		deleteLength := int64(0)
		if maxDelete > 0 {
			deleteLength = rng.Int64N(min(maxDelete, 8) + 1)
		}
		choices := []string{"x", "\n", "ab\r\ncd", "\r\n", ""}
		insert := []byte(choices[rng.IntN(len(choices))])
		offset := journal.add(insert)
		tree.SetSource(SourceJournal, journal.reader())
		piece := Piece{}
		if len(insert) > 0 {
			piece = Piece{Source: SourceJournal, Offset: offset, Length: int64(len(insert)),
				Newlines: int64(bytes.Count(insert, []byte{'\n'})), NewlinesKnown: true}
		}
		if _, _, err := tree.ReplacePiece(start, deleteLength, piece); err != nil {
			t.Fatalf("edit %d: %v", edit, err)
		}
		reference = splice(reference, int(start), int(deleteLength), insert)
		count, known := tree.LineBreaks()
		manual := int64(bytes.Count(reference, []byte{'\n'}))
		if known && count != manual {
			t.Fatalf("edit %d: known line breaks = %d, manual = %d", edit, count, manual)
		}
		if count > manual {
			t.Fatalf("edit %d: line breaks %d exceed manual count %d", edit, count, manual)
		}
	}
	assertTreeInvariants(t, tree)
}

// TestPropertyPieceCountBoundUnderSequentialOps verifies that after N inserts
// and a bounded number of deletes, the piece count stays within O(N). An
// unbounded growth would indicate fragmentation that the treap merge fails to
// coalesce.
func TestPropertyPieceCountBoundUnderSequentialOps(t *testing.T) {
	rng := rand.New(rand.NewPCG(313, 317))
	const operations = 2000
	tree := mustNewTree(t, nil)
	var journal appendSource
	tree.SetSource(SourceJournal, journal.reader())
	for op := 0; op < operations; op++ {
		insert := []byte{byte('a' + rng.IntN(26))}
		offset := journal.add(insert)
		tree.SetSource(SourceJournal, journal.reader())
		pos := rng.Int64N(tree.Len() + 1)
		if _, _, err := tree.ReplacePiece(pos, 0, Piece{Source: SourceJournal, Offset: offset, Length: 1}); err != nil {
			t.Fatalf("op %d insert: %v", op, err)
		}
		if tree.Len() > 32 && rng.IntN(3) == 0 {
			delAt := rng.Int64N(tree.Len())
			if _, _, err := tree.ReplacePiece(delAt, 1, Piece{}); err != nil {
				t.Fatalf("op %d delete: %v", op, err)
			}
		}
	}
	pieces := tree.PieceCount()
	if pieces > operations {
		t.Fatalf("piece count = %d after %d operations (no coalescing bound)", pieces, operations)
	}
	assertTreeInvariants(t, tree)
}

// TestPropertyReadAtRandomOffsetsMatchesBytesReader drives the snapshot
// ReadAt oracle on randomly generated content of many lengths to catch
// length-dependent edge cases. The shared checker covers a fixed matrix per
// content; here we exercise many distinct lengths.
func TestPropertyReadAtRandomOffsetsMatchesBytesReader(t *testing.T) {
	rng := rand.New(rand.NewPCG(401, 409))
	for probe := 0; probe < 50; probe++ {
		base := make([]byte, rng.IntN(300))
		for i := range base {
			base[i] = byte(rng.IntN(256))
		}
		tree := mustNewTree(t, base)
		assertReadAtMatchesBytesReader(t, tree.Snapshot(), base)
	}
}

// TestPropertyRestoreToArbitrarySnapshot verifies that Restore to any captured
// snapshot yields exactly that snapshot's content and re-establishes the
// source bindings it carried.
func TestPropertyRestoreToArbitrarySnapshot(t *testing.T) {
	rng := rand.New(rand.NewPCG(503, 509))
	base := []byte("hello world")
	tree := mustNewTree(t, base)
	var journal appendSource
	tree.SetSource(SourceJournal, journal.reader())
	reference := append([]byte(nil), base...)

	snapshots := []Snapshot{tree.Snapshot()}
	for edit := 0; edit < 80; edit++ {
		start := rng.Int64N(int64(len(reference) + 1))
		insert := []byte{byte('A' + rng.IntN(26))}
		offset := journal.add(insert)
		tree.SetSource(SourceJournal, journal.reader())
		if _, _, err := tree.ReplacePiece(start, 0, Piece{Source: SourceJournal, Offset: offset, Length: 1}); err != nil {
			t.Fatalf("edit %d: %v", edit, err)
		}
		reference = splice(reference, int(start), 0, insert)
		snapshots = append(snapshots, tree.Snapshot())
	}

	// Restore to each saved snapshot and verify content equality.
	for index, snap := range snapshots {
		tree.Restore(snap)
		got, err := io.ReadAll(io.NewSectionReader(tree.Snapshot(), 0, tree.Snapshot().Len()))
		if err != nil {
			t.Fatalf("restore %d read: %v", index, err)
		}
		want, err := io.ReadAll(io.NewSectionReader(snap, 0, snap.Len()))
		if err != nil {
			t.Fatalf("restore %d want read: %v", index, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restore %d content\n got: %q\nwant: %q", index, got, want)
		}
		assertTreeInvariants(t, tree)
	}
}

// TestPropertyReplaceFailureLeavesTreeUnchanged checks that any rejected
// ReplacePiece (bad range or unknown source) returns empty snapshots and does
// not mutate the tree root, piece count, or content.
func TestPropertyReplaceFailureLeavesTreeUnchanged(t *testing.T) {
	rng := rand.New(rand.NewPCG(601, 607))
	base := []byte("abcdef")
	for iteration := 0; iteration < 100; iteration++ {
		tree := mustNewTree(t, base)
		tree.SetSource(SourceJournal, bytes.NewReader([]byte("xy")))
		root := tree.root
		pieces := tree.PieceCount()
		var start, del int64
		var piece Piece
		var want error
		switch rng.IntN(4) {
		case 0:
			start = -1
			want = ErrInvalidRange
		case 1:
			start = rng.Int64N(20) + 10
			want = ErrInvalidRange
		case 2:
			del = rng.Int64N(20) + 10
			want = ErrInvalidRange
		case 3:
			piece = Piece{Source: SourceID(99), Length: 1}
			want = ErrUnknownSource
		}
		before, after, err := tree.ReplacePiece(start, del, piece)
		if !errors.Is(err, want) {
			t.Fatalf("iteration %d: error = %v, want %v", iteration, err, want)
		}
		if before.root != nil || after.root != nil {
			t.Fatalf("iteration %d: failed replace returned non-empty snapshots", iteration)
		}
		if tree.root != root || tree.PieceCount() != pieces {
			t.Fatalf("iteration %d: failed replace mutated the tree", iteration)
		}
		assertSnapshot(t, tree.Snapshot(), "abcdef")
		assertTreeInvariants(t, tree)
	}
}

// splice is the reference-model helper: it returns a new slice with the bytes
// [start, start+delete) replaced by insert. It is the in-memory analogue of
// ReplacePiece.
func splice(base []byte, start, delete int, insert []byte) []byte {
	result := make([]byte, 0, len(base)-delete+len(insert))
	result = append(result, base[:start]...)
	result = append(result, insert...)
	result = append(result, base[start+delete:]...)
	return result
}
