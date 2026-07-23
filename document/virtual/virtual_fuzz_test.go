package virtual

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"unicode/utf8"
)

func FuzzLogicalPagesPreserveUTF8(f *testing.F) {
	f.Add([]byte("alpha\n界\nomega"), uint8(4), uint8(16))
	f.Add([]byte(""), uint8(1), uint8(1))
	f.Add([]byte("🙂🙂🙂🙂"), uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, targetSeed, maximumSeed uint8) {
		if len(data) > 4096 || !utf8.Valid(data) {
			return
		}
		target := int64(4 + int(targetSeed)%64)
		maximum := target + int64(int(maximumSeed)%128)
		pager, err := Build(context.Background(), newCoreTestSource(data), 7, Options{
			TargetPageBytes: target, MaximumPageBytes: maximum,
			Window: Budget{Bytes: int64(len(data)) + maximum + 1, Pages: 8192, Fragments: 8192, Measure: 1 << 20},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		var rebuilt []byte
		var cursor int64
		for index, meta := range pager.logical {
			if meta.start != cursor || meta.end < meta.start || meta.end-meta.start > maximum {
				t.Fatalf("page %d violates partition: %+v", index, meta)
			}
			page, err := pager.ReadPage(context.Background(), PageKey{
				Revision: 7, Generation: 0, Index: index, Start: meta.start, End: meta.end,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(page.Content) {
				t.Fatalf("page %d is not UTF-8", index)
			}
			rebuilt = append(rebuilt, page.Content...)
			cursor = meta.end
		}
		if cursor != int64(len(data)) || !bytes.Equal(rebuilt, data) {
			t.Fatalf("partition did not reconstruct source")
		}
	})
}

func FuzzFragmentWindowsRespectRanges(f *testing.F) {
	f.Add([]byte("a界b🙂c"), []byte{1, 2, 3, 4}, uint8(3))
	f.Add([]byte("abcdef"), []byte{0, 1, 0, 1, 0, 1}, uint8(0))
	f.Fuzz(func(t *testing.T, data, selectors []byte, offsetSeed uint8) {
		if len(data) > 2048 || !utf8.Valid(data) {
			return
		}
		boundaries := []int64{0}
		for offset := 0; offset < len(data); {
			_, size := utf8.DecodeRune(data[offset:])
			offset += size
			boundaries = append(boundaries, int64(offset))
		}
		fragments := make([]Fragment, 0)
		for index := 0; index+1 < len(boundaries) && index < len(selectors); index++ {
			if selectors[index]&1 == 0 {
				continue
			}
			fragments = append(fragments, Fragment{
				ID: string(rune(index + 1)), Start: boundaries[index], End: boundaries[index+1],
				Measure: Measure(selectors[index] >> 1),
			})
		}
		pager, err := Build(context.Background(), newCoreTestSource(data), 11, Options{
			TargetPageBytes: 4, MaximumPageBytes: 16,
			Window: Budget{Bytes: int64(len(data)) + 16, Pages: 4096, Fragments: 4096, Measure: 1 << 20},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		stats, err := pager.Publish(context.Background(), Publication{
			Revision: 11, IndexedThrough: int64(len(data)), Complete: true, Fragments: fragments,
		})
		if err != nil {
			t.Fatal(err)
		}
		var offset int64
		if len(data) != 0 {
			offset = int64(offsetSeed) % (int64(len(data)) + 1)
		}
		window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
			Revision: 11, Generation: stats.Generation, Offset: offset,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range window.Pages {
			if page.Key.Start < 0 || page.Key.End > int64(len(data)) || !utf8.Valid(page.Content) {
				t.Fatalf("invalid Page: %+v", page)
			}
		}
		for _, fragment := range fragments {
			result, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
				Revision: 11, Generation: stats.Generation, ID: fragment.ID,
			})
			if err != nil || len(result.Pages) == 0 {
				t.Fatalf("fragment %q is not addressable: %v", fragment.ID, err)
			}
		}
	})
}

func FuzzPagerGenerationStateMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 3, 0, 2})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 1024 {
			return
		}
		pager, err := Build(context.Background(), newCoreTestSource([]byte("abcdefgh")), 5, Options{
			TargetPageBytes: 4, MaximumPageBytes: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		var generation uint64
		for _, operation := range operations {
			switch operation % 4 {
			case 0:
				stats, err := pager.Publish(context.Background(), Publication{
					Revision: 5, BaseGeneration: generation, IndexedThrough: 8,
					Fragments: []Fragment{{ID: "middle", Start: 2, End: 6, Measure: 1}},
				})
				if err != nil {
					t.Fatal(err)
				}
				generation = stats.Generation
			case 1:
				if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
					Revision: 5, Generation: generation, Offset: int64(operation) % 9,
				}); err != nil {
					t.Fatal(err)
				}
			case 2:
				_, err := pager.Publish(context.Background(), Publication{
					Revision: 5, BaseGeneration: generation + 1, IndexedThrough: 8,
				})
				if !errors.Is(err, ErrStaleGeneration) {
					t.Fatalf("stale publication = %v", err)
				}
			case 3:
				stats := pager.Stats()
				if stats.Generation != generation || stats.Revision != 5 {
					t.Fatalf("Stats = %+v", stats)
				}
			}
		}
	})
}
