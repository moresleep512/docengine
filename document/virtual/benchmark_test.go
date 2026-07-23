package virtual

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func BenchmarkBuildLogicalPages(b *testing.B) {
	source := newCoreTestSource([]byte(strings.Repeat("0123456789abcdef\n", 1<<16)))
	b.ReportAllocs()
	b.SetBytes(int64(len(source.data)))
	for b.Loop() {
		pager, err := Build(context.Background(), source, 1, Options{})
		if err != nil {
			b.Fatal(err)
		}
		if err := pager.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPublishFragments(b *testing.B) {
	const fragments = 1 << 14
	source := newCoreTestSource([]byte(strings.Repeat("x", fragments)))
	items := make([]Fragment, fragments)
	for index := range items {
		items[index] = Fragment{
			ID: strconv.Itoa(index), Start: int64(index), End: int64(index + 1), Measure: 1,
		}
	}
	pager, err := Build(context.Background(), source, 1, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer pager.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		generation := pager.Stats().Generation
		if _, err := pager.Publish(context.Background(), Publication{
			Revision: 1, BaseGeneration: generation, IndexedThrough: fragments,
			Complete: true, Fragments: items,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWindowByByteCached(b *testing.B) {
	source := newCoreTestSource([]byte(strings.Repeat("0123456789abcdef", 1<<12)))
	pager, err := Build(context.Background(), source, 1, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer pager.Close()
	request := ByteWindowRequest{Revision: 1, Offset: int64(len(source.data) / 2)}
	if _, err := pager.WindowByByte(context.Background(), request); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := pager.WindowByByte(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}
