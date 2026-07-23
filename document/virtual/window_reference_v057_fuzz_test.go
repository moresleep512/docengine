package virtual

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func FuzzVirtualWindowsMatchReferenceModel(f *testing.F) {
	f.Add([]byte{12, 0xff, 7, 3, 9, 1, 4, 2, 8, 5, 6})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) == 0 || len(input) > 256 {
			return
		}
		length := 1 + int(input[0])%64
		data := make([]byte, length)
		for index := range data {
			if index%11 == 10 {
				data[index] = '\n'
			} else {
				data[index] = byte('a' + index%26)
			}
		}
		fragments := make([]Fragment, 0, length)
		for index := 0; index < length; index++ {
			selector := fuzzByte(input, index+1)
			if selector&1 == 0 {
				continue
			}
			fragments = append(fragments, Fragment{
				ID: fmt.Sprintf("f%d", index), Start: int64(index), End: int64(index + 1),
				Measure: Measure(1 + selector%4),
			})
		}
		if len(fragments) == 0 {
			fragments = append(fragments, Fragment{ID: "f0", Start: 0, End: 1, Measure: 1})
		}
		maximumBytes := int64(length + 4)
		pager, err := Build(context.Background(), testSource(data), 19, Options{
			TargetPageBytes: 4, MaximumPageBytes: 4,
			MaximumInflightBytes: maximumBytes,
			Window: Budget{
				Bytes: maximumBytes, Pages: length + 1,
				Fragments: length + 1, Measure: Measure(length*4 + 1),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		stats, err := pager.Publish(context.Background(), Publication{
			Revision: 19, IndexedThrough: int64(length), Complete: true, Fragments: fragments,
		})
		if err != nil {
			t.Fatal(err)
		}
		pager.mu.RLock()
		state := pager.state
		pager.mu.RUnlock()

		cursor := 1 + length
		budget := Budget{
			Bytes:     1 + int64(fuzzByte(input, cursor))%maximumBytes,
			Pages:     1 + int(fuzzByte(input, cursor+1))%(length+1),
			Fragments: 1 + int(fuzzByte(input, cursor+2))%(length+1),
			Measure:   1 + Measure(fuzzByte(input, cursor+3))%Measure(length*4+1),
		}
		offset := int64(fuzzByte(input, cursor+4)) % int64(length+1)
		beforeBytes := int64(fuzzByte(input, cursor+5)) % int64(length+2)
		afterBytes := int64(fuzzByte(input, cursor+6)) % int64(length+2)
		byteAnchor := referencePageAtByte(state.pages, offset, int64(length))
		byteFirst := referencePageAtByte(state.pages, offset-min(offset, beforeBytes), int64(length))
		byteLast := byteAnchor
		byteEnd := min(int64(length), offset+afterBytes)
		if byteEnd > offset {
			byteLast = referencePageAtByte(state.pages, byteEnd-1, int64(length))
		}
		want, wantErr := referenceVirtualWindow(state, byteAnchor, byteFirst, byteLast, budget)
		got, gotErr := pager.WindowByByte(context.Background(), ByteWindowRequest{
			Revision: 19, Generation: stats.Generation, Offset: offset,
			Before: beforeBytes, After: afterBytes, Budget: budget,
		})
		assertVirtualWindowModel(t, got, gotErr, want, wantErr, data)

		fragmentIndex := int(fuzzByte(input, cursor+7)) % len(fragments)
		meta := state.fragments[fragmentIndex]
		continuation := int(fuzzByte(input, cursor+8)) % (meta.pageLast - meta.pageFirst + 1)
		beforeFragments := int(fuzzByte(input, cursor+9)) % (len(fragments) + 1)
		afterFragments := int(fuzzByte(input, cursor+10)) % (len(fragments) + 1)
		firstFragment := max(0, fragmentIndex-beforeFragments)
		lastFragment := min(len(fragments)-1, fragmentIndex+afterFragments)
		fragmentAnchor := meta.pageFirst + continuation
		want, wantErr = referenceVirtualWindow(
			state, fragmentAnchor, state.fragments[firstFragment].pageFirst,
			state.fragments[lastFragment].pageLast, budget,
		)
		got, gotErr = pager.WindowByFragment(context.Background(), FragmentWindowRequest{
			Revision: 19, Generation: stats.Generation, ID: meta.ID,
			Continuation: continuation, Before: beforeFragments, After: afterFragments,
			Budget: budget,
		})
		assertVirtualWindowModel(t, got, gotErr, want, wantErr, data)

		measureOffset := Measure(fuzzByte(input, cursor+11)) % (state.totalMeasure + 1)
		affinity := AffinityBefore
		if fuzzByte(input, cursor+12)&1 != 0 {
			affinity = AffinityAfter
		}
		beforeMeasure := Measure(fuzzByte(input, cursor+13)) % (state.totalMeasure + 2)
		afterMeasure := Measure(fuzzByte(input, cursor+14)) % (state.totalMeasure + 2)
		measureFragment := referenceFragmentAtMeasure(state.fragments, measureOffset, affinity)
		measureAnchor := state.fragments[measureFragment].pageFirst
		if state.totalMeasure > 0 && measureOffset == state.totalMeasure {
			measureAnchor = state.fragments[measureFragment].pageLast
		} else if state.totalMeasure > 0 && measureOffset != 0 && affinity == AffinityBefore {
			measureAnchor = state.fragments[measureFragment].pageLast
		}
		firstFragment, lastFragment = measureFragment, measureFragment
		measureStart := measureOffset - min(measureOffset, beforeMeasure)
		if measureStart < measureOffset {
			firstFragment = referenceFragmentAtMeasure(state.fragments, measureStart, AffinityAfter)
		}
		measureEnd := min(state.totalMeasure, measureOffset+afterMeasure)
		if measureEnd > measureOffset {
			lastFragment = referenceFragmentAtMeasure(state.fragments, measureEnd, AffinityBefore)
		}
		want, wantErr = referenceVirtualWindow(
			state, measureAnchor, state.fragments[firstFragment].pageFirst,
			state.fragments[lastFragment].pageLast, budget,
		)
		got, gotErr = pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
			Revision: 19, Generation: stats.Generation, Offset: measureOffset,
			Affinity: affinity, Before: beforeMeasure, After: afterMeasure, Budget: budget,
		})
		assertVirtualWindowModel(t, got, gotErr, want, wantErr, data)
	})
}

