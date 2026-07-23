package virtual

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicationAllowsGapsAndIncompleteFullWatermark(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		MaximumFragments: 4, MaximumKeyBytes: 32,
		Window: Budget{Bytes: 12, Pages: 8, Fragments: 4, Measure: 10},
	})
	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 41, IndexedThrough: 12, Complete: false,
		Fragments: []Fragment{
			{ID: "left", Start: 1, End: 3, Measure: 2, DataKey: "L"},
			{ID: "right", Start: 8, End: 10, Measure: 3, DataKey: "R"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Complete || stats.IndexedThrough != 12 || stats.Fragments != 2 ||
		stats.KeyBytes != int64(len("leftLrightR")) {
		t.Fatalf("Stats = %+v", stats)
	}

	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 0, After: math.MaxInt64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if window.Complete || window.IndexedThrough != 12 || len(window.Pages) != stats.Pages {
		t.Fatalf("Window = %+v", window)
	}
	var sawLeadingGap, sawMiddleGap, sawTrailingGap bool
	for _, page := range window.Pages {
		if !page.Indexed {
			t.Fatalf("page below full watermark is not indexed: %+v", page)
		}
		if page.FragmentIndex >= 0 {
			continue
		}
		switch {
		case page.Key.End <= 1:
			sawLeadingGap = true
		case page.Key.Start >= 3 && page.Key.End <= 8:
			sawMiddleGap = true
		case page.Key.Start >= 10:
			sawTrailingGap = true
		}
	}
	if !sawLeadingGap || !sawMiddleGap || !sawTrailingGap {
		t.Fatalf("gap fallback pages missing: leading=%v middle=%v trailing=%v pages=%+v",
			sawLeadingGap, sawMiddleGap, sawTrailingGap, window.Pages)
	}
}

func TestPublicationKeyMeasureAndProviderLimits(t *testing.T) {
	t.Run("key bytes exact and exceeded", func(t *testing.T) {
		pager := newPublicationPager(t, "abcd", Options{
			TargetPageBytes: 4, MaximumPageBytes: 4,
			MaximumFragments: 2, MaximumKeyBytes: 8,
			Window: Budget{Bytes: 4, Pages: 2, Fragments: 2, Measure: 10},
		})
		stats, err := pager.Publish(context.Background(), Publication{
			Revision: 41, IndexedThrough: 4, Complete: true,
			Fragments: []Fragment{
				{ID: "a", DataKey: "bc", Start: 0, End: 2, Measure: 1},
				{ID: "de", DataKey: "fgh", Start: 2, End: 4, Measure: 1},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if stats.KeyBytes != 8 || stats.MaximumKeyBytes != 8 {
			t.Fatalf("exact key budget Stats = %+v", stats)
		}
		tooLarge := Publication{
			Revision: 41, BaseGeneration: 1, IndexedThrough: 4, Complete: true,
			Fragments: []Fragment{
				{ID: "a", DataKey: "bc", Start: 0, End: 2, Measure: 1},
				{ID: "de", DataKey: "fghi", Start: 2, End: 4, Measure: 1},
			},
		}
		if _, err := pager.Publish(context.Background(), tooLarge); !errors.Is(err, ErrInvalidFragment) {
			t.Fatalf("over key budget Publish = %v", err)
		}
	})

	t.Run("aggregate measure overflow", func(t *testing.T) {
		pager := newPublicationPager(t, "abcd", Options{
			TargetPageBytes: 4, MaximumPageBytes: 4,
			MaximumFragments: 2,
			Window:           Budget{Bytes: 4, Pages: 2, Fragments: 2, Measure: Measure(math.MaxInt64)},
		})
		_, err := pager.Publish(context.Background(), Publication{
			Revision: 41, IndexedThrough: 4, Complete: true,
			Fragments: []Fragment{
				{ID: "left", Start: 0, End: 2, Measure: Measure(math.MaxInt64)},
				{ID: "right", Start: 2, End: 4, Measure: 1},
			},
		})
		if !errors.Is(err, ErrInvalidFragment) {
			t.Fatalf("overflow Publish = %v", err)
		}
		if _, ok := checkedAddMeasure(-1, 0); ok {
			t.Fatal("checkedAddMeasure accepted a negative operand")
		}
	})

	t.Run("provider receives resolved hard limits", func(t *testing.T) {
		pager := newPublicationPager(t, "abcd", Options{
			TargetPageBytes: 4, MaximumPageBytes: 4,
			MaximumFragments: 2, MaximumKeyBytes: 9,
			Window: Budget{Bytes: 4, Pages: 2, Fragments: 2, Measure: 7},
		})
		stats, err := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
				if request.Revision != 41 || request.BaseGeneration != 0 ||
					request.ByteLength != 4 || request.MaxFragments != 2 ||
					request.MaxKeyBytes != 9 || request.MaxFragmentMeasure != 7 {
					return FragmentResult{}, errors.New("unexpected provider limits")
				}
				return FragmentResult{
					IndexedThrough: 4, Complete: true,
					Fragments: []Fragment{{ID: "all", Start: 0, End: 4, Measure: 7}},
				}, nil
			},
		))
		if err != nil {
			t.Fatal(err)
		}
		if stats.Generation != 1 || stats.TotalMeasure != 7 {
			t.Fatalf("Stats = %+v", stats)
		}
	})
}

func TestPublicationCancellationAndFinalCAS(t *testing.T) {
	pager := newPublicationPager(t, "abcd", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if _, err := pager.publish(nil, Publication{}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("publish(nil) = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pager.publish(cancelled, Publication{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish(cancelled) = %v", err)
	}
	if _, _, _, err := pager.validateFragments(newCoreCancelAtContext(1), Publication{
		IndexedThrough: 4,
		Fragments:      []Fragment{{ID: "all", Start: 0, End: 4}},
	}, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("validateFragments cancellation = %v", err)
	}
	if _, err := pager.buildPublishedPages(newCoreCancelAtContext(1), nil, 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("buildPublishedPages cancellation = %v", err)
	}

	t.Run("close wins final publication", func(t *testing.T) {
		source := &blockingReadSource{
			data: []byte("abcd"), started: make(chan struct{}), release: make(chan struct{}),
		}
		pager, err := Build(context.Background(), source, 41, Options{
			TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		source.enabled.Store(true)
		publishDone := make(chan error, 1)
		go func() {
			_, err := pager.Publish(context.Background(), Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 2},
					{ID: "right", Start: 2, End: 4},
				},
			})
			publishDone <- err
		}()
		waitForSignal(t, source.started, "blocked publication read")
		closeDone := make(chan error, 1)
		go func() { closeDone <- pager.Close() }()
		waitForPagerClosed(t, pager)
		close(source.release)
		if err := waitForError(t, publishDone, "publication closed at commit"); !errors.Is(err, ErrClosed) {
			t.Fatalf("Publish = %v", err)
		}
		if err := waitForError(t, closeDone, "Pager.Close"); err != nil {
			t.Fatalf("Close = %v", err)
		}
	})

	t.Run("new generation wins final publication", func(t *testing.T) {
		source := &blockingReadSource{
			data: []byte("abcd"), started: make(chan struct{}), release: make(chan struct{}),
		}
		pager, err := Build(context.Background(), source, 41, Options{
			TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		source.enabled.Store(true)
		publishDone := make(chan error, 1)
		go func() {
			_, err := pager.Publish(context.Background(), Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 2},
					{ID: "right", Start: 2, End: 4},
				},
			})
			publishDone <- err
		}()
		waitForSignal(t, source.started, "blocked publication read")
		pager.mu.Lock()
		pager.state.generation++
		pager.mu.Unlock()
		close(source.release)
		if err := waitForError(t, publishDone, "stale publication commit"); !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("Publish = %v", err)
		}
	})
}

