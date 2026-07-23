package store_test

import (
	"bytes"
	"fmt"
	"io"

	"github.com/moresleep512/docengine/document/store"
)

func ExampleTree_automaticCompaction() {
	tree, _ := store.NewWithOptions(nil, 0, store.Options{AutoCompactPieces: 2})
	tree.SetSource(store.SourceJournal, bytes.NewReader([]byte("ab")))

	_, first, _ := tree.ReplacePiece(0, 0, store.Piece{
		Source: store.SourceJournal, Length: 1,
	})
	_, _, _ = tree.ReplacePiece(1, 0, store.Piece{
		Source: store.SourceJournal, Offset: 1, Length: 1,
	})

	old, _ := io.ReadAll(io.NewSectionReader(first, 0, first.Len()))
	current := tree.Snapshot()
	body, _ := io.ReadAll(io.NewSectionReader(current, 0, current.Len()))
	stats := tree.Stats()
	fmt.Printf("old=%s current=%s pieces=%d automatic=%d\n",
		old, body, stats.PieceCount, stats.AutoCompactions)
	// Output:
	// old=a current=ab pieces=1 automatic=1
}
