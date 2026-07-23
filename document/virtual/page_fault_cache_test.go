package virtual

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestPageReadSourceFaultsAndExactReadLoop(t *testing.T) {
	injected := errors.New("page read failed")
	tests := []struct {
		name string
		hook func([]byte, int64) (int, error)
		want error
	}{
		{
			name: "read error",
			hook: func([]byte, int64) (int, error) { return 0, injected },
			want: injected,
		},
		{
			name: "zero read",
			hook: func([]byte, int64) (int, error) { return 0, nil },
			want: io.ErrUnexpectedEOF,
		},
		{
			name: "negative count",
			hook: func([]byte, int64) (int, error) { return -1, nil },
			want: ErrSourceInconsistent,
		},
		{
			name: "oversized count",
			hook: func(buffer []byte, _ int64) (int, error) { return len(buffer) + 1, nil },
			want: ErrSourceInconsistent,
		},
		{
			name: "error after complete count",
			hook: func(buffer []byte, offset int64) (int, error) {
				copy(buffer, []byte("abcd")[offset:])
				return len(buffer), injected
			},
			want: injected,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newCoreTestSource([]byte("abcd"))
			pager, err := Build(context.Background(), source, 1, Options{
				TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer pager.Close()
			source.setReadHook(test.hook)
			if _, err := pager.ReadPage(context.Background(), coreTestKey(pager, 0)); !errors.Is(err, test.want) {
				t.Fatalf("ReadPage error = %v, want %v", err, test.want)
			}
		})
	}

	partial := newCoreTestSource([]byte("abcd"))
	pager, err := Build(context.Background(), partial, 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	partial.setReadHook(func(buffer []byte, offset int64) (int, error) {
		buffer[0] = partial.data[offset]
		if offset+1 == int64(len(partial.data)) {
			return 1, io.EOF
		}
		return 1, nil
	})
	page, err := pager.ReadPage(context.Background(), coreTestKey(pager, 0))
	if err != nil || string(page.Content) != "abcd" {
		t.Fatalf("partial ReadPage = (%q, %v)", page.Content, err)
	}
	_ = pager.Close()

	cancelledSource := newCoreTestSource([]byte("abcd"))
	cancelledPager, err := Build(context.Background(), cancelledSource, 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelledSource.setReadHook(func(buffer []byte, offset int64) (int, error) {
		buffer[0] = cancelledSource.data[offset]
		cancel()
		return 1, nil
	})
	if _, err := cancelledPager.ReadPage(ctx, coreTestKey(cancelledPager, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page read error = %v", err)
	}
	_ = cancelledPager.Close()

	if err := readExactAt(context.Background(), newCoreTestSource(nil), nil, 0); err != nil {
		t.Fatalf("empty exact read: %v", err)
	}
	cancelled, cancelEmpty := context.WithCancel(context.Background())
	cancelEmpty()
	if err := readExactAt(cancelled, newCoreTestSource([]byte("x")), make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exact read error = %v", err)
	}
	if _, err := readPageBytes(context.Background(), newCoreTestSource(nil), pageMeta{start: 2, end: 1}); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("negative page length error = %v", err)
	}
}

func TestLineAndBoundaryMapInternalOffsetsAndFaults(t *testing.T) {
	source := newCoreTestSource([]byte("ab\ncd"))
	pager, err := Build(context.Background(), source, 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	lines, afterLF, err := lineAndBoundaryMap(
		context.Background(), source, pager.logical, []int64{0, 1, 3, 4, 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := map[int64]int64{0: 0, 1: 0, 3: 1, 4: 1, 5: 1}
	for offset, want := range wantLines {
		if lines[offset] != want {
			t.Errorf("line at %d = %d, want %d", offset, lines[offset], want)
		}
	}
	if !afterLF[0] || afterLF[1] || !afterLF[3] || afterLF[4] || afterLF[5] {
		t.Fatalf("after-LF map = %#v", afterLF)
	}

	injected := errors.New("boundary read failed")
	source.setReadHook(func([]byte, int64) (int, error) { return 0, injected })
	if _, _, err := lineAndBoundaryMap(
		context.Background(), source, pager.logical, []int64{1},
	); !errors.Is(err, injected) {
		t.Fatalf("boundary read fault = %v", err)
	}
}

func TestPageCacheCopiesLRUAndCapacity(t *testing.T) {
	source := newCoreTestSource([]byte("abcdefghijkl"))
	pager, err := Build(context.Background(), source, 3, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, CacheBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	source.mu.Lock()
	source.reads = 0
	source.mu.Unlock()

	first, err := pager.ReadPage(context.Background(), coreTestKey(pager, 0))
	if err != nil {
		t.Fatal(err)
	}
	if source.readCount() != 1 || pager.Stats().CacheBytes != 4 || pager.Stats().CacheEntries != 1 {
		t.Fatalf("first cache state: reads=%d stats=%+v", source.readCount(), pager.Stats())
	}
	first.Content[0] = 'X'
	again, err := pager.ReadPage(context.Background(), first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Content) != "abcd" || source.readCount() != 1 {
		t.Fatalf("cached copy = %q, reads = %d", again.Content, source.readCount())
	}

	second, err := pager.ReadPage(context.Background(), coreTestKey(pager, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pager.ReadPage(context.Background(), first.Key); err != nil {
		t.Fatal(err)
	}
	third, err := pager.ReadPage(context.Background(), coreTestKey(pager, 2))
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Content) != "efgh" || string(third.Content) != "ijkl" {
		t.Fatalf("unexpected cached pages: %q %q", second.Content, third.Content)
	}
	if stats := pager.Stats(); stats.CacheBytes != 8 || stats.CacheEntries != 2 {
		t.Fatalf("evicted cache stats = %+v", stats)
	}
	pager.mu.RLock()
	_, hasFirst := pager.cache[cacheKey{generation: 0, index: 0, start: 0, end: 4}]
	_, hasSecond := pager.cache[cacheKey{generation: 0, index: 1, start: 4, end: 8}]
	_, hasThird := pager.cache[cacheKey{generation: 0, index: 2, start: 8, end: 12}]
	pager.mu.RUnlock()
	if !hasFirst || hasSecond || !hasThird {
		t.Fatalf("LRU membership = first:%v second:%v third:%v", hasFirst, hasSecond, hasThird)
	}
	if _, err := pager.ReadPage(context.Background(), second.Key); err != nil {
		t.Fatal(err)
	}
	if source.readCount() != 4 {
		t.Fatalf("read count after evicted reload = %d", source.readCount())
	}

	state := pager.state
	key := cacheKey{generation: state.generation, index: 99, start: 0, end: 1}
	before := pager.Stats()
	pager.storeCache(state, key, []byte("x"))
	pager.storeCache(state, key, []byte("changed"))
	after := pager.Stats()
	if before.CacheEntries != 2 || before.CacheBytes != 8 || after.CacheEntries != 2 || after.CacheBytes != 5 {
		t.Fatalf("duplicate direct cache store: before=%+v after=%+v", before, after)
	}
	if got, ok := pager.cached(key); !ok || string(got) != "x" {
		t.Fatalf("direct cached entry = %q, %v", got, ok)
	}
	if _, ok := pager.cached(cacheKey{index: -1}); ok {
		t.Fatal("missing cache key was reported present")
	}

	stale := &pagerState{}
	staleBefore := pager.Stats()
	pager.storeCache(stale, cacheKey{index: 100}, []byte("z"))
	if got := pager.Stats(); got.CacheEntries != staleBefore.CacheEntries || got.CacheBytes != staleBefore.CacheBytes {
		t.Fatalf("stale state changed cache: before=%+v after=%+v", staleBefore, got)
	}
}

func TestPageCacheDisabledOversizedAndClosed(t *testing.T) {
	tests := []struct {
		name    string
		options Options
	}{
		{
			name: "disabled",
			options: Options{
				TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
			},
		},
		{
			name: "page exceeds cache",
			options: Options{
				TargetPageBytes: 4, MaximumPageBytes: 4, CacheBytes: 3,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newCoreTestSource([]byte("abcd"))
			pager, err := Build(context.Background(), source, 1, test.options)
			if err != nil {
				t.Fatal(err)
			}
			source.mu.Lock()
			source.reads = 0
			source.mu.Unlock()
			key := coreTestKey(pager, 0)
			if _, err := pager.ReadPage(context.Background(), key); err != nil {
				t.Fatal(err)
			}
			if _, err := pager.ReadPage(context.Background(), key); err != nil {
				t.Fatal(err)
			}
			if source.readCount() != 2 || pager.Stats().CacheEntries != 0 || pager.Stats().CacheBytes != 0 {
				t.Fatalf("uncached reads=%d stats=%+v", source.readCount(), pager.Stats())
			}
			if err := pager.Close(); err != nil {
				t.Fatal(err)
			}
			if _, ok := pager.cached(cacheKey{}); ok {
				t.Fatal("closed Pager returned cache data")
			}
			pager.storeCache(pager.state, cacheKey{}, []byte("x"))
			if pager.Stats().CacheEntries != 0 {
				t.Fatalf("closed store changed stats: %+v", pager.Stats())
			}
		})
	}
}

func TestLogicalWindowBudgetsAndPageKeyValidation(t *testing.T) {
	pager, err := Build(context.Background(), newCoreTestSource([]byte("abcdefghijkl")), 8, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
		Window: Budget{Bytes: 12, Pages: 3, Fragments: 1, Measure: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 8, Generation: 0, Offset: 5, Before: 5, After: 7,
		Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Pages) != 1 || string(window.Pages[0].Content) != "efgh" ||
		window.Bytes != 4 || !window.TruncatedBefore || !window.TruncatedAfter {
		t.Fatalf("bounded window = %+v, content=%q", window, window.Pages[0].Content)
	}

	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 8, Offset: 5, Budget: Budget{Bytes: 3, Pages: 1, Fragments: 1, Measure: 1},
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("undersized anchor budget error = %v", err)
	}
	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 8, Offset: 5, Budget: Budget{Bytes: 13},
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized request budget error = %v", err)
	}

	validBudget, err := pager.resolveBudget(Budget{})
	if err != nil || validBudget != (Budget{Bytes: 12, Pages: 3, Fragments: 1, Measure: 1}) {
		t.Fatalf("default request budget = (%+v, %v)", validBudget, err)
	}
	invalidBudgets := []Budget{
		{Bytes: -1}, {Pages: -1}, {Fragments: -1}, {Measure: -1},
		{Bytes: 13}, {Pages: 4}, {Fragments: 2}, {Measure: 2},
	}
	for _, budget := range invalidBudgets {
		if _, err := pager.resolveBudget(budget); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("budget %+v error = %v", budget, err)
		}
	}

	invalidRequests := []ByteWindowRequest{
		{Revision: 8, Offset: -1},
		{Revision: 8, Offset: 13},
		{Revision: 8, Offset: 0, Before: -1},
		{Revision: 8, Offset: 0, After: -1},
	}
	for _, request := range invalidRequests {
		if _, err := pager.WindowByByte(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("request %+v error = %v", request, err)
		}
	}
	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 7,
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision error = %v", err)
	}
	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 8, Generation: 1,
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("generation error = %v", err)
	}

	key := coreTestKey(pager, 0)
	badKeys := []PageKey{
		{Revision: 7, Generation: key.Generation, Index: key.Index, Start: key.Start, End: key.End},
		{Revision: key.Revision, Generation: 1, Index: key.Index, Start: key.Start, End: key.End},
		{Revision: key.Revision, Generation: key.Generation, Index: -1, Start: key.Start, End: key.End},
		{Revision: key.Revision, Generation: key.Generation, Index: 99, Start: key.Start, End: key.End},
		{Revision: key.Revision, Generation: key.Generation, Index: key.Index, Start: 1, End: key.End},
		{Revision: key.Revision, Generation: key.Generation, Index: key.Index, Start: key.Start, End: 3},
	}
	wants := []error{
		ErrRevisionMismatch, ErrStaleGeneration, ErrInvalidRequest,
		ErrInvalidRequest, ErrInvalidRequest, ErrInvalidRequest,
	}
	for index, bad := range badKeys {
		if _, err := pager.ReadPage(context.Background(), bad); !errors.Is(err, wants[index]) {
			t.Errorf("bad key %+v error = %v, want %v", bad, err, wants[index])
		}
	}

	eofWindow, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 8, Offset: 12, After: mathMaxInt64ForCoreTest(),
		Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 1},
	})
	if err != nil || len(eofWindow.Pages) != 1 || string(eofWindow.Pages[0].Content) != "ijkl" {
		t.Fatalf("EOF window = (%+v, %v)", eofWindow, err)
	}
}

func mathMaxInt64ForCoreTest() int64 { return int64(^uint64(0) >> 1) }

func TestTaskLimitCloseBarrierAndOwnedRelease(t *testing.T) {
	closeFailure := errors.New("owned release failed")
	closeStarted := make(chan struct{})
	finishClose := make(chan struct{})
	source := newCoreTestSource([]byte("abcd"))
	source.closeHook = func() error {
		close(closeStarted)
		<-finishClose
		return closeFailure
	}
	pager, err := BuildOwned(context.Background(), source, 4, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		MaximumTasks: 1, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := coreTestKey(pager, 0)

	started := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	source.setReadHook(func(buffer []byte, offset int64) (int, error) {
		once.Do(func() { close(started) })
		<-unblock
		return coreTestRead(source.data, buffer, offset)
	})

	readDone := make(chan error, 1)
	go func() {
		_, err := pager.ReadPage(context.Background(), key)
		readDone <- err
	}()
	<-started
	if stats := pager.Stats(); stats.ActiveTasks != 1 || stats.MaximumTasks != 1 {
		t.Fatalf("active task stats = %+v", stats)
	}
	if _, err := pager.ReadPage(context.Background(), key); !errors.Is(err, ErrBusy) {
		t.Fatalf("second task error = %v", err)
	}
	if _, err := pager.ReadPage(nil, key); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil task Context error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pager.ReadPage(cancelled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled task Context error = %v", err)
	}
	if _, _, err := pager.acquireTask(newCoreStepContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context cancelled at task select error = %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- pager.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		pager.mu.RLock()
		closed := pager.closed
		pager.mu.RUnlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not enter the close barrier")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while task was active: %v", err)
	default:
	}
	if source.closeCount() != 0 {
		t.Fatalf("owned Source closed before task exit: %d", source.closeCount())
	}

	close(unblock)
	if err := <-readDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight read error = %v", err)
	}
	<-closeStarted
	secondCloseDone := make(chan error, 1)
	go func() { secondCloseDone <- pager.Close() }()
	select {
	case err := <-secondCloseDone:
		t.Fatalf("concurrent Close returned before owned release: %v", err)
	default:
	}
	close(finishClose)
	if err := <-closeDone; !errors.Is(err, closeFailure) {
		t.Fatalf("Close release error = %v", err)
	}
	if err := <-secondCloseDone; !errors.Is(err, closeFailure) {
		t.Fatalf("concurrent Close release error = %v", err)
	}
	if source.closeCount() != 1 {
		t.Fatalf("owned Source close count = %d", source.closeCount())
	}
	if stats := pager.Stats(); stats.ActiveTasks != 0 || stats.CacheBytes != 0 || stats.CacheEntries != 0 {
		t.Fatalf("closed stats = %+v", stats)
	}
	if _, err := pager.ReadPage(context.Background(), key); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-Close read error = %v", err)
	}
	if err := pager.Close(); !errors.Is(err, closeFailure) || source.closeCount() != 1 {
		t.Fatalf("second Close = %v, source closes = %d", err, source.closeCount())
	}
}