func TestRefreshRejectsCloseAfterTaskAdmission(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcd"), 41, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	refreshed := make(chan error, 1)
	go func() {
		_, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(context.Context, FragmentRequest) (FragmentResult, error) {
				close(entered)
				<-release
				return FragmentResult{}, nil
			},
		))
		refreshed <- refreshErr
	}()
	waitForSignal(t, entered, "FragmentProvider")
	closed := make(chan error, 1)
	go func() { closed <- pager.Close() }()
	waitForPagerClosed(t, pager)
	select {
	case closeErr := <-closed:
		t.Fatalf("Close returned before admitted Refresh completed: %v", closeErr)
	default:
	}
	close(release)
	if refreshErr := waitForError(t, refreshed, "Refresh after Close"); !errors.Is(refreshErr, ErrClosed) {
		t.Fatalf("Refresh after Close = %v", refreshErr)
	}
	if closeErr := waitForError(t, closed, "Close after Refresh"); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func waitForPagerClosed(t *testing.T, pager *Pager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pager.mu.RLock()
		closed := pager.closed
		pager.mu.RUnlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Pager to close")
		}
		runtime.Gosched()
	}
}

func TestFragmentAtMeasureZeroAndDefensiveBoundaries(t *testing.T) {
	allZero := []fragmentMeta{
		{Fragment: Fragment{Measure: 0}, measureStart: 0},
		{Fragment: Fragment{Measure: 0}, measureStart: 0},
	}
	if got := fragmentAtMeasure(allZero, 0, AffinityBefore); got != 1 {
		t.Fatalf("all-zero before index = %d", got)
	}
	if got := fragmentAtMeasure(allZero, 0, AffinityAfter); got != 0 {
		t.Fatalf("all-zero after index = %d", got)
	}
	defensive := []fragmentMeta{
		{Fragment: Fragment{Measure: 1}, measureStart: 5},
		{Fragment: Fragment{Measure: 1}, measureStart: 6},
	}
	if got := fragmentAtMeasure(defensive, 1, AffinityBefore); got != 0 {
		t.Fatalf("defensive before index = %d", got)
	}
}

