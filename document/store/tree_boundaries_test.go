package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
)

func TestConstructorsValidateBasePiece(t *testing.T) {
	tests := []struct {
		name  string
		base  io.ReaderAt
		piece Piece
		want  error
	}{
		{name: "empty nil source is valid", piece: Piece{}},
		{name: "positive length needs source", piece: Piece{Length: 1}, want: ErrUnknownSource},
		{name: "negative length", base: zeroReaderAt{}, piece: Piece{Length: -1}, want: ErrInvalidPiece},
		{name: "negative offset", base: zeroReaderAt{}, piece: Piece{Offset: -1, Length: 1}, want: ErrInvalidPiece},
		{name: "source end overflow", base: zeroReaderAt{}, piece: Piece{Offset: math.MaxInt64, Length: 1}, want: ErrInvalidPiece},
		{name: "negative known newlines", base: zeroReaderAt{}, piece: Piece{Length: 2, Newlines: -1, NewlinesKnown: true}, want: ErrInvalidPiece},
		{name: "too many known newlines", base: zeroReaderAt{}, piece: Piece{Length: 2, Newlines: 3, NewlinesKnown: true}, want: ErrInvalidPiece},
		{name: "valid known newlines", base: zeroReaderAt{}, piece: Piece{Offset: 4, Length: 5, Newlines: 2, NewlinesKnown: true}},
		{name: "unknown newlines are normalized", base: zeroReaderAt{}, piece: Piece{Length: 5, Newlines: math.MaxInt64}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, err := NewWithBasePiece(test.base, test.piece)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if test.want != nil {
				if tree != nil {
					t.Fatal("invalid constructor returned a tree")
				}
				return
			}
			assertTreeInvariants(t, tree)
			if tree.Len() != test.piece.Length {
				t.Fatalf("length = %d, want %d", tree.Len(), test.piece.Length)
			}
		})
	}
}

func TestReplacePieceBoundaryCases(t *testing.T) {
	tests := []struct {
		name          string
		base          string
		start, delete int64
		insert        string
		want          string
	}{
		{name: "insert into empty", start: 0, insert: "x", want: "x"},
		{name: "insert at beginning", base: "abc", start: 0, insert: "x", want: "xabc"},
		{name: "insert in middle", base: "abc", start: 1, insert: "x", want: "axbc"},
		{name: "insert at end", base: "abc", start: 3, insert: "x", want: "abcx"},
		{name: "delete beginning", base: "abc", start: 0, delete: 1, want: "bc"},
		{name: "delete middle", base: "abc", start: 1, delete: 1, want: "ac"},
		{name: "delete end", base: "abc", start: 2, delete: 1, want: "ab"},
		{name: "delete all", base: "abc", start: 0, delete: 3, want: ""},
		{name: "replace with longer text", base: "abc", start: 1, delete: 1, insert: "WXYZ", want: "aWXYZc"},
		{name: "replace with shorter text", base: "abcdef", start: 1, delete: 4, insert: "X", want: "aXf"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree := mustNewTree(t, []byte(test.base))
			tree.SetSource(SourceJournal, bytes.NewReader([]byte(test.insert)))
			replacement := Piece{}
			if test.insert != "" {
				replacement = Piece{
					Source:        SourceJournal,
					Length:        int64(len(test.insert)),
					Newlines:      int64(bytes.Count([]byte(test.insert), []byte{'\n'})),
					NewlinesKnown: true,
				}
			}
			before, after, err := tree.ReplacePiece(test.start, test.delete, replacement)
			if err != nil {
				t.Fatal(err)
			}
			assertSnapshot(t, before, test.base)
			assertSnapshot(t, after, test.want)
			assertSnapshot(t, tree.Snapshot(), test.want)
			assertTreeInvariants(t, tree)
			assertSnapshotInvariants(t, before)
			assertSnapshotInvariants(t, after)
		})
	}
}

