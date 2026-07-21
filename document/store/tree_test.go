package store

import (
	"bytes"
	"io"
	"math/rand/v2"
	"testing"
)

func TestTreeReplaceReadAndRestore(t *testing.T) {
	base := bytes.NewReader([]byte("hello world"))
	journal := bytes.NewReader([]byte("engine"))
	tree := New(base, 11)
	tree.SetSource(SourceJournal, journal)
	before, after, err := tree.ReplacePiece(6, 5, Piece{Source: SourceJournal, Length: 6})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, after, "hello engine")
	tree.Restore(before)
	assertSnapshot(t, tree.Snapshot(), "hello world")
}

func TestTreeRandomReplacementsMatchReference(t *testing.T) {
	baseBytes := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	addBytes := make([]byte, 0, 32_000)
	tree := New(bytes.NewReader(baseBytes), int64(len(baseBytes)))
	reference := append([]byte(nil), baseBytes...)
	rng := rand.New(rand.NewPCG(1, 2))

	for i := 0; i < 2_000; i++ {
		start := rng.IntN(len(reference) + 1)
		maxDelete := len(reference) - start
		deleteLength := 0
		if maxDelete > 0 {
			deleteLength = rng.IntN(min(maxDelete, 12) + 1)
		}
		insert := make([]byte, rng.IntN(12))
		for j := range insert {
			insert[j] = byte('A' + rng.IntN(26))
		}
		offset := len(addBytes)
		addBytes = append(addBytes, insert...)
		tree.SetSource(SourceJournal, bytes.NewReader(addBytes))
		piece := Piece{}
		if len(insert) > 0 {
			piece = Piece{Source: SourceJournal, Offset: int64(offset), Length: int64(len(insert))}
		}
		_, _, err := tree.ReplacePiece(int64(start), int64(deleteLength), piece)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		next := make([]byte, 0, len(reference)-deleteLength+len(insert))
		next = append(next, reference[:start]...)
		next = append(next, insert...)
		next = append(next, reference[start+deleteLength:]...)
		reference = next
		assertSnapshotBytes(t, tree.Snapshot(), reference)
	}
}

func assertSnapshot(t *testing.T, snapshot Snapshot, expected string) {
	t.Helper()
	assertSnapshotBytes(t, snapshot, []byte(expected))
}

func assertSnapshotBytes(t *testing.T, snapshot Snapshot, expected []byte) {
	t.Helper()
	got, err := io.ReadAll(io.NewSectionReader(snapshot, 0, snapshot.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("content mismatch\n got: %q\nwant: %q", got, expected)
	}
}
