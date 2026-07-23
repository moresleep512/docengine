package virtual

import (
	"context"
	"errors"
	"io"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type testSource []byte

func (source testSource) Len() int64 {
	return int64(len(source))
}

func (source testSource) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative test offset")
	}
	if offset >= int64(len(source)) {
		return 0, io.EOF
	}
	n := copy(buffer, source[offset:])
	if n != len(buffer) {
		return n, io.EOF
	}
	return n, nil
}

type fragmentProviderFunc func(context.Context, FragmentRequest) (FragmentResult, error)

func (provider fragmentProviderFunc) Fragments(ctx context.Context, request FragmentRequest) (FragmentResult, error) {
	return provider(ctx, request)
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForRequest(t *testing.T, requests <-chan FragmentRequest) FragmentRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for FragmentProvider request")
		return FragmentRequest{}
	}
}

func waitForError(t *testing.T, results <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func newPublicationPager(t *testing.T, body string, options Options) *Pager {
	t.Helper()
	pager, err := Build(context.Background(), testSource(body), 41, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pager.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	})
	return pager
}

func completePublication(generation uint64) Publication {
	return Publication{
		Revision:       41,
		BaseGeneration: generation,
		IndexedThrough: 12,
		Complete:       true,
		Fragments: []Fragment{
			{ID: "first", Start: 0, End: 4, Measure: 10, DataKey: "data-1"},
			{ID: "zero", Start: 4, End: 8, Measure: 0, DataKey: "data-2"},
			{ID: "last", Start: 8, End: 12, Measure: 20, DataKey: "data-3"},
		},
	}
}

func TestPublishAtomicallyReplacesFragmentPrefix(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumFragments: 8,
	})
	publication := completePublication(0)
	stats, err := pager.Publish(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Revision != 41 || stats.Generation != 1 || stats.Fragments != 3 ||
		stats.IndexedThrough != 12 || !stats.Complete || stats.TotalMeasure != 30 {
		t.Fatalf("Stats = %+v", stats)
	}

	// A publication is immutable after installation, even if the host reuses
	// and mutates its input slice.
	publication.Fragments[0].ID = "mutated"
	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "first",
	}); err != nil {
		t.Fatalf("WindowByFragment(first) = %v", err)
	}
	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 41, Generation: 1, ID: "mutated",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WindowByFragment(mutated) = %v", err)
	}

	before := pager.Stats()
	invalid := completePublication(1)
	invalid.Fragments[1].Start--
	if _, err := pager.Publish(context.Background(), invalid); !errors.Is(err, ErrInvalidFragment) {
		t.Fatalf("Publish(invalid) = %v", err)
	}
	if after := pager.Stats(); after.Generation != before.Generation ||
		after.Fragments != before.Fragments || after.TotalMeasure != before.TotalMeasure {
		t.Fatalf("invalid publication changed state: before=%+v after=%+v", before, after)
	}
}