func TestNoOpReplacementDoesNotFragmentTree(t *testing.T) {
	tree := mustNewTree(t, []byte("abcdef"))
	root := tree.root
	pieces := tree.PieceCount()
	before, after, err := tree.ReplacePiece(3, 0, Piece{Offset: -10, Newlines: -20})
	if err != nil {
		t.Fatal(err)
	}
	if tree.root != root {
		t.Fatal("no-op replacement changed the root")
	}
	if tree.PieceCount() != pieces {
		t.Fatalf("piece count = %d, want %d", tree.PieceCount(), pieces)
	}
	if before.root != root || after.root != root {
		t.Fatal("no-op snapshots do not share the existing root")
	}
	assertTreeInvariants(t, tree)
}

func TestEmptyTreeMetadataReadAndWrite(t *testing.T) {
	tree := mustNewTree(t, nil)
	if count, known := tree.LineBreaks(); count != 0 || !known {
		t.Fatalf("line breaks = (%d, %v), want (0, true)", count, known)
	}
	if tree.Len() != 0 || tree.PieceCount() != 0 {
		t.Fatalf("empty tree summary = (%d bytes, %d pieces)", tree.Len(), tree.PieceCount())
	}
	snapshot := tree.Snapshot()
	if n, err := snapshot.WriteTo(io.Discard); n != 0 || err != nil {
		t.Fatalf("empty WriteTo = (%d, %v), want (0, nil)", n, err)
	}
	buffer := make([]byte, 1)
	if n, err := snapshot.ReadAt(buffer, 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("empty ReadAt = (%d, %v), want (0, EOF)", n, err)
	}
	assertTreeInvariants(t, tree)
}

func TestInvalidReplacementLeavesTreeUnchanged(t *testing.T) {
	tests := []struct {
		name          string
		start, delete int64
		piece         Piece
		want          error
	}{
		{name: "negative start", start: -1, piece: Piece{}, want: ErrInvalidRange},
		{name: "negative delete", delete: -1, piece: Piece{}, want: ErrInvalidRange},
		{name: "start past end", start: 7, piece: Piece{}, want: ErrInvalidRange},
		{name: "delete past end", start: 5, delete: 2, piece: Piece{}, want: ErrInvalidRange},
		{name: "negative piece length", piece: Piece{Source: SourceJournal, Length: -1}, want: ErrInvalidPiece},
		{name: "negative source offset", piece: Piece{Source: SourceJournal, Offset: -1, Length: 1}, want: ErrInvalidPiece},
		{name: "source range overflow", piece: Piece{Source: SourceJournal, Offset: math.MaxInt64, Length: 1}, want: ErrInvalidPiece},
		{name: "negative newline count", piece: Piece{Source: SourceJournal, Length: 2, Newlines: -1, NewlinesKnown: true}, want: ErrInvalidPiece},
		{name: "newline count exceeds bytes", piece: Piece{Source: SourceJournal, Length: 2, Newlines: 3, NewlinesKnown: true}, want: ErrInvalidPiece},
		{name: "unknown source", piece: Piece{Source: SourceID(99), Length: 1}, want: ErrUnknownSource},
		{name: "document length overflow", piece: Piece{Source: SourceJournal, Length: math.MaxInt64}, want: ErrLengthOverflow},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree := mustNewTree(t, []byte("abcdef"))
			tree.SetSource(SourceJournal, zeroReaderAt{})
			original := tree.Snapshot()
			originalRoot := tree.root
			originalPieces := tree.PieceCount()
			before, after, err := tree.ReplacePiece(test.start, test.delete, test.piece)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if before.root != nil || after.root != nil {
				t.Fatal("failed replacement returned non-zero snapshots")
			}
			if tree.root != originalRoot || tree.PieceCount() != originalPieces {
				t.Fatal("failed replacement mutated the tree")
			}
			assertSnapshot(t, tree.Snapshot(), "abcdef")
			assertSnapshot(t, original, "abcdef")
			assertTreeInvariants(t, tree)
		})
	}
}

func TestReplaceAcrossManyPieces(t *testing.T) {
	tree := mustNewTree(t, []byte("abcdef"))
	journal := []byte("123XYZ!")
	tree.SetSource(SourceJournal, bytes.NewReader(journal))
	if _, _, err := tree.ReplacePiece(2, 0, Piece{Source: SourceJournal, Length: 3}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tree.ReplacePiece(7, 0, Piece{Source: SourceJournal, Offset: 3, Length: 3}); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, tree.Snapshot(), "ab123cdXYZef")
	if _, _, err := tree.ReplacePiece(1, 10, Piece{Source: SourceJournal, Offset: 6, Length: 1}); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, tree.Snapshot(), "a!f")
	assertTreeInvariants(t, tree)
}