func TestFragmentContinuationAnchorAndBounds(t *testing.T) {
	body := "界界界界"
	pager := newPublicationPager(t, body, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		Window: Budget{Bytes: 12, Pages: 4, Fragments: 1, Measure: 7},
	})
	if _, err := pager.Publish(context.Background(), Publication{
		Revision: 41, IndexedThrough: int64(len(body)), Complete: true,
		Fragments: []Fragment{{ID: "giant", Start: 0, End: int64(len(body)), Measure: 7}},
	}); err != nil {
		t.Fatal(err)
	}
	for continuation := 0; continuation < 4; continuation++ {
		window, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
			Revision: 41, Generation: 1, ID: "giant", Continuation: continuation,
			Before: math.MaxInt, After: math.MaxInt,
			Budget: Budget{Bytes: 3, Pages: 1, Fragments: 1, Measure: 7},
		})
		if err != nil {
			t.Fatalf("continuation %d: %v", continuation, err)
		}
		if len(window.Pages) != 1 || window.Pages[0].ContinuationIndex != continuation ||
			window.TruncatedBefore != (continuation > 0) ||
			window.TruncatedAfter != (continuation < 3) {
			t.Fatalf("continuation %d Window = %+v", continuation, window)
		}
	}
	for _, continuation := range []int{-1, 4, math.MaxInt} {
		if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
			Revision: 41, Generation: 1, ID: "giant", Continuation: continuation,
		}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("continuation %d = %v", continuation, err)
		}
	}
}

