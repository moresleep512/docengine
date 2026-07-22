package store

import (
	"io"
	"strings"
	"testing"
)

func TestCompactCoalescesContiguousSourcesAndPreservesSnapshots(t *testing.T) {
	tree, err := New(strings.NewReader("abcdef"), 6)
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(SourceJournal, strings.NewReader("X"))
	if _, _, err := tree.ReplacePiece(3, 0, Piece{Source: SourceJournal, Length: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tree.ReplacePiece(3, 1, Piece{}); err != nil {
		t.Fatal(err)
	}
	before := tree.Snapshot()
	if tree.PieceCount() != 2 {
		t.Fatalf("fragment count = %d", tree.PieceCount())
	}
	result := tree.Compact()
	if result != (CompactResult{BeforePieces: 2, AfterPieces: 1}) || tree.PieceCount() != 1 {
		t.Fatalf("compact = %+v, pieces = %d", result, tree.PieceCount())
	}
	for name, source := range map[string]io.ReaderAt{"snapshot": before, "tree": tree} {
		buffer := make([]byte, 6)
		if n, err := source.ReadAt(buffer, 0); n != 6 || err != nil || string(buffer) != "abcdef" {
			t.Fatalf("%s = (%q, %d, %v)", name, buffer, n, err)
		}
	}
	if _, known := tree.LineBreaks(); known {
		t.Fatal("unknown split newline metadata became known")
	}
}

func TestCompactPreservesKnownNewlinesAndNoncontiguousPieces(t *testing.T) {
	tree, err := New(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(SourceJournal, strings.NewReader("a\nb\n--z"))
	for _, piece := range []Piece{
		{Source: SourceJournal, Offset: 0, Length: 2, Newlines: 1, NewlinesKnown: true},
		{Source: SourceJournal, Offset: 2, Length: 2, Newlines: 1, NewlinesKnown: true},
		{Source: SourceJournal, Offset: 6, Length: 1, NewlinesKnown: true},
	} {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, piece); err != nil {
			t.Fatal(err)
		}
	}
	result := tree.Compact()
	if result != (CompactResult{BeforePieces: 3, AfterPieces: 2}) {
		t.Fatalf("compact = %+v", result)
	}
	if lines, known := tree.LineBreaks(); lines != 2 || !known {
		t.Fatalf("line metadata = (%d, %v)", lines, known)
	}
	if second := tree.Compact(); second != (CompactResult{BeforePieces: 2, AfterPieces: 2}) {
		t.Fatalf("stable compact = %+v", second)
	}

	empty, _ := New(nil, 0)
	if result := empty.Compact(); result != (CompactResult{}) {
		t.Fatalf("empty compact = %+v", result)
	}
	single, _ := New(strings.NewReader("x"), 1)
	if result := single.Compact(); result != (CompactResult{BeforePieces: 1, AfterPieces: 1}) {
		t.Fatalf("single compact = %+v", result)
	}
}
