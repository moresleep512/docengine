package save

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkAtomicChecked4MiB(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "document")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	content := bytes.Repeat([]byte("0123456789abcdef"), (4<<20)/16)
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := AtomicChecked(path, 0o600, nil, func(writer io.Writer) (int64, error) {
			n, writeErr := writer.Write(content)
			return int64(n), writeErr
		}, nil); err != nil {
			b.Fatal(err)
		}
	}
}
