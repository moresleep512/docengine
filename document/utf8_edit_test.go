package document

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"unicode/utf8"

	"github.com/moresleep512/docengine/document/store"
)

func TestApplyBatchRejectsNonUTF8BoundariesAtomically(t *testing.T) {
	tests := []struct {
		name      string
		operation ReplaceOperation
	}{
		{name: "insert inside rune", operation: ReplaceOperation{Start: 2, Insert: "x"}},
		{name: "deletion ends inside rune", operation: ReplaceOperation{Start: 1, DeleteLength: 1}},
		{name: "deletion starts inside rune", operation: ReplaceOperation{Start: 2, DeleteLength: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, _, _ := openAtomicTestSession(t, "aé🙂z")
			defer session.Close()
			if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{test.operation}); !errors.Is(err, ErrInvalidUTF8Boundary) {
				t.Fatalf("ApplyBatch error = %v", err)
			}
			assertSessionText(t, session, "aé🙂z")
			metadata := session.Metadata()
			if metadata.Revision != 0 || metadata.Dirty || session.journal != nil {
				t.Fatalf("failed edit published state: metadata=%+v journal=%v", metadata, session.journal)
			}
		})
	}
}

func TestApplyBatchChecksSequentialUTF8Boundaries(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "az")
	defer session.Close()

	_, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 1, Insert: "🙂"},
		{Start: 2, Insert: "x"},
	})
	if !errors.Is(err, ErrInvalidUTF8Boundary) {
		t.Fatalf("ApplyBatch error = %v", err)
	}
	assertSessionText(t, session, "az")
	if metadata := session.Metadata(); metadata.Revision != 0 || metadata.Dirty {
		t.Fatalf("failed batch published state: %+v", metadata)
	}

	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 1, Insert: "🙂"},
		{Start: 5, DeleteLength: 1, Insert: "界"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 2 || result.ByteLength != 8 {
		t.Fatalf("valid batch result = %+v", result)
	}
	assertSessionText(t, session, "a🙂界")
}

func FuzzUTF8ReplacementBoundaries(f *testing.F) {
	f.Add([]byte("aé🙂z"), uint16(1), uint16(6))
	f.Add([]byte("plain"), uint16(2), uint16(2))
	f.Add([]byte{0xff}, uint16(0), uint16(0))
	f.Fuzz(func(t *testing.T, body []byte, rawStart, rawDelete uint16) {
		if len(body) > 4096 || !utf8.Valid(body) {
			t.Skip()
		}
		start := int64(rawStart) % (int64(len(body)) + 1)
		deleteLength := int64(rawDelete) % (int64(len(body)) - start + 1)
		tree, err := store.New(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		err = validateUTF8ReplacementBoundaries(tree, start, deleteLength)
		isBoundary := func(offset int64) bool {
			return offset == 0 || offset == int64(len(body)) || utf8.RuneStart(body[offset])
		}
		wantValid := isBoundary(start) && isBoundary(start+deleteLength)
		if wantValid && err != nil {
			t.Fatalf("valid boundaries (%d,%d) = %v for %x", start, deleteLength, err, body)
		}
		if !wantValid && !errors.Is(err, ErrInvalidUTF8Boundary) {
			t.Fatalf("invalid boundaries (%d,%d) = %v for %x", start, deleteLength, err, body)
		}
	})
}
