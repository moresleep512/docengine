package coordinate

import (
	"bytes"
	"context"
	"io"
	"testing"
)

type benchmarkSource []byte

func (source benchmarkSource) Len() int64 { return int64(len(source)) }

func (source benchmarkSource) ReadAt(buffer []byte, offset int64) (int, error) {
	return bytes.NewReader(source).ReadAt(buffer, offset)
}

func BenchmarkIndexQueryWindow(b *testing.B) {
	body := bytes.Repeat([]byte("abcdefghijklmno\n"), 1<<16)
	for _, test := range []struct {
		name    string
		options Options
	}{
		{name: "cached", options: Options{}},
		{name: "uncached", options: Options{DisableCache: true}},
	} {
		b.Run(test.name, func(b *testing.B) {
			index, err := Build(context.Background(), benchmarkSource(body), 1, test.options)
			if err != nil {
				b.Fatal(err)
			}
			defer index.Close()
			offset := int64(len(body)/2) + DefaultCheckpointBytes/2
			if _, err := index.ByteToPosition(context.Background(), offset); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(DefaultCheckpointBytes)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := index.ByteToPosition(context.Background(), offset); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkIndexRebuildMiddleEdit(b *testing.B) {
	before := bytes.Repeat([]byte("abcdefghijklmno\n"), 1<<18)
	previous, err := Build(context.Background(), benchmarkSource(before), 1, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer previous.Close()
	start := int64(len(before) / 2)
	after := append([]byte(nil), before...)
	after[start] = 'X'
	change, err := NewChangeMap(1, 2, int64(len(before)), []Edit{{Start: start, OldLength: 1, NewLength: 1}})
	if err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		build func() (*Index, error)
	}{
		{name: "full", build: func() (*Index, error) {
			return Build(context.Background(), benchmarkSource(after), 2, Options{})
		}},
		{name: "prefix-and-suffix", build: func() (*Index, error) {
			return Rebuild(context.Background(), benchmarkSource(after), previous, change)
		}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(after)))
			for iteration := 0; iteration < b.N; iteration++ {
				index, buildErr := test.build()
				if buildErr != nil {
					b.Fatal(buildErr)
				}
				if closeErr := index.Close(); closeErr != nil {
					b.Fatal(closeErr)
				}
			}
		})
	}
}

func BenchmarkChangeMapComposeHistory(b *testing.B) {
	const count = 256
	chain := make([]ChangeMap, count)
	length := int64(1)
	revision := uint64(0)
	for index := range chain {
		change, err := NewChangeMap(revision, revision+1, length, []Edit{{Start: length, NewLength: 1}})
		if err != nil {
			b.Fatal(err)
		}
		chain[index] = change
		length++
		revision++
	}
	identity, _ := Identity(0, 1)
	for _, test := range []struct {
		name    string
		compose func() (ChangeMap, error)
	}{
		{name: "linear", compose: func() (ChangeMap, error) {
			return identity.ComposeAll(chain...)
		}},
		{name: "pairwise", compose: func() (ChangeMap, error) {
			result := identity
			var err error
			for _, change := range chain {
				result, err = result.Compose(change)
				if err != nil {
					return ChangeMap{}, err
				}
			}
			return result, nil
		}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				result, composeErr := test.compose()
				if composeErr != nil || result.Len() != count {
					b.Fatalf("Compose = (%d,%v)", result.Len(), composeErr)
				}
			}
		})
	}
}

var _ io.ReaderAt = benchmarkSource(nil)