func TestZeroMeasureFragmentsAtBothEnds(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		Window: Budget{Bytes: 12, Pages: 3, Fragments: 3, Measure: 10},
	})
	if _, err := pager.Publish(context.Background(), Publication{
		Revision: 41, IndexedThrough: 12, Complete: true,
		Fragments: []Fragment{
			{ID: "zero-first", Start: 0, End: 4, Measure: 0},
			{ID: "middle", Start: 4, End: 8, Measure: 10},
			{ID: "zero-last", Start: 8, End: 12, Measure: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		offset   Measure
		affinity Affinity
		id       string
	}{
		{name: "zero first before", offset: 0, affinity: AffinityBefore, id: "zero-first"},
		{name: "zero first skipped after", offset: 0, affinity: AffinityAfter, id: "middle"},
		{name: "zero last skipped before", offset: 10, affinity: AffinityBefore, id: "middle"},
		{name: "zero last after", offset: 10, affinity: AffinityAfter, id: "zero-last"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
				Revision: 41, Generation: 1, Offset: test.offset, Affinity: test.affinity,
				Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(window.Pages) != 1 || window.Pages[0].FragmentID != test.id {
				t.Fatalf("Window = %+v", window)
			}
		})
	}
}

func TestWindowErrorAndOverflowBranches(t *testing.T) {
	pager := publishedPager(t)
	validByte := ByteWindowRequest{Revision: 41, Generation: 1, Offset: 1}
	validFragment := FragmentWindowRequest{Revision: 41, Generation: 1, ID: "zero"}
	validMeasure := MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 1, Affinity: AffinityAfter,
	}

	if _, err := pager.WindowByByte(nil, validByte); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("WindowByByte(nil) = %v", err)
	}
	if _, err := pager.WindowByFragment(nil, validFragment); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("WindowByFragment(nil) = %v", err)
	}
	if _, err := pager.WindowByMeasure(nil, validMeasure); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("WindowByMeasure(nil) = %v", err)
	}
	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 40, Generation: 1, ID: "zero",
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("WindowByFragment revision = %v", err)
	}
	if _, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 40, Generation: 1, Offset: 1, Affinity: AffinityAfter,
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("WindowByMeasure revision = %v", err)
	}
	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "zero",
		Budget: Budget{Bytes: 13, Pages: 1, Fragments: 1, Measure: 1},
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WindowByFragment budget = %v", err)
	}
	if _, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 1, Affinity: AffinityAfter,
		Budget: Budget{Bytes: 13, Pages: 1, Fragments: 1, Measure: 1},
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WindowByMeasure budget = %v", err)
	}

	byteWindow, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 1, After: 2,
	})
	if err != nil || len(byteWindow.Pages) != 1 {
		t.Fatalf("bounded byte After = %+v, %v", byteWindow, err)
	}
	fragmentWindow, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "zero", After: 0,
	})
	if err != nil || len(fragmentWindow.Pages) != 1 {
		t.Fatalf("bounded fragment After = %+v, %v", fragmentWindow, err)
	}
	measureWindow, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Offset: 1, Affinity: AffinityAfter,
		Before: 1, After: 2,
	})
	if err != nil || len(measureWindow.Pages) == 0 {
		t.Fatalf("bounded measure overscan = %+v, %v", measureWindow, err)
	}

	fragmentBudget, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "zero", Before: 1, After: 1,
		Budget: Budget{Bytes: 12, Pages: 3, Fragments: 1, Measure: 30},
	})
	if err != nil || !fragmentBudget.TruncatedBefore || !fragmentBudget.TruncatedAfter ||
		len(fragmentBudget.Pages) != 1 {
		t.Fatalf("fragment-count budget = %+v, %v", fragmentBudget, err)
	}

	cancelDuringWindow := &errStepContext{cancelAt: 2}
	if _, err := pager.WindowByByte(cancelDuringWindow, validByte); !errors.Is(err, context.Canceled) {
		t.Fatalf("WindowByByte mid-read cancellation = %v", err)
	}

	faultSource := newCoreTestSource([]byte("abcd"))
	faultPager, err := Build(context.Background(), faultSource, 7, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer faultPager.Close()
	injected := errors.New("window read failed")
	faultSource.setReadHook(func([]byte, int64) (int, error) { return 0, injected })
	if _, err := faultPager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 7, Generation: 0, Offset: 0,
	}); !errors.Is(err, injected) {
		t.Fatalf("WindowByByte source fault = %v", err)
	}

	pager.mu.RLock()
	state := pager.state
	pager.mu.RUnlock()
	if _, err := pager.readWindow(context.Background(), state, -1, 0, 0, 0, pager.options.window); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("readWindow invalid internal bounds = %v", err)
	}

	pages := []pageMeta{{start: 0, end: 2}}
	if got := pageAtByte(pages, 2, 3); got != 0 {
		t.Fatalf("pageAtByte defensive tail = %d", got)
	}
	if got := pageBeforeByte(pages, 0, 2); got != 0 {
		t.Fatalf("pageBeforeByte zero = %d", got)
	}
	if got := pageBeforeByte(pages, 1, 2); got != 0 {
		t.Fatalf("pageBeforeByte interior = %d", got)
	}
}

type errStepContext struct {
	calls    atomic.Int32
	cancelAt int32
}

func (*errStepContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*errStepContext) Done() <-chan struct{}       { return nil }
func (*errStepContext) Value(any) any               { return nil }
func (ctx *errStepContext) Err() error {
	if ctx.calls.Add(1) >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}
