package recovery

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalV2BatchRoundTripRepairAndReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "document.docengine-journal-v2")
	fingerprint := testFingerprint(filepath.Join(dir, "document"), []byte("base"))
	journal, replay, err := Open(path, fingerprint)
	if err != nil || replay.ValidBytes != fileHeaderSize || replay.Truncated || len(replay.Batches) != 0 {
		t.Fatalf("Open = (%v, %+v)", err, replay)
	}
	if journal.Path() != path {
		t.Fatalf("Path = %q", journal.Path())
	}
	first, err := journal.AppendBatch(10, 7, []ReplaceOperation{
		{Start: 1, DeleteLength: 2, Inserted: []byte("XYZ")},
		{Start: 4, Inserted: []byte("tail")},
	})
	if err != nil || first.BatchOffset != fileHeaderSize || len(first.PayloadOffsets) != 2 {
		t.Fatalf("AppendBatch = (%+v, %v)", first, err)
	}
	if _, err := journal.AppendBatch(12, 8, []ReplaceOperation{{Start: 0}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Sync(); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 3)
	if n, err := journal.ReadAt(payload, first.PayloadOffsets[0]); n != 3 || err != nil || string(payload) != "XYZ" {
		t.Fatalf("ReadAt = (%d, %v, %q)", n, err, payload)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	journal, replay, err = Open(path, fingerprint)
	if err != nil || replay.Truncated || len(replay.Batches) != 2 {
		t.Fatalf("reopen = (%+v, %v)", replay, err)
	}
	if got := replay.Batches[0]; got.FirstRevision != 10 || got.Group != 7 || len(got.Operations) != 2 || got.Operations[0].PayloadOffset != first.PayloadOffsets[0] {
		t.Fatalf("first batch = %+v", got)
	}
	validBytes := replay.ValidBytes
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("partial"))
	_ = file.Close()
	journal, replay, err = Open(path, fingerprint)
	if err != nil || !replay.Truncated || replay.ValidBytes != validBytes || len(replay.Batches) != 2 {
		t.Fatalf("truncated reopen = (%+v, %v)", replay, err)
	}
	if err := journal.RepairTail(replay.ValidBytes); err != nil {
		t.Fatal(err)
	}
	newFingerprint := testFingerprint(filepath.Join(dir, "document"), []byte("new base"))
	if err := journal.Reset(newFingerprint); err != nil {
		t.Fatal(err)
	}
	stored, err := ReadFingerprint(path)
	if err != nil || stored != newFingerprint {
		t.Fatalf("ReadFingerprint = (%+v, %v)", stored, err)
	}
	replay, err = journal.Replay()
	if err != nil || replay.ValidBytes != fileHeaderSize || replay.Truncated || len(replay.Batches) != 0 {
		t.Fatalf("Replay after reset = (%+v, %v)", replay, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalClosedAndInvalidLifecycleErrors(t *testing.T) {
	journal, _ := openTestJournal(t)
	if err := journal.RepairTail(fileHeaderSize - 1); err == nil {
		t.Fatal("RepairTail accepted header prefix")
	}
	if err := journal.Reset(Fingerprint{BaseSize: -1}); !errors.Is(err, ErrStaleJournal) {
		t.Fatalf("Reset negative fingerprint = %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadAt = %v", err)
	}
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("AppendBatch = %v", err)
	}
	if _, err := journal.Replay(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Replay = %v", err)
	}
	if err := journal.RepairTail(fileHeaderSize); !errors.Is(err, ErrClosed) {
		t.Fatalf("RepairTail = %v", err)
	}
	if err := journal.Sync(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Sync = %v", err)
	}
	if err := journal.Reset(Fingerprint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reset = %v", err)
	}
}

func TestOpenRejectsStaleJournalAndInvalidFingerprint(t *testing.T) {
	journal, path := openTestJournal(t)
	fingerprint, err := ReadFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	fingerprint.ContentHash[0] ^= 1
	if _, _, err := Open(path, fingerprint); !errors.Is(err, ErrStaleJournal) {
		t.Fatalf("stale Open = %v", err)
	}
	if _, _, err := Open(filepath.Join(t.TempDir(), "negative"), Fingerprint{BaseSize: -1}); !errors.Is(err, ErrStaleJournal) {
		t.Fatalf("negative Open = %v", err)
	}
}

func TestJournalPayloadCorruptionNeverExposesBatch(t *testing.T) {
	journal, path := openTestJournal(t)
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: []byte("payload")}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content[len(content)-1] ^= 1
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := readFileHeader(bytes.NewReader(content))
	opened, replay, err := Open(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Batches) != 0 {
		t.Fatalf("corrupt replay = %+v", replay)
	}
}

func testFingerprint(path string, body []byte) Fingerprint {
	return FingerprintFor(path, int64(len(body)), sha256.Sum256(body))
}

func openTestJournal(t testing.TB) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.docengine-journal-v2")
	journal, _, err := Open(path, testFingerprint(filepath.Join(filepath.Dir(path), "base"), []byte("base")))
	if err != nil {
		t.Fatal(err)
	}
	return journal, path
}

func readInserted(t testing.TB, journal *Journal, operation Operation) []byte {
	t.Helper()
	value := make([]byte, operation.InsertLength)
	n, err := journal.ReadAt(value, operation.PayloadOffset)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(value)) {
		t.Fatal(err)
	}
	return value[:n]
}
