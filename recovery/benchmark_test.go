package recovery

import (
	"path/filepath"
	"testing"
)

func BenchmarkJournalAppendBatch(b *testing.B) {
	journal, _, err := Open(filepath.Join(b.TempDir(), "journal"), Fingerprint{})
	if err != nil {
		b.Fatal(err)
	}
	defer journal.Close()
	inserted := make([]byte, 256)
	operations := []ReplaceOperation{{Inserted: inserted}}
	b.ReportAllocs()
	b.SetBytes(int64(len(inserted)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		revision := uint64(index) + 1
		if _, err := journal.AppendBatch(revision, revision, operations); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJournalReplay4096Batches(b *testing.B) {
	journal, _, err := Open(filepath.Join(b.TempDir(), "journal"), Fingerprint{})
	if err != nil {
		b.Fatal(err)
	}
	defer journal.Close()
	inserted := make([]byte, 32)
	for revision := uint64(1); revision <= 4_096; revision++ {
		if _, err := journal.AppendBatch(revision, revision, []ReplaceOperation{{Inserted: inserted}}); err != nil {
			b.Fatal(err)
		}
	}
	if err := journal.Sync(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(4_096 * int64(len(inserted)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		replay, err := journal.Replay()
		if err != nil || len(replay.Batches) != 4_096 {
			b.Fatalf("Replay = (%+v, %v)", replay, err)
		}
	}
}
