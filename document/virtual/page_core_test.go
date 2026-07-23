package virtual

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type coreTestSource struct {
	mu        sync.Mutex
	data      []byte
	readHook  func([]byte, int64) (int, error)
	lenHook   func() int64
	closeHook func() error
	closeErr  error
	reads     int
	closes    int
}

// coreStepContext models cancellation becoming visible immediately after the
// operation's initial fast-path check.
type coreStepContext struct {
	calls    atomic.Int32
	cancelAt int32
	done     chan struct{}
}

func newCoreStepContext() *coreStepContext {
	return newCoreCancelAtContext(2)
}

func newCoreCancelAtContext(cancelAt int32) *coreStepContext {
	done := make(chan struct{})
	close(done)
	return &coreStepContext{cancelAt: cancelAt, done: done}
}

func (c *coreStepContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *coreStepContext) Done() <-chan struct{}       { return c.done }
func (c *coreStepContext) Value(any) any               { return nil }
func (c *coreStepContext) Err() error {
	if c.calls.Add(1) < c.cancelAt {
		return nil
	}
	return context.Canceled
}

func newCoreTestSource(value []byte) *coreTestSource {
	return &coreTestSource{data: append([]byte(nil), value...)}
}

func (s *coreTestSource) Len() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lenHook != nil {
		return s.lenHook()
	}
	return int64(len(s.data))
}

func (s *coreTestSource) ReadAt(buffer []byte, offset int64) (int, error) {
	s.mu.Lock()
	s.reads++
	hook := s.readHook
	s.mu.Unlock()
	if hook != nil {
		return hook(buffer, offset)
	}
	return coreTestRead(s.data, buffer, offset)
}

func (s *coreTestSource) Close() error {
	s.mu.Lock()
	s.closes++
	hook, closeErr := s.closeHook, s.closeErr
	s.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return closeErr
}

func (s *coreTestSource) setReadHook(hook func([]byte, int64) (int, error)) {
	s.mu.Lock()
	s.readHook = hook
	s.mu.Unlock()
}

func (s *coreTestSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *coreTestSource) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func coreTestRead(data []byte, buffer []byte, offset int64) (int, error) {
	if offset < 0 || offset > int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(buffer, data[int(offset):])
	if n < len(buffer) {
		return n, io.EOF
	}
	return n, nil
}

func coreTestKey(pager *Pager, index int) PageKey {
	pager.mu.RLock()
	defer pager.mu.RUnlock()
	meta := pager.state.pages[index]
	return PageKey{
		Revision: pager.revision, Generation: pager.state.generation,
		Index: index, Start: meta.start, End: meta.end, identity: pager.identity,
	}
}

