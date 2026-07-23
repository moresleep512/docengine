package document

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkSessionSave4MiB(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "document")
	content := bytes.Repeat([]byte("0123456789abcdef"), (4<<20)/16)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		b.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir:         filepath.Join(dir, "recovery"),
		SessionDir:          filepath.Join(dir, "session"),
		JournalSyncInterval: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer session.Close()
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		metadata := session.Metadata()
		if _, err := session.ApplyBatch(context.Background(), metadata.Revision, []ReplaceOperation{{
			Start: metadata.ByteLength, Insert: "x",
		}}); err != nil {
			b.Fatal(err)
		}
		if _, err := session.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
