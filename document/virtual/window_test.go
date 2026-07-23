package virtual

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func publishedPager(t *testing.T) *Pager {
	t.Helper()
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumFragments: 8,
		Window: Budget{Bytes: 12, Pages: 3, Fragments: 3, Measure: 30},
	})
	if _, err := pager.Publish(context.Background(), completePublication(0)); err != nil {
		t.Fatal(err)
	}
	return pager
}

func pageIDs(window Window) []string {
	ids := make([]string, len(window.Pages))
	for index := range window.Pages {
		ids[index] = window.Pages[index].FragmentID
	}
	return ids
}

func TestWindowByByteOverscanAndBudgets(t *testing.T) {
	pager := publishedPager(t)
	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 4, Before: 4, After: math.MaxInt64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Pages) != 3 || window.Bytes != 12 || window.Fragments != 3 ||
		window.Measure != 30 || window.TruncatedBefore || window.TruncatedAfter {
		t.Fatalf("Window = %+v", window)
	}
	if got := pageIDs(window); got[0] != "first" || got[1] != "zero" || got[2] != "last" {
		t.Fatalf("page IDs = %v", got)
	}

	window, err = pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 4, Before: 4, After: 8,
		Budget: Budget{Bytes: 8, Pages: 2, Fragments: 2, Measure: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pageIDs(window); len(got) != 2 || got[0] != "first" || got[1] != "zero" {
		t.Fatalf("budgeted page IDs = %v", got)
	}
	if window.TruncatedBefore || !window.TruncatedAfter ||
		window.Bytes != 8 || window.Fragments != 2 || window.Measure != 10 {
		t.Fatalf("budgeted Window = %+v", window)
	}

	for _, test := range []struct {
		name   string
		budget Budget
	}{
		{name: "bytes", budget: Budget{Bytes: 3, Pages: 1, Fragments: 1, Measure: 10}},
		{name: "measure", budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 9}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
				Revision: 41, Generation: 1, Offset: 0, Budget: test.budget,
			}); !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("WindowByByte = %v", err)
			}
		})
	}
}

func TestWindowByByteBoundariesAndInvalidRequests(t *testing.T) {
	pager := publishedPager(t)
	for _, test := range []struct {
		offset int64
		id     string
	}{
		{offset: 0, id: "first"},
		{offset: 4, id: "zero"},
		{offset: 8, id: "last"},
		{offset: 12, id: "last"},
	} {
		window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
			Revision: 41, Generation: 1, Offset: test.offset,
			Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 30},
		})
		if err != nil {
			t.Fatalf("offset %d: %v", test.offset, err)
		}
		if len(window.Pages) != 1 || window.Pages[0].FragmentID != test.id {
			t.Fatalf("offset %d: %+v", test.offset, window)
		}
	}

	for _, request := range []ByteWindowRequest{
		{Revision: 41, Generation: 1, Offset: -1},
		{Revision: 41, Generation: 1, Offset: 13},
		{Revision: 41, Generation: 1, Offset: 0, Before: -1},
		{Revision: 41, Generation: 1, Offset: 0, After: -1},
		{Revision: 40, Generation: 1, Offset: 0},
		{Revision: 41, Generation: 0, Offset: 0},
		{
			Revision: 41, Generation: 1, Offset: 0,
			Budget: Budget{Bytes: 13, Pages: 3, Fragments: 3, Measure: 30},
		},
	} {
		_, err := pager.WindowByByte(context.Background(), request)
		if err == nil {
			t.Fatalf("WindowByByte(%+v) unexpectedly succeeded", request)
		}
	}
}