type referenceWindow struct {
	first, last                     int
	bytes                           int64
	pages, fragments                int
	measure                         Measure
	truncatedBefore, truncatedAfter bool
}

func referenceVirtualWindow(state *pagerState, anchor, desiredFirst, desiredLast int, budget Budget) (referenceWindow, error) {
	result := referenceWindow{first: anchor, last: anchor}
	seen := make(map[int]bool)
	add := func(index int) bool {
		page := state.pages[index]
		length := page.end - page.start
		fragments, measure := result.fragments, result.measure
		if page.fragment >= 0 && !seen[page.fragment] {
			fragments++
			measure += state.fragments[page.fragment].Measure
		}
		if result.pages+1 > budget.Pages || result.bytes+length > budget.Bytes ||
			fragments > budget.Fragments || measure > budget.Measure {
			return false
		}
		result.pages++
		result.bytes += length
		if page.fragment >= 0 && !seen[page.fragment] {
			seen[page.fragment] = true
			result.fragments = fragments
			result.measure = measure
		}
		return true
	}
	if !add(anchor) {
		return referenceWindow{}, ErrBudgetExceeded
	}
	for index := anchor - 1; index >= desiredFirst; index-- {
		if !add(index) {
			result.truncatedBefore = true
			break
		}
		result.first = index
	}
	for index := anchor + 1; index <= desiredLast; index++ {
		if !add(index) {
			result.truncatedAfter = true
			break
		}
		result.last = index
	}
	return result, nil
}

func referencePageAtByte(pages []pageMeta, offset, length int64) int {
	if offset == length {
		return len(pages) - 1
	}
	for index, page := range pages {
		if page.end > offset {
			return index
		}
	}
	panic("referencePageAtByte: missing page")
}

func referenceFragmentAtMeasure(fragments []fragmentMeta, offset Measure, affinity Affinity) int {
	if affinity == AffinityBefore && offset != 0 {
		for index := len(fragments) - 1; index >= 0; index-- {
			if fragments[index].measureStart < offset {
				return index
			}
		}
		return 0
	}
	for index, fragment := range fragments {
		if fragment.measureStart+fragment.Measure > offset {
			return index
		}
	}
	return len(fragments) - 1
}

func assertVirtualWindowModel(t testing.TB, got Window, gotErr error, want referenceWindow, wantErr error, data []byte) {
	t.Helper()
	if wantErr != nil {
		if !errors.Is(gotErr, wantErr) {
			t.Fatalf("Window error = %v, want %v", gotErr, wantErr)
		}
		return
	}
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if len(got.Pages) != want.pages || got.Bytes != want.bytes ||
		got.Fragments != want.fragments || got.Measure != want.measure ||
		got.TruncatedBefore != want.truncatedBefore || got.TruncatedAfter != want.truncatedAfter {
		t.Fatalf("Window = %+v, want %+v", got, want)
	}
	var content []byte
	for offset, page := range got.Pages {
		wantIndex := want.first + offset
		if page.Key.Index != wantIndex {
			t.Fatalf("Page %d index = %d, want %d", offset, page.Key.Index, wantIndex)
		}
		content = append(content, page.Content...)
	}
	first := got.Pages[0].Key.Start
	last := got.Pages[len(got.Pages)-1].Key.End
	if string(content) != string(data[first:last]) {
		t.Fatalf("Window content = %q, want %q", content, data[first:last])
	}
}

func fuzzByte(input []byte, index int) byte {
	return input[index%len(input)]
}
