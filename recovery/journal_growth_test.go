package recovery

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestJournalSizeAndBatchEncodedSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	journal, _, err := Open(path, Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	if size, err := journal.Size(); err != nil || size != fileHeaderSize {
		t.Fatalf("initial Size = (%d, %v)", size, err)
	}
	operations := []ReplaceOperation{
		{Start: 0, Inserted: []byte("abc")},
		{Start: 1, DeleteLength: 1, Inserted: []byte("xy")},
	}
	encoded, err := BatchEncodedSize(1, 7, operations)
	if err != nil || encoded != batchHeaderSize+2*batchRecordSize+5 {
		t.Fatalf("BatchEncodedSize = (%d, %v)", encoded, err)
	}
	appended, err := journal.AppendBatch(1, 7, operations)
	if err != nil {
		t.Fatal(err)
	}
	if appended.BatchOffset != fileHeaderSize || appended.EndOffset != fileHeaderSize+encoded {
		t.Fatalf("AppendBatch = %+v, encoded=%d", appended, encoded)
	}
	if size, err := journal.Size(); err != nil || size != appended.EndOffset {
		t.Fatalf("final Size = (%d, %v), want %d", size, err, appended.EndOffset)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Size(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Size = %v", err)
	}
}

func TestBatchEncodedSizeRejectsInvalidBatchWithoutAllocation(t *testing.T) {
	tests := []struct {
		revision   uint64
		group      uint64
		operations []ReplaceOperation
	}{
		{group: 1, operations: []ReplaceOperation{{}}},
		{revision: 1, operations: []ReplaceOperation{{}}},
		{revision: 1, group: 1},
		{revision: 1, group: 1, operations: make([]ReplaceOperation, maximumBatchSize+1)},
		{revision: math.MaxUint64, group: 1, operations: []ReplaceOperation{{}, {}}},
		{revision: 1, group: 1, operations: []ReplaceOperation{{Start: -1}}},
		{revision: 1, group: 1, operations: []ReplaceOperation{{DeleteLength: -1}}},
	}
	for index, test := range tests {
		if _, err := BatchEncodedSize(test.revision, test.group, test.operations); !errors.Is(err, ErrInvalidBatch) {
			t.Fatalf("case %d = %v", index, err)
		}
	}
}

func TestJournalSizePropagatesStatFailure(t *testing.T) {
	sentinel := errors.New("stat")
	file := createFaultBase(t)
	journal := &Journal{file: &faultJournalFile{base: file, statFaults: map[int]error{1: sentinel}}}
	if _, err := journal.Size(); !errors.Is(err, sentinel) {
		t.Fatalf("Size = %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}