func TestLogicalPageBuildAndReadBoundaries(t *testing.T) {
	crossBuffer := append(bytes.Repeat([]byte{'a'}, scanBufferBytes-1), []byte("🙂z")...)
	tests := []struct {
		name    string
		content []byte
		target  int64
		maximum int64
		ranges  [][2]int64
	}{
		{name: "empty", target: 4, maximum: 4, ranges: [][2]int64{{0, 0}}},
		{name: "exact", content: []byte("abcd"), target: 4, maximum: 4, ranges: [][2]int64{{0, 4}}},
		{name: "hard split", content: []byte("abcde"), target: 4, maximum: 4, ranges: [][2]int64{{0, 4}, {4, 5}}},
		{name: "newline at target", content: []byte("abc\n"), target: 4, maximum: 8, ranges: [][2]int64{{0, 4}}},
		{name: "newline after target", content: []byte("abcd\nxyz"), target: 4, maximum: 8, ranges: [][2]int64{{0, 5}, {5, 8}}},
		{name: "four byte rune", content: []byte("🙂a"), target: 4, maximum: 4, ranges: [][2]int64{{0, 4}, {4, 5}}},
		{
			name: "rune across scan buffer", content: crossBuffer,
			target: scanBufferBytes, maximum: scanBufferBytes,
			ranges: [][2]int64{{0, scanBufferBytes - 1}, {scanBufferBytes - 1, int64(len(crossBuffer))}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newCoreTestSource(test.content)
			pager, err := Build(context.Background(), source, 17, Options{
				TargetPageBytes: test.target, MaximumPageBytes: test.maximum,
				DisableCache: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer pager.Close()

			stats := pager.Stats()
			if stats.Revision != 17 || stats.Generation != 0 ||
				stats.ByteLength != int64(len(test.content)) ||
				stats.LogicalPages != len(test.ranges) || stats.Pages != len(test.ranges) {
				t.Fatalf("unexpected stats: %+v", stats)
			}

			var rebuilt []byte
			for index, expected := range test.ranges {
				key := coreTestKey(pager, index)
				if key.Start != expected[0] || key.End != expected[1] {
					t.Fatalf("page %d key = %+v, want range %v", index, key, expected)
				}
				page, err := pager.ReadPage(context.Background(), key)
				if err != nil {
					t.Fatalf("read page %d: %v", index, err)
				}
				if !utf8.Valid(page.Content) {
					t.Fatalf("page %d is not valid UTF-8: %x", index, page.Content)
				}
				if page.StartLine != int64(bytes.Count(test.content[:expected[0]], []byte{'\n'})) ||
					page.EndLine != int64(bytes.Count(test.content[:expected[1]], []byte{'\n'})) {
					t.Fatalf("page %d line range = [%d,%d)", index, page.StartLine, page.EndLine)
				}
				wantFrom := expected[0] > 0 && test.content[expected[0]-1] != '\n'
				wantTo := expected[1] < int64(len(test.content)) && expected[1] > 0 && test.content[expected[1]-1] != '\n'
				if page.ContinuesFromPrevious != wantFrom || page.ContinuesToNext != wantTo {
					t.Fatalf("page %d continuation = (%v,%v), want (%v,%v)", index,
						page.ContinuesFromPrevious, page.ContinuesToNext, wantFrom, wantTo)
				}
				if page.FragmentIndex != -1 || page.Indexed || page.FragmentID != "" ||
					page.ContinuationIndex != 0 || page.ContinuationCount != 0 {
					t.Fatalf("logical page unexpectedly carries Fragment metadata: %+v", page)
				}
				rebuilt = append(rebuilt, page.Content...)
			}
			if !bytes.Equal(rebuilt, test.content) {
				t.Fatalf("rebuilt content differs:\n got %x\nwant %x", rebuilt, test.content)
			}
		})
	}
}

func TestBuildOptionsDefaultsValidationAndLimits(t *testing.T) {
	defaultPager, err := Build(context.Background(), newCoreTestSource(nil), 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defaultStats := defaultPager.Stats()
	if defaultStats.TargetPageBytes != DefaultTargetPageBytes ||
		defaultStats.MaximumPageBytes != DefaultMaximumPageBytes ||
		defaultStats.MaximumCacheBytes != DefaultCacheBytes ||
		defaultStats.MaximumTasks != DefaultMaximumConcurrentTasks ||
		defaultStats.MaximumKeyBytes != DefaultMaximumKeyBytes {
		t.Fatalf("defaults not resolved: %+v", defaultStats)
	}
	if err := defaultPager.Close(); err != nil {
		t.Fatal(err)
	}

	limitPager, err := Build(context.Background(), newCoreTestSource(nil), math.MaxUint64, Options{
		TargetPageBytes: 4, MaximumPageBytes: MaximumPageBytes,
		MaximumFragments: MaximumFragments, MaximumTasks: MaximumConcurrentTasks,
		MaximumKeyBytes: MaximumKeyBytes, CacheBytes: MaximumCacheBytes,
		Window: Budget{Bytes: math.MaxInt64, Pages: math.MaxInt, Fragments: math.MaxInt, Measure: Measure(math.MaxInt64)},
	})
	if err != nil {
		t.Fatalf("maximum accepted options: %v", err)
	}
	if got := limitPager.Stats(); got.Revision != math.MaxUint64 ||
		got.MaximumPageBytes != MaximumPageBytes || got.MaximumCacheBytes != MaximumCacheBytes ||
		got.MaximumKeyBytes != MaximumKeyBytes {
		t.Fatalf("maximum stats = %+v", got)
	}
	if err := limitPager.Close(); err != nil {
		t.Fatal(err)
	}

	noCache, err := Build(context.Background(), newCoreTestSource(nil), 0, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if noCache.Stats().MaximumCacheBytes != 0 {
		t.Fatalf("disabled cache stats = %+v", noCache.Stats())
	}
	_ = noCache.Close()

	invalid := []Options{
		{TargetPageBytes: -1},
		{TargetPageBytes: 3, MaximumPageBytes: 4},
		{TargetPageBytes: 8, MaximumPageBytes: 4},
		{TargetPageBytes: 4, MaximumPageBytes: MaximumPageBytes + 1},
		{MaximumFragments: -1},
		{MaximumFragments: MaximumFragments + 1},
		{MaximumTasks: -1},
		{MaximumTasks: MaximumConcurrentTasks + 1},
		{MaximumKeyBytes: -1},
		{MaximumKeyBytes: MaximumKeyBytes + 1},
		{CacheBytes: -1},
		{CacheBytes: MaximumCacheBytes + 1},
		{DisableCache: true, CacheBytes: 1},
		{Window: Budget{Bytes: -1}},
		{Window: Budget{Pages: -1}},
		{Window: Budget{Fragments: -1}},
		{Window: Budget{Measure: -1}},
	}
	for index, options := range invalid {
		if _, err := Build(context.Background(), newCoreTestSource(nil), 0, options); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("invalid options %d error = %v", index, err)
		}
	}

	if _, err := Build(nil, newCoreTestSource(nil), 0, Options{}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil Context error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(cancelled, newCoreTestSource(nil), 0, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Context error = %v", err)
	}
	if _, err := Build(context.Background(), nil, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil Source error = %v", err)
	}
	negative := newCoreTestSource(nil)
	negative.lenHook = func() int64 { return -1 }
	if _, err := Build(context.Background(), negative, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("negative length error = %v", err)
	}
}

func TestBuildSourceFaultsAndOwnedLifecycle(t *testing.T) {
	injected := errors.New("read failed")
	tests := []struct {
		name    string
		content []byte
		hook    func([]byte, int64) (int, error)
		want    error
	}{
		{name: "invalid byte", content: []byte{0xff}, want: ErrInvalidUTF8},
		{name: "truncated rune", content: []byte{0xf0, 0x9f, 0x99}, want: ErrInvalidUTF8},
		{
			name: "read error", content: []byte("abcd"),
			hook: func([]byte, int64) (int, error) { return 0, injected }, want: injected,
		},
		{
			name: "zero read", content: []byte("abcd"),
			hook: func([]byte, int64) (int, error) { return 0, nil }, want: io.ErrUnexpectedEOF,
		},
		{
			name: "negative count", content: []byte("abcd"),
			hook: func([]byte, int64) (int, error) { return -1, nil }, want: ErrSourceInconsistent,
		},
		{
			name: "oversized count", content: []byte("abcd"),
			hook: func(buffer []byte, _ int64) (int, error) { return len(buffer) + 1, nil }, want: ErrSourceInconsistent,
		},
		{
			name: "premature eof", content: []byte("abcd"),
			hook: func(buffer []byte, _ int64) (int, error) {
				buffer[0] = 'a'
				return 1, io.EOF
			},
			want: io.EOF,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newCoreTestSource(test.content)
			source.readHook = test.hook
			if _, err := Build(context.Background(), source, 0, Options{
				TargetPageBytes: 4, MaximumPageBytes: 4,
			}); !errors.Is(err, test.want) {
				t.Fatalf("Build error = %v, want %v", err, test.want)
			}
		})
	}

	var lengthCalls atomic.Int32
	changing := newCoreTestSource([]byte("abcd"))
	changing.lenHook = func() int64 {
		if lengthCalls.Add(1) >= 2 {
			return 5
		}
		return 4
	}
	if _, err := Build(context.Background(), changing, 0, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	}); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("changed length error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringRead := newCoreTestSource([]byte(strings.Repeat("x", 8)))
	cancelDuringRead.readHook = func(buffer []byte, offset int64) (int, error) {
		n, err := coreTestRead(cancelDuringRead.data, buffer, offset)
		cancel()
		return n, err
	}
	if _, err := Build(ctx, cancelDuringRead, 0, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-scan cancellation error = %v", err)
	}

	cancelAfterEmptyScan := newCoreStepContext()
	if _, err := Build(cancelAfterEmptyScan, newCoreTestSource(nil), 0, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-scan cancellation error = %v", err)
	}
	cancelBeforeSecondRead := newCoreCancelAtContext(4)
	cancelBetweenReads := newCoreTestSource([]byte("ab"))
	cancelBetweenReads.readHook = func(buffer []byte, offset int64) (int, error) {
		buffer[0] = cancelBetweenReads.data[offset]
		return 1, nil
	}
	if _, err := Build(cancelBeforeSecondRead, cancelBetweenReads, 0, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("between-read cancellation error = %v", err)
	}

	for _, test := range []struct {
		name string
		hook func([]byte, int64) (int, error)
	}{
		{
			name: "partial successful reads",
			hook: func(buffer []byte, offset int64) (int, error) {
				buffer[0] = []byte("abcd")[offset]
				return 1, nil
			},
		},
		{
			name: "exact final eof",
			hook: func(buffer []byte, offset int64) (int, error) {
				n := copy(buffer, []byte("abcd")[offset:])
				return n, io.EOF
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := newCoreTestSource([]byte("abcd"))
			source.readHook = test.hook
			pager, err := Build(context.Background(), source, 0, Options{
				TargetPageBytes: 4, MaximumPageBytes: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = pager.Close()
		})
	}

	closeFailure := errors.New("close failed")
	ownedFailure := newCoreTestSource([]byte{0xff})
	ownedFailure.closeErr = closeFailure
	if _, err := BuildOwned(context.Background(), ownedFailure, 0, Options{}); !errors.Is(err, ErrInvalidUTF8) ||
		!errors.Is(err, closeFailure) || ownedFailure.closeCount() != 1 {
		t.Fatalf("owned build failure = %v, closes = %d", err, ownedFailure.closeCount())
	}
	if _, err := BuildOwned(context.Background(), nil, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil owned Source error = %v", err)
	}

	owned := newCoreTestSource([]byte("ok"))
	owned.closeErr = closeFailure
	pager, err := BuildOwned(context.Background(), owned, 9, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pager.Close(); !errors.Is(err, closeFailure) || owned.closeCount() != 1 {
		t.Fatalf("first Close = %v, closes = %d", err, owned.closeCount())
	}
	if err := pager.Close(); !errors.Is(err, closeFailure) || owned.closeCount() != 1 {
		t.Fatalf("second Close = %v, closes = %d", err, owned.closeCount())
	}

	borrowed := newCoreTestSource([]byte("ok"))
	borrowedPager, err := Build(context.Background(), borrowed, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := borrowedPager.Close(); err != nil || borrowed.closeCount() != 0 {
		t.Fatalf("borrowed Close = %v, source closes = %d", err, borrowed.closeCount())
	}
	if _, err := borrowedPager.capture(0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed direct capture error = %v", err)
	}
}

func TestMaximumKeyBytesStatsAndMeasureAddition(t *testing.T) {
	pager, err := Build(context.Background(), newCoreTestSource([]byte("abcd")), 6, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumKeyBytes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	if stats := pager.Stats(); stats.MaximumKeyBytes != 5 || stats.KeyBytes != 0 {
		t.Fatalf("initial key stats = %+v", stats)
	}
	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 6, IndexedThrough: 4, Complete: true,
		Fragments: []Fragment{{ID: "é", Start: 0, End: 4, Measure: 1, DataKey: "界"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MaximumKeyBytes != 5 || stats.KeyBytes != 5 {
		t.Fatalf("published key stats = %+v", stats)
	}
	if sum, ok := checkedAddMeasure(2, 3); !ok || sum != 5 {
		t.Fatalf("checked measure sum = (%d, %v)", sum, ok)
	}
	for _, values := range [][2]Measure{
		{-1, 0}, {0, -1}, {Measure(math.MaxInt64), 1},
	} {
		if sum, ok := checkedAddMeasure(values[0], values[1]); ok || sum != 0 {
			t.Errorf("invalid checked measure %v = (%d, %v)", values, sum, ok)
		}
	}
}

func FuzzLogicalPagePartition(f *testing.F) {
	f.Add([]byte{}, uint16(4), uint16(4))
	f.Add([]byte("one\ntwo\nthree"), uint16(4), uint16(9))
	f.Add([]byte("🙂界\nabc"), uint16(5), uint16(8))
	f.Add([]byte{0xff}, uint16(4), uint16(4))

	f.Fuzz(func(t *testing.T, content []byte, targetSeed, slackSeed uint16) {
		if len(content) > 1<<20 {
			t.Skip()
		}
		target := int64(4 + targetSeed%1021)
		maximum := target + int64(slackSeed%1025)
		source := newCoreTestSource(content)
		pager, err := Build(context.Background(), source, 23, Options{
			TargetPageBytes: target, MaximumPageBytes: maximum, DisableCache: true,
			Window: Budget{
				Bytes: max(maximum, max(1, int64(len(content)))), Pages: max(1, len(content)+1),
				Fragments: 1, Measure: 1,
			},
		})
		if !utf8.Valid(content) {
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("invalid UTF-8 Build error = %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()

		stats := pager.Stats()
		if stats.LogicalPages < 1 || stats.Pages != stats.LogicalPages {
			t.Fatalf("page counts = %+v", stats)
		}
		cursor := int64(0)
		var rebuilt []byte
		for index := 0; index < stats.Pages; index++ {
			page, err := pager.ReadPage(context.Background(), coreTestKey(pager, index))
			if err != nil {
				t.Fatal(err)
			}
			if page.Key.Start != cursor || page.Key.End < page.Key.Start ||
				page.Key.End-page.Key.Start > maximum {
				t.Fatalf("invalid page %d range [%d,%d), cursor=%d max=%d",
					index, page.Key.Start, page.Key.End, cursor, maximum)
			}
			if len(content) != 0 && page.Key.End == page.Key.Start {
				t.Fatalf("non-empty document has empty page %d", index)
			}
			if !utf8.Valid(page.Content) ||
				page.StartLine != int64(bytes.Count(content[:page.Key.Start], []byte{'\n'})) ||
				page.EndLine != int64(bytes.Count(content[:page.Key.End], []byte{'\n'})) {
				t.Fatalf("invalid page %d content/line metadata: %+v", index, page)
			}
			wantFrom := page.Key.Start > 0 && content[page.Key.Start-1] != '\n'
			wantTo := page.Key.End < int64(len(content)) && page.Key.End > 0 && content[page.Key.End-1] != '\n'
			if page.ContinuesFromPrevious != wantFrom || page.ContinuesToNext != wantTo {
				t.Fatalf("page %d continuation = (%v,%v), want (%v,%v)", index,
					page.ContinuesFromPrevious, page.ContinuesToNext, wantFrom, wantTo)
			}
			rebuilt = append(rebuilt, page.Content...)
			cursor = page.Key.End
		}
		if cursor != int64(len(content)) || !bytes.Equal(rebuilt, content) {
			t.Fatalf("partition ends at %d/%d or rebuild differs", cursor, len(content))
		}
	})
}
