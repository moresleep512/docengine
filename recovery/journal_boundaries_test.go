package recovery

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalLifecycleResetAndClosedErrors(t *testing.T) {
	journal, path := openTestJournal(t)
	if journal.Path() != path {
		t.Fatalf("Path = %q, want %q", journal.Path(), path)
	}
	if _, err := journal.AppendReplaceGroup(1, 0, 0, []byte("x"), 9); err != nil {
		t.Fatal(err)
	}
	if err := journal.AppendRoot(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := journal.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := journal.RepairTail(fileHeaderSize - 1); err == nil {
		t.Fatal("expected invalid repair offset")
	}
	fingerprint := Fingerprint{BaseSize: 7, ModTimeNanos: 11, PathHash: [32]byte{1, 2, 3}}
	if err := journal.Reset(fingerprint); err != nil {
		t.Fatal(err)
	}
	stored, err := ReadFingerprint(path)
	if err != nil || stored != fingerprint {
		t.Fatalf("fingerprint = (%+v, %v), want %+v", stored, err, fingerprint)
	}
	replay, err := journal.Replay()
	if err != nil || replay.ValidBytes != fileHeaderSize || replay.Truncated || len(replay.Frames) != 0 {
		t.Fatalf("replay after reset = (%+v, %v)", replay, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := journal.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadAt error = %v", err)
	}
	if _, err := journal.AppendReplaceGroup(3, 0, 0, nil, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("AppendReplaceGroup error = %v", err)
	}
	if err := journal.AppendRoot(3, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("AppendRoot error = %v", err)
	}
	if _, err := journal.Replay(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Replay error = %v", err)
	}
	if err := journal.RepairTail(fileHeaderSize); !errors.Is(err, ErrClosed) {
		t.Fatalf("RepairTail error = %v", err)
	}
	if err := journal.Sync(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Sync error = %v", err)
	}
	if err := journal.Reset(Fingerprint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reset error = %v", err)
	}
}

func TestOpenAndFingerprintRejectFilesystemAndHeaderFailures(t *testing.T) {
	t.Run("recovery directory is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(filepath.Join(parent, "journal"), Fingerprint{}); err == nil {
			t.Fatal("expected MkdirAll failure")
		}
	})

	t.Run("journal path is a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path, Fingerprint{}); err == nil {
			t.Fatal("expected OpenFile failure")
		}
	})

	tests := []struct {
		name   string
		bytes  []byte
		mutate func([]byte)
	}{
		{name: "short", bytes: []byte("short")},
		{name: "unsupported magic", mutate: func(header []byte) { header[0] ^= 1 }},
		{name: "unsupported version", mutate: func(header []byte) { binary.LittleEndian.PutUint32(header[8:12], journalVersion+1) }},
		{name: "bad CRC", mutate: func(header []byte) { header[20] ^= 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal")
			content := test.bytes
			if content == nil {
				content = make([]byte, fileHeaderSize)
				copy(content[:8], fileMagic[:])
				binary.LittleEndian.PutUint32(content[8:12], journalVersion)
				binary.LittleEndian.PutUint32(content[64:68], crc32.Checksum(content[:64], castagnoli))
				test.mutate(content)
			}
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(path, Fingerprint{}); err == nil {
				t.Fatal("expected invalid-header error")
			}
			if _, err := ReadFingerprint(path); err == nil {
				t.Fatal("ReadFingerprint accepted invalid header")
			}
		})
	}
	if _, err := ReadFingerprint(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected missing fingerprint error")
	}
}

func TestLowLevelJournalIOFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, make([]byte, fileHeaderSize), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	journal := &Journal{file: readOnly, path: path}
	if _, err := journal.AppendReplaceBatch(1, 1, []ReplaceOperation{{Inserted: []byte("x")}}); err == nil {
		t.Fatal("expected read-only batch append error")
	}
	if _, err := journal.appendFrame(Frame{Kind: FrameReplace, Revision: 1}, []byte("x")); err == nil {
		t.Fatal("expected read-only append error")
	}
	if err := journal.Reset(Fingerprint{}); err == nil {
		t.Fatal("expected read-only reset error")
	}
	if err := writeFileHeader(readOnly, Fingerprint{}); err == nil {
		t.Fatal("expected read-only header-write error")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.appendFrame(Frame{Kind: FrameReplace, Revision: 1}, nil); err == nil {
		t.Fatal("expected seek error on closed underlying file")
	}
	if _, err := journal.Replay(); err == nil {
		t.Fatal("expected stat error on closed underlying file")
	}
}

func TestReplayRejectsOversizedFrameAndBatchMetadataReadFailure(t *testing.T) {
	journal, path := openTestJournal(t)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, frameHeaderSize)
	copy(header[:8], frameMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], journalVersion)
	binary.LittleEndian.PutUint16(header[10:12], uint16(FrameReplace))
	binary.LittleEndian.PutUint32(header[12:16], frameHeaderSize)
	binary.LittleEndian.PutUint64(header[16:24], 1)
	binary.LittleEndian.PutUint64(header[48:56], maximumFramePayload+1)
	binary.LittleEndian.PutUint32(header[56:60], crc32.Checksum(header[:56], castagnoli))
	if _, err := file.WriteAt(header, fileHeaderSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	opened, replay, err := Open(path, Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Truncated || replay.ValidBytes != fileHeaderSize {
		t.Fatalf("oversized replay = %+v", replay)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	shortPath := filepath.Join(t.TempDir(), "short")
	short, err := os.OpenFile(shortPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	_, valid, err := decodeBatchFrames(short, Frame{
		Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize,
		InsertLength: batchRecordSize, PayloadOffset: 0,
	})
	if err == nil || valid {
		t.Fatalf("decodeBatchFrames = (valid=%v, err=%v)", valid, err)
	}
}

func TestBatchEncoderRejectsPayloadLimitWithoutAllocation(t *testing.T) {
	// A negative-looking length decoded from uint64 is rejected before any
	// payload cursor arithmetic can wrap.
	record := make([]byte, batchRecordSize)
	binary.LittleEndian.PutUint64(record[16:24], math.MaxUint64)
	path := filepath.Join(t.TempDir(), "metadata")
	if err := os.WriteFile(path, record, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	frames, valid, err := decodeBatchFrames(file, Frame{
		Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize,
		InsertLength: batchRecordSize, PayloadOffset: 0,
	})
	if err != nil || valid || frames != nil {
		t.Fatalf("decodeBatchFrames = (%+v, %v, %v)", frames, valid, err)
	}
}
