package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalReplayAndTailRepair(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(basePath, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(basePath)
	fingerprint := FingerprintFor(basePath, info)
	journalPath := filepath.Join(dir, "doc.journal")
	journal, _, err := Open(journalPath, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	offset, err := journal.AppendReplace(1, 1, 2, []byte("XYZ"))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.AppendRoot(2, 0); err != nil {
		t.Fatal(err)
	}
	if err := journal.Sync(); err != nil {
		t.Fatal(err)
	}
	replay, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Frames) != 2 || replay.Frames[0].PayloadOffset != offset || replay.Frames[1].TargetRevision != 0 {
		t.Fatalf("unexpected replay: %+v", replay)
	}
	if _, err := journal.file.WriteAt([]byte("broken"), replay.ValidBytes); err != nil {
		t.Fatal(err)
	}
	replay, err = journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Truncated || len(replay.Frames) != 2 {
		t.Fatalf("expected truncated tail: %+v", replay)
	}
	if err := journal.RepairTail(replay.ValidBytes); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
}

func TestJournalRejectsStaleBase(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(basePath, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(basePath)
	fingerprint := FingerprintFor(basePath, info)
	journalPath := filepath.Join(dir, "doc.journal")
	journal, _, err := Open(journalPath, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	fingerprint.BaseSize++
	if _, _, err := Open(journalPath, fingerprint); err != ErrStaleJournal {
		t.Fatalf("got %v, want ErrStaleJournal", err)
	}
}