func TestSnapshotsAndRestorePreserveSourceBindings(t *testing.T) {
	tree := mustNewTree(t, []byte("A"))
	tree.SetSource(SourceJournal, bytes.NewReader([]byte("old")))
	_, oldSnapshot, err := tree.ReplacePiece(1, 0, Piece{Source: SourceJournal, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, oldSnapshot, "Aold")

	tree.SetSource(SourceJournal, bytes.NewReader([]byte("new")))
	assertSnapshot(t, tree.Snapshot(), "Anew")
	assertSnapshot(t, oldSnapshot, "Aold")

	tree.Restore(oldSnapshot)
	assertSnapshot(t, tree.Snapshot(), "Aold")

	tree.SetSource(SourceJournal, nil)
	buffer := make([]byte, tree.Len())
	if _, err := tree.ReadAt(buffer, 0); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("read after source removal = %v, want %v", err, ErrUnknownSource)
	}
	if n, err := tree.Snapshot().WriteTo(io.Discard); n != 1 || !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("write after source removal = (%d, %v), want (1, %v)", n, err, ErrUnknownSource)
	}
	assertSnapshot(t, oldSnapshot, "Aold")
	assertTreeInvariantsAllowMissingSources(t, tree)
}

func TestSnapshotReadAtBoundaries(t *testing.T) {
	snapshot := mustNewTree(t, []byte("abcdef")).Snapshot()
	tests := []struct {
		name     string
		offset   int64
		size     int
		wantText string
		wantN    int
		wantErr  error
	}{
		{name: "negative offset", offset: -1, size: 1, wantErr: ErrInvalidRange},
		{name: "zero buffer", offset: 3, size: 0, wantN: 0},
		{name: "zero buffer past end", offset: 99, size: 0, wantN: 0},
		{name: "offset at end", offset: 6, size: 1, wantErr: io.EOF},
		{name: "offset past end", offset: 7, size: 1, wantErr: io.EOF},
		{name: "exact read", offset: 1, size: 3, wantText: "bcd", wantN: 3},
		{name: "partial read", offset: 4, size: 5, wantText: "ef", wantN: 2, wantErr: io.EOF},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := make([]byte, test.size)
			n, err := snapshot.ReadAt(buffer, test.offset)
			if n != test.wantN || string(buffer[:n]) != test.wantText {
				t.Fatalf("read = (%d, %q), want (%d, %q)", n, buffer[:n], test.wantN, test.wantText)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestShortSourceErrorsPropagate(t *testing.T) {
	tree, err := New(bytes.NewReader([]byte("ab")), 5)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	n, err := tree.ReadAt(buffer, 0)
	if n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt = (%d, %v), want (2, EOF)", n, err)
	}
	var output bytes.Buffer
	n64, err := tree.Snapshot().WriteTo(&output)
	if n64 != 2 || !errors.Is(err, io.EOF) || output.String() != "ab" {
		t.Fatalf("WriteTo = (%d, %v, %q), want (2, EOF, %q)", n64, err, output.String(), "ab")
	}
}

func TestWriteToPropagatesWriterFailure(t *testing.T) {
	snapshot := mustNewTree(t, []byte("abcdef")).Snapshot()
	writer := &failingWriter{remaining: 3}
	n, err := snapshot.WriteTo(writer)
	if n != 3 || !errors.Is(err, errWriterFailed) {
		t.Fatalf("WriteTo = (%d, %v), want (3, %v)", n, err, errWriterFailed)
	}
}

func TestLineBreakMetadata(t *testing.T) {
	tree, err := NewWithBasePiece(bytes.NewReader([]byte("a\nb\n")), Piece{Length: 4, Newlines: 2, NewlinesKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	if count, known := tree.LineBreaks(); count != 2 || !known {
		t.Fatalf("initial line breaks = (%d, %v), want (2, true)", count, known)
	}
	tree.SetSource(SourceJournal, bytes.NewReader([]byte("\nX")))
	if _, _, err := tree.ReplacePiece(4, 0, Piece{Source: SourceJournal, Length: 1, Newlines: 1, NewlinesKnown: true}); err != nil {
		t.Fatal(err)
	}
	if count, known := tree.LineBreaks(); count != 3 || !known {
		t.Fatalf("line breaks after append = (%d, %v), want (3, true)", count, known)
	}
	if _, _, err := tree.ReplacePiece(1, 0, Piece{Source: SourceJournal, Offset: 1, Length: 1, NewlinesKnown: true}); err != nil {
		t.Fatal(err)
	}
	if count, known := tree.LineBreaks(); count != 1 || known {
		t.Fatalf("line breaks after splitting known base = (%d, %v), want (1, false)", count, known)
	}
	assertTreeInvariants(t, tree)
}

func TestSequentialInsertsRemainBalanced(t *testing.T) {
	const insertions = 10_000
	tree, err := NewWithOptions(nil, 0, Options{DisableAutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(SourceJournal, bytes.NewReader(bytes.Repeat([]byte{'x'}, insertions)))
	for position := 0; position < insertions; position++ {
		if _, _, err := tree.ReplacePiece(int64(position), 0, Piece{Source: SourceJournal, Offset: int64(position), Length: 1}); err != nil {
			t.Fatalf("insert %d: %v", position, err)
		}
	}
	assertTreeInvariants(t, tree)
	if height := nodeHeight(tree.root); height > 128 {
		t.Fatalf("tree height = %d after %d sequential inserts", height, insertions)
	}
	if tree.PieceCount() != insertions || tree.Len() != insertions {
		t.Fatalf("tree summary = (%d pieces, %d bytes)", tree.PieceCount(), tree.Len())
	}
}

func TestConcurrentSnapshotsDuringEdits(t *testing.T) {
	const edits = 2_000
	tree := mustNewTree(t, bytes.Repeat([]byte{'a'}, 128))
	tree.SetSource(SourceJournal, bytes.NewReader(bytes.Repeat([]byte{'b'}, edits)))

	done := make(chan struct{})
	errorsFound := make(chan error, 8)
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				snapshot := tree.Snapshot()
				if snapshot.Len() == 0 {
					continue
				}
				size := min(snapshot.Len(), 64)
				buffer := make([]byte, size)
				n, err := snapshot.ReadAt(buffer, 0)
				if err != nil || int64(n) != size {
					select {
					case errorsFound <- fmt.Errorf("snapshot read = (%d, %v), want (%d, nil)", n, err, size):
					default:
					}
					return
				}
			}
		}()
	}

	for edit := 0; edit < edits; edit++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{Source: SourceJournal, Offset: int64(edit), Length: 1}); err != nil {
			t.Fatal(err)
		}
		if tree.Len() > 256 {
			if _, _, err := tree.ReplacePiece(0, 1, Piece{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(done)
	readers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	assertTreeInvariants(t, tree)
}

func TestNilInternalHelpersAreSafe(t *testing.T) {
	if cloneNode(nil, nil, nil) != nil {
		t.Fatal("cloneNode(nil) returned a node")
	}
	if recalc(nil) != nil {
		t.Fatal("recalc(nil) returned a node")
	}
	buffer := make([]byte, 1)
	if n, err := readNode(nil, nil, 0, buffer); n != 0 || err != nil {
		t.Fatalf("readNode(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if err := walk(nil, func(Piece) error { return errors.New("must not be called") }); err != nil {
		t.Fatalf("walk(nil) = %v, want nil", err)
	}
}

func TestInternalTraversalErrorsPropagateFromLeftSubtree(t *testing.T) {
	left := recalc(&node{piece: Piece{Source: SourceBase, Length: 1}, priority: 1})
	root := recalc(&node{piece: Piece{Source: SourceJournal, Length: 1}, priority: 2, left: left})
	if err := walk(root, func(Piece) error { return errSourceFailed }); !errors.Is(err, errSourceFailed) {
		t.Fatalf("walk error = %v, want %v", err, errSourceFailed)
	}
	buffer := make([]byte, 1)
	n, err := readNode(root, map[SourceID]io.ReaderAt{
		SourceBase:    errorReaderAt{},
		SourceJournal: bytes.NewReader([]byte("x")),
	}, 0, buffer)
	if n != 0 || !errors.Is(err, errSourceFailed) {
		t.Fatalf("readNode error = (%d, %v), want (0, %v)", n, err, errSourceFailed)
	}
}

type zeroReaderAt struct{}

func (zeroReaderAt) ReadAt(buffer []byte, _ int64) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

var errWriterFailed = errors.New("writer failed")

var errSourceFailed = errors.New("source failed")

type errorReaderAt struct{}

func (errorReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, errSourceFailed
}

type failingWriter struct {
	remaining int
}

func (w *failingWriter) Write(buffer []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errWriterFailed
	}
	n := min(len(buffer), w.remaining)
	w.remaining -= n
	if n < len(buffer) {
		return n, errWriterFailed
	}
	return n, nil
}

func mustNewTree(t testing.TB, base []byte) *Tree {
	t.Helper()
	tree, err := New(bytes.NewReader(base), int64(len(base)))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func assertTreeInvariants(t testing.TB, tree *Tree) {
	t.Helper()
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	assertNodeInvariants(t, tree.root, tree.sources, false, make(map[*node]struct{}))
}

func assertTreeInvariantsAllowMissingSources(t testing.TB, tree *Tree) {
	t.Helper()
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	assertNodeInvariants(t, tree.root, tree.sources, true, make(map[*node]struct{}))
}

func assertSnapshotInvariants(t testing.TB, snapshot Snapshot) {
	t.Helper()
	assertNodeInvariants(t, snapshot.root, snapshot.sources, false, make(map[*node]struct{}))
}

type invariantStats struct {
	bytes, pieces, newlines int64
	newlinesKnown           bool
}

func assertNodeInvariants(t testing.TB, current *node, sources map[SourceID]io.ReaderAt, allowMissingSources bool, seen map[*node]struct{}) invariantStats {
	t.Helper()
	if current == nil {
		return invariantStats{newlinesKnown: true}
	}
	if _, exists := seen[current]; exists {
		t.Fatal("tree contains a cycle or repeated node")
	}
	seen[current] = struct{}{}
	if current.piece.Length <= 0 {
		t.Fatalf("node contains non-positive piece length: %+v", current.piece)
	}
	if err := validatePiece(current.piece); err != nil {
		t.Fatalf("node contains invalid piece %+v: %v", current.piece, err)
	}
	if !allowMissingSources && sources[current.piece.Source] == nil {
		t.Fatalf("piece references missing source %d", current.piece.Source)
	}
	if current.left != nil && current.left.priority > current.priority {
		t.Fatalf("left child priority %d exceeds parent priority %d (parent piece %+v, child piece %+v)", current.left.priority, current.priority, current.piece, current.left.piece)
	}
	if current.right != nil && current.right.priority > current.priority {
		t.Fatalf("right child priority %d exceeds parent priority %d (parent piece %+v, child piece %+v)", current.right.priority, current.priority, current.piece, current.right.piece)
	}

	left := assertNodeInvariants(t, current.left, sources, allowMissingSources, seen)
	right := assertNodeInvariants(t, current.right, sources, allowMissingSources, seen)
	if left.bytes > math.MaxInt64-current.piece.Length || left.bytes+current.piece.Length > math.MaxInt64-right.bytes {
		t.Fatal("subtree byte count overflows int64")
	}
	wantBytes := left.bytes + current.piece.Length + right.bytes
	wantPieces := left.pieces + 1 + right.pieces
	wantNewlines := left.newlines + current.piece.Newlines + right.newlines
	wantKnown := left.newlinesKnown && current.piece.NewlinesKnown && right.newlinesKnown
	if current.bytes != wantBytes || current.pieceCount != wantPieces || current.newlines != wantNewlines || current.newlinesKnown != wantKnown {
		t.Fatalf("cached node summary = (%d bytes, %d pieces, %d newlines, known=%v), want (%d, %d, %d, %v)",
			current.bytes, current.pieceCount, current.newlines, current.newlinesKnown,
			wantBytes, wantPieces, wantNewlines, wantKnown)
	}
	return invariantStats{bytes: wantBytes, pieces: wantPieces, newlines: wantNewlines, newlinesKnown: wantKnown}
}

func nodeHeight(current *node) int {
	if current == nil {
		return 0
	}
	return 1 + max(nodeHeight(current.left), nodeHeight(current.right))
}
