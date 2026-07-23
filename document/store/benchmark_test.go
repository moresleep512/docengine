package store

import (
	"math/rand/v2"
	"testing"
)

type benchmarkReaderAt struct{}

func (benchmarkReaderAt) ReadAt(buffer []byte, _ int64) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func BenchmarkSequentialAppend(b *testing.B) {
	for _, benchmark := range []struct {
		name    string
		options Options
	}{
		{name: "automatic", options: Options{}},
		{name: "manual", options: Options{DisableAutoCompact: true}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			tree, err := NewWithOptions(nil, 0, benchmark.options)
			if err != nil {
				b.Fatal(err)
			}
			tree.SetSource(SourceJournal, benchmarkReaderAt{})
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
					Source: SourceJournal, Offset: int64(index), Length: 1,
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRandomSingleByteReplace(b *testing.B) {
	const length = 1 << 20
	tree, err := New(benchmarkReaderAt{}, length)
	if err != nil {
		b.Fatal(err)
	}
	tree.SetSource(SourceJournal, benchmarkReaderAt{})
	rng := rand.New(rand.NewPCG(41, 43))
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		position := rng.Int64N(length)
		if _, _, err := tree.ReplacePiece(position, 1, Piece{
			Source: SourceJournal, Offset: int64(index), Length: 1,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshotReadAtFragmented(b *testing.B) {
	const pieces = 16 << 10
	tree, err := NewWithOptions(nil, 0, Options{DisableAutoCompact: true})
	if err != nil {
		b.Fatal(err)
	}
	tree.SetSource(SourceJournal, benchmarkReaderAt{})
	for index := int64(0); index < pieces; index++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: index * 2, Length: 1,
		}); err != nil {
			b.Fatal(err)
		}
	}
	snapshot := tree.Snapshot()
	buffer := make([]byte, 4096)
	rng := rand.New(rand.NewPCG(47, 53))
	b.SetBytes(int64(len(buffer)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		offset := rng.Int64N(snapshot.Len() - int64(len(buffer)) + 1)
		if n, err := snapshot.ReadAt(buffer, offset); n != len(buffer) || err != nil {
			b.Fatalf("ReadAt = (%d, %v)", n, err)
		}
	}
}

func BenchmarkCompactFragmented(b *testing.B) {
	const pieces = 16 << 10
	tree, err := NewWithOptions(nil, 0, Options{DisableAutoCompact: true})
	if err != nil {
		b.Fatal(err)
	}
	tree.SetSource(SourceJournal, benchmarkReaderAt{})
	for index := int64(0); index < pieces; index++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: index, Length: 1,
		}); err != nil {
			b.Fatal(err)
		}
	}
	fragmented := tree.Snapshot()
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		tree.Restore(fragmented)
		b.StartTimer()
		result := tree.Compact()
		if result.BeforePieces != pieces || result.AfterPieces != 1 {
			b.Fatalf("Compact = %+v", result)
		}
	}
}