func TestPublishRejectsInvalidMetadataAndFragments(t *testing.T) {
	baseFragments := []Fragment{
		{ID: "left", Start: 0, End: 2, Measure: 1},
		{ID: "right", Start: 2, End: 4, Measure: 2},
	}
	tests := []struct {
		name        string
		publication Publication
		want        error
	}{
		{
			name: "negative indexed through",
			publication: Publication{
				Revision: 41, IndexedThrough: -1,
			},
			want: ErrInvalidPublication,
		},
		{
			name: "indexed beyond source",
			publication: Publication{
				Revision: 41, IndexedThrough: 5, Fragments: baseFragments,
			},
			want: ErrInvalidPublication,
		},
		{
			name: "complete prefix",
			publication: Publication{
				Revision: 41, IndexedThrough: 2, Complete: true,
				Fragments: []Fragment{{ID: "left", Start: 0, End: 2}},
			},
			want: ErrInvalidPublication,
		},
		{
			name: "fragments for empty prefix",
			publication: Publication{
				Revision: 41, Fragments: []Fragment{{ID: "empty", Start: 0, End: 1}},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "blank id",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{{Start: 0, End: 4}},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "overlap",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 3},
					{ID: "right", Start: 2, End: 4},
				},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "empty range",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 0},
					{ID: "right", Start: 0, End: 4},
				},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "range beyond indexed prefix",
			publication: Publication{
				Revision: 41, IndexedThrough: 3,
				Fragments: []Fragment{{ID: "all", Start: 0, End: 4}},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "negative measure",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{{ID: "all", Start: 0, End: 4, Measure: -1}},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "duplicate id",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "same", Start: 0, End: 2},
					{ID: "same", Start: 2, End: 4},
				},
			},
			want: ErrInvalidFragment,
		},
		{
			name: "measure overflow",
			publication: Publication{
				Revision: 41, IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 2, Measure: Measure(math.MaxInt64)},
					{ID: "right", Start: 2, End: 4, Measure: 1},
				},
			},
			want: ErrInvalidFragment,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pager := newPublicationPager(t, "abcd", Options{MaximumFragments: 2})
			if _, err := pager.Publish(context.Background(), test.publication); !errors.Is(err, test.want) {
				t.Fatalf("Publish = %v, want %v", err, test.want)
			}
			if stats := pager.Stats(); stats.Generation != 0 || stats.Fragments != 0 {
				t.Fatalf("rejected publication changed state: %+v", stats)
			}
		})
	}

	t.Run("gaps and unfinished EOF are valid", func(t *testing.T) {
		pager := newPublicationPager(t, "abcd", Options{})
		stats, err := pager.Publish(context.Background(), Publication{
			Revision: 41, IndexedThrough: 4,
			Fragments: []Fragment{{ID: "middle", Start: 1, End: 3, Measure: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Complete || stats.IndexedThrough != 4 || stats.Fragments != 1 {
			t.Fatalf("Stats = %+v", stats)
		}
	})

	t.Run("too many fragments", func(t *testing.T) {
		pager := newPublicationPager(t, "abcd", Options{MaximumFragments: 1})
		publication := Publication{
			Revision: 41, IndexedThrough: 4, Complete: true,
			Fragments: baseFragments,
		}
		if _, err := pager.Publish(context.Background(), publication); !errors.Is(err, ErrInvalidPublication) {
			t.Fatalf("Publish = %v", err)
		}
	})

	t.Run("UTF-8 split", func(t *testing.T) {
		pager := newPublicationPager(t, "aéb", Options{})
		publication := Publication{
			Revision: 41, IndexedThrough: 4, Complete: true,
			Fragments: []Fragment{
				{ID: "left", Start: 0, End: 2},
				{ID: "right", Start: 2, End: 4},
			},
		}
		if _, err := pager.Publish(context.Background(), publication); !errors.Is(err, ErrInvalidFragment) {
			t.Fatalf("Publish = %v", err)
		}
	})
}

func TestPublishGenerationGuards(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	publication := completePublication(0)
	publication.Revision++
	if _, err := pager.Publish(context.Background(), publication); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}
	publication = completePublication(1)
	if _, err := pager.Publish(context.Background(), publication); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("future generation = %v", err)
	}
	if _, err := pager.Publish(context.Background(), completePublication(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pager.Publish(context.Background(), completePublication(0)); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old generation = %v", err)
	}

	pager.mu.Lock()
	pager.state.generation = math.MaxUint64
	pager.mu.Unlock()
	overflow := completePublication(math.MaxUint64)
	if _, err := pager.Publish(context.Background(), overflow); !errors.Is(err, ErrGenerationOverflow) {
		t.Fatalf("generation overflow = %v", err)
	}
}

func TestRefreshProviderAndGenerationCAS(t *testing.T) {
	pager := newPublicationPager(t, "abcdefghijkl", Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumFragments: 8, MaximumTasks: 2,
	})
	if _, err := pager.Refresh(context.Background(), nil); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("Refresh(nil) = %v", err)
	}

	sentinel := errors.New("provider failed")
	if _, err := pager.Refresh(context.Background(), fragmentProviderFunc(
		func(context.Context, FragmentRequest) (FragmentResult, error) {
			return FragmentResult{}, sentinel
		},
	)); !errors.Is(err, sentinel) {
		t.Fatalf("Refresh(provider error) = %v", err)
	}
	if generation := pager.Stats().Generation; generation != 0 {
		t.Fatalf("generation after provider error = %d", generation)
	}

	started := make(chan FragmentRequest, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
				// Calling back into Pager must not deadlock: providers run
				// without a Pager lock.
				if stats := pager.Stats(); stats.Generation != request.BaseGeneration {
					return FragmentResult{}, errors.New("provider observed inconsistent generation")
				}
				started <- request
				<-release
				return FragmentResult{
					IndexedThrough: 12, Complete: true,
					Fragments: completePublication(0).Fragments,
				}, nil
			},
		))
		done <- err
	}()
	request := waitForRequest(t, started)
	if request.Revision != 41 || request.BaseGeneration != 0 ||
		request.ByteLength != 12 || request.MaxFragments != 8 {
		t.Fatalf("FragmentRequest = %+v", request)
	}

	if _, err := pager.Publish(context.Background(), completePublication(0)); err != nil {
		t.Fatalf("winning Publish = %v", err)
	}
	close(release)
	if err := waitForError(t, done, "losing Refresh"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("losing Refresh = %v", err)
	}
	if stats := pager.Stats(); stats.Generation != 1 || stats.Fragments != 3 {
		t.Fatalf("winning state = %+v", stats)
	}
}

