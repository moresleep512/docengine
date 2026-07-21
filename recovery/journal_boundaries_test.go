package recovery

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndFingerprintFilesystemFailures(t *testing.T) {
	t.Run("parent is file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(filepath.Join(parent, "journal"), Fingerprint{}); err == nil {
			t.Fatal("expected mkdir failure")
		}
	})
	t.Run("journal is directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path, Fingerprint{}); err == nil {
			t.Fatal("expected open failure")
		}
	})
	t.Run("short header", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal")
		if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path, Fingerprint{}); err == nil {
			t.Fatal("short header accepted")
		}
	})
}

func TestEveryBatchTruncationIsAtomic(t *testing.T) {
	journal, path := openTestJournal(t)
	fingerprint, _ := ReadFingerprint(path)
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: []byte("first")}, {Start: 5, Inserted: []byte("second")}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	complete, _ := os.ReadFile(path)
	for cut := fileHeaderSize + 1; cut < len(complete); cut++ {
		cut := cut
		t.Run("cut-"+string(rune(cut)), func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "journal")
			if err := os.WriteFile(candidate, complete[:cut], 0o600); err != nil {
				t.Fatal(err)
			}
			opened, replay, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if !replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Batches) != 0 {
				t.Fatalf("cut %d replay = %+v", cut, replay)
			}
		})
	}
}

func TestReplayRejectsOversizedAndBadCRCHeaders(t *testing.T) {
	journal, path := openTestJournal(t)
	fingerprint, _ := ReadFingerprint(path)
	_ = journal.Close()
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "oversized payload", mutate: func(header []byte) {
			binary.LittleEndian.PutUint64(header[40:48], maximumBatchPayload+1)
			binary.LittleEndian.PutUint32(header[56:60], crc32.Checksum(header[:56], castagnoli))
		}},
		{name: "bad crc", mutate: func(header []byte) { header[56] ^= 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := encodeBatchHeader(Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}, make([]byte, batchRecordSize))
			test.mutate(header)
			candidate := filepath.Join(t.TempDir(), "journal")
			base, _ := os.ReadFile(path)
			base = append(base, header...)
			if err := os.WriteFile(candidate, base, 0o600); err != nil {
				t.Fatal(err)
			}
			opened, replay, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if !replay.Truncated || len(replay.Batches) != 0 {
				t.Fatalf("Replay = %+v", replay)
			}
		})
	}
}

func TestDecodeBatchOperationsReadFailure(t *testing.T) {
	file, err := os.Open(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		_ = file.Close()
		t.Fatal("unexpected file")
	}
	shortPath := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(shortPath, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	short, err := os.Open(shortPath)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	operations, valid, err := decodeBatchOperations(short, Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}, 0, batchRecordSize)
	if err == nil || valid || operations != nil {
		t.Fatalf("decode = (%+v, %v, %v)", operations, valid, err)
	}
}

func TestLowLevelReadOnlyFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	if err := os.WriteFile(path, make([]byte, fileHeaderSize), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	journal := &Journal{file: readOnly, path: path}
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: []byte("x")}}); err == nil {
		t.Fatal("read-only append succeeded")
	}
	if err := journal.Reset(Fingerprint{}); err == nil {
		t.Fatal("read-only reset succeeded")
	}
	if err := writeFileHeader(readOnly, Fingerprint{}); err == nil {
		t.Fatal("read-only header write succeeded")
	}
	_ = readOnly.Close()
	if _, _, err := journal.appendBatch(Batch{FirstRevision: 1, Group: 1}, nil); err == nil {
		t.Fatal("append on closed base succeeded")
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("replay on closed base succeeded")
	}
	if !errors.Is(journal.Close(), os.ErrClosed) && !errors.Is(journal.Close(), io.ErrClosedPipe) {
		// os.File.Close commonly returns os.ErrClosed; other implementations may
		// return nil on a repeated close, either is acceptable here.
	}
}