func TestWindowByFragmentOverscanAndLookup(t *testing.T) {
	pager := publishedPager(t)
	window, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "zero", Before: 1, After: 1,
		Budget: Budget{Bytes: 8, Pages: 2, Fragments: 2, Measure: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pageIDs(window); len(got) != 2 || got[0] != "first" || got[1] != "zero" {
		t.Fatalf("page IDs = %v", got)
	}
	if window.TruncatedBefore || !window.TruncatedAfter {
		t.Fatalf("truncation = (%v, %v)", window.TruncatedBefore, window.TruncatedAfter)
	}

	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "missing",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing fragment = %v", err)
	}
	for _, request := range []FragmentWindowRequest{
		{Revision: 41, Generation: 1},
		{Revision: 41, Generation: 1, ID: "first", Before: -1},
		{Revision: 41, Generation: 1, ID: "first", After: -1},
	} {
		if _, err := pager.WindowByFragment(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("WindowByFragment(%+v) = %v", request, err)
		}
	}
}

func TestWindowByMeasureAffinityOverscanAndBudgets(t *testing.T) {
	pager := publishedPager(t)
	for _, test := range []struct {
		name     string
		offset   Measure
		affinity Affinity
		id       string
	}{
		{name: "start before", offset: 0, affinity: AffinityBefore, id: "first"},
		{name: "start after", offset: 0, affinity: AffinityAfter, id: "first"},
		{name: "boundary before", offset: 10, affinity: AffinityBefore, id: "first"},
		{name: "boundary after skips zero", offset: 10, affinity: AffinityAfter, id: "last"},
		{name: "end before", offset: 30, affinity: AffinityBefore, id: "last"},
		{name: "end after", offset: 30, affinity: AffinityAfter, id: "last"},
	} {
		t.Run(test.name, func(t *testing.T) {
			window, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
				Revision: 41, Generation: 1, Offset: test.offset, Affinity: test.affinity,
				Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 30},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(window.Pages) != 1 || window.Pages[0].FragmentID != test.id {
				t.Fatalf("Window = %+v", window)
			}
		})
	}

	window, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 10, Affinity: AffinityAfter,
		Before: 10, After: math.MaxInt64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Pages) != 3 || window.Measure != 30 {
		t.Fatalf("overscan Window = %+v", window)
	}

	if _, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 10, Affinity: AffinityAfter,
		Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 19},
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("measure budget = %v", err)
	}
	for _, request := range []MeasureWindowRequest{
		{Revision: 41, Generation: 1, Offset: -1, Affinity: AffinityAfter},
		{Revision: 41, Generation: 1, Offset: 31, Affinity: AffinityAfter},
		{Revision: 41, Generation: 1, Offset: 0, Affinity: 0},
		{Revision: 41, Generation: 1, Offset: 0, Affinity: AffinityAfter, Before: -1},
		{Revision: 41, Generation: 1, Offset: 0, Affinity: AffinityAfter, After: -1},
	} {
		if _, err := pager.WindowByMeasure(context.Background(), request); err == nil {
			t.Fatalf("WindowByMeasure(%+v) unexpectedly succeeded", request)
		}
	}
}

func TestGiantUTF8FragmentUsesContinuationPages(t *testing.T) {
	body := "界界界界"
	pager := newPublicationPager(t, body, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		Window: Budget{Bytes: 12, Pages: 4, Fragments: 1, Measure: 7},
	})
	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 41, IndexedThrough: int64(len(body)), Complete: true,
		Fragments: []Fragment{{ID: "giant", Start: 0, End: int64(len(body)), Measure: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pages != 4 || stats.Fragments != 1 {
		t.Fatalf("Stats = %+v", stats)
	}

	window, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "giant",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Pages) != 4 || window.Bytes != 12 || window.Fragments != 1 || window.Measure != 7 {
		t.Fatalf("Window = %+v", window)
	}
	for index, page := range window.Pages {
		if !utf8.Valid(page.Content) || string(page.Content) != "界" ||
			page.ContinuationIndex != index || page.ContinuationCount != 4 ||
			page.MeasureStart != 0 || page.MeasureEnd != 7 {
			t.Fatalf("page %d = %+v", index, page)
		}
		if page.ContinuesFromPrevious != (index > 0) ||
			page.ContinuesToNext != (index < len(window.Pages)-1) {
			t.Fatalf("page %d continuation flags = (%v, %v)", index, page.ContinuesFromPrevious, page.ContinuesToNext)
		}
	}

	pageLimited, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "giant",
		Budget: Budget{Bytes: 12, Pages: 3, Fragments: 1, Measure: 7},
	})
	if err != nil || !pageLimited.TruncatedAfter || len(pageLimited.Pages) != 3 {
		t.Fatalf("fragment page truncation = %+v, %v", pageLimited, err)
	}
	byteLimited, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "giant",
		Budget: Budget{Bytes: 11, Pages: 4, Fragments: 1, Measure: 7},
	})
	if err != nil || !byteLimited.TruncatedAfter || len(byteLimited.Pages) != 3 {
		t.Fatalf("fragment byte truncation = %+v, %v", byteLimited, err)
	}

	middle, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 4,
		Budget: Budget{Bytes: 3, Pages: 1, Fragments: 1, Measure: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(middle.Pages) != 1 || middle.Pages[0].ContinuationIndex != 1 ||
		middle.Fragments != 1 || middle.Measure != 7 {
		t.Fatalf("middle continuation = %+v", middle)
	}
}