func TestProviderTaskLimitAndCloseBarrier(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcdefghijkl"), 41, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	refreshDone := make(chan error, 1)
	go func() {
		_, err := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(context.Context, FragmentRequest) (FragmentResult, error) {
				close(started)
				<-release
				return FragmentResult{
					IndexedThrough: 12, Complete: true,
					Fragments: completePublication(0).Fragments,
				}, nil
			},
		))
		refreshDone <- err
	}()
	waitForSignal(t, started, "blocked FragmentProvider")
	if _, err := pager.Publish(context.Background(), completePublication(0)); !errors.Is(err, ErrBusy) {
		t.Fatalf("Publish while provider owns sole task = %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- pager.Close()
	}()
	deadline := time.Now().Add(5 * time.Second)
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
		runtime.Gosched()
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before provider retired: %v", err)
	default:
	}
	close(release)
	if err := waitForError(t, refreshDone, "Refresh retirement"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Refresh after concurrent Close = %v", err)
	}
	if err := waitForError(t, closeDone, "Close barrier"); err != nil {
		t.Fatalf("Close = %v", err)
	}
	var providerCalled atomic.Bool
	if _, err := pager.Refresh(context.Background(), fragmentProviderFunc(
		func(context.Context, FragmentRequest) (FragmentResult, error) {
			providerCalled.Store(true)
			return FragmentResult{}, nil
		},
	)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Refresh after Close = %v", err)
	}
	if providerCalled.Load() {
		t.Fatal("provider called after Pager.Close")
	}
}

func TestPublishEmptyDocument(t *testing.T) {
	pager := newPublicationPager(t, "", Options{})
	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 41, BaseGeneration: 0, IndexedThrough: 0, Complete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Generation != 1 || stats.Pages != 1 || stats.Fragments != 0 ||
		stats.IndexedThrough != 0 || !stats.Complete {
		t.Fatalf("Stats = %+v", stats)
	}
	window, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 41, Generation: 1, Offset: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(window.Pages) != 1 || len(window.Pages[0].Content) != 0 || !window.Pages[0].Indexed {
		t.Fatalf("empty Window = %+v", window)
	}
	if _, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 41, Generation: 1, Affinity: AffinityAfter,
	}); !errors.Is(err, ErrMeasureUnavailable) {
		t.Fatalf("WindowByMeasure(empty) = %v", err)
	}
}