func TestIncompletePublicationRetainsLogicalFallback(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if _, err := pager.Publish(context.Background(), Publication{
		Revision: 41, IndexedThrough: 4,
		Fragments: []Fragment{{ID: "known", Start: 0, End: 4, Measure: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if window.Complete || window.IndexedThrough != 4 || len(window.Pages) != 1 ||
		window.Pages[0].Indexed || window.Pages[0].FragmentIndex != -1 ||
		string(window.Pages[0].Content) != "ijkl" {
		t.Fatalf("fallback Window = %+v", window)
	}
	known, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 2, Affinity: AffinityAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(known.Pages) != 1 || known.Pages[0].FragmentID != "known" {
		t.Fatalf("known measure Window = %+v", known)
	}
}

func TestReadPageRejectsChangedOrForgedKeys(t *testing.T) {
	pager := publishedPager(t)
	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 0,
		Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := window.Pages[0].Key
	page, err := pager.ReadPage(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if string(page.Content) != "abcd" {
		t.Fatalf("Content = %q", page.Content)
	}
	forged := key
	forged.End++
	if _, err := pager.ReadPage(context.Background(), forged); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("forged key = %v", err)
	}
	forged = key
	forged.Index = -1
	if _, err := pager.ReadPage(context.Background(), forged); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative index = %v", err)
	}

	if _, err := pager.Publish(context.Background(), completePublication(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := pager.ReadPage(context.Background(), key); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old key = %v", err)
	}
}

type blockingReadSource struct {
	data    []byte
	enabled atomic.Bool
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (source *blockingReadSource) Len() int64 {
	return int64(len(source.data))
}

func (source *blockingReadSource) ReadAt(buffer []byte, offset int64) (int, error) {
	if source.enabled.CompareAndSwap(true, false) {
		source.once.Do(func() { close(source.started) })
		<-source.release
	}
	if offset >= int64(len(source.data)) {
		return 0, io.EOF
	}
	n := copy(buffer, source.data[offset:])
	if n != len(buffer) {
		return n, io.EOF
	}
	return n, nil
}

func TestWindowRejectsGenerationPublishedDuringRead(t *testing.T) {
	source := &blockingReadSource{
		data: []byte("abcdefghijkl"), started: make(chan struct{}), release: make(chan struct{}),
	}
	pager, err := Build(context.Background(), source, 41, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true, MaximumTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	source.enabled.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
			Revision: 41, Generation: 0, Offset: 0,
		})
		done <- err
	}()
	waitForSignal(t, source.started, "blocked page read")
	if _, err := pager.Publish(context.Background(), completePublication(0)); err != nil {
		t.Fatalf("Publish = %v", err)
	}
	close(source.release)
	if err := waitForError(t, done, "in-flight WindowByByte"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("in-flight WindowByByte = %v", err)
	}
}
