package virtual

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func TestCloseContextCancelsProviderAndCompletesSharedBarrier(t *testing.T) {
	source := newCoreTestSource([]byte("abcd"))
	var recorder progressRecorder
	pager, err := BuildOwned(context.Background(), source, 9, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		Observer: &recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(ctx context.Context, _ FragmentRequest) (FragmentResult, error) {
				close(entered)
				<-ctx.Done()
				return FragmentResult{}, ctx.Err()
			},
		))
		refreshDone <- refreshErr
	}()
	waitForSignal(t, entered, "Context-aware provider")
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitForError(t, refreshDone, "canceled Refresh"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Refresh after Close = %v", err)
	}
	if source.closeCount() != 1 {
		t.Fatalf("owned Source closes = %d", source.closeCount())
	}
	stats := pager.Stats()
	if stats.Closing || !stats.Closed || stats.ActiveTasks != 0 ||
		stats.ActiveInflightBytes != 0 {
		t.Fatalf("closed Stats = %+v", stats)
	}
	progress := recorder.snapshot()
	if len(progress) != 2 || progress[0].Stage != ProgressStarted ||
		progress[1].Stage != ProgressFailed ||
		progress[0].OperationID != progress[1].OperationID ||
		!errors.Is(progress[1].Cause, ErrClosed) {
		t.Fatalf("close progress = %+v", progress)
	}
}

func TestCloseContextTimeoutDoesNotAbandonCleanup(t *testing.T) {
	closeFailure := errors.New("owned close")
	source := newCoreTestSource([]byte("abcd"))
	source.closeErr = closeFailure
	pager, err := BuildOwned(context.Background(), source, 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
			func(context.Context, FragmentRequest) (FragmentResult, error) {
				close(entered)
				<-release
				return FragmentResult{IndexedThrough: 4, Complete: true}, nil
			},
		))
		refreshDone <- refreshErr
	}()
	waitForSignal(t, entered, "non-canceling provider")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := pager.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed CloseContext = %v", err)
	}
	stats := pager.Stats()
	if !stats.Closing || stats.Closed || stats.ActiveTasks != 1 {
		t.Fatalf("closing Stats = %+v", stats)
	}
	if _, err := pager.Publish(context.Background(), Publication{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish during closing = %v", err)
	}
	close(release)
	if err := waitForError(t, refreshDone, "Refresh after timeout"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Refresh = %v", err)
	}
	if err := pager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("final Close = %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := pager.CloseContext(canceled); !errors.Is(err, closeFailure) {
		t.Fatalf("completed CloseContext with canceled caller = %v", err)
	}
	if stats := pager.Stats(); stats.Closing || !stats.Closed || stats.ActiveTasks != 0 {
		t.Fatalf("completed Stats = %+v", stats)
	}
}

func TestCloseContextValidationDoesNotStartShutdown(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcd"), 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pager.CloseContext(nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil CloseContext = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pager.CloseContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled CloseContext = %v", err)
	}
	if stats := pager.Stats(); stats.Closing || stats.Closed {
		t.Fatalf("validation started shutdown: %+v", stats)
	}
	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 1, Offset: 0,
	}); err != nil {
		t.Fatalf("Pager unusable after rejected CloseContext: %v", err)
	}
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskAdmissionRechecksCancellationAndClosedInflight(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcd"), 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelAtLock := newCoreCancelAtContext(3)
	if _, _, err := pager.acquireTask(cancelAtLock); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation at locked recheck = %v", err)
	}
	if stats := pager.Stats(); stats.ActiveTasks != 0 {
		t.Fatalf("rejected task was admitted: %+v", stats)
	}
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pager.reserveInflight(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed reserveInflight = %v", err)
	}
}

func TestInflightByteBudgetRejectsConcurrentPayloads(t *testing.T) {
	source := &blockingReadSource{
		data: []byte("abcdefgh"), started: make(chan struct{}), release: make(chan struct{}),
	}
	pager, err := Build(context.Background(), source, 7, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: 2,
		MaximumInflightBytes: 8, DisableCache: true,
		Window: Budget{Bytes: 8, Pages: 2, Fragments: 1, Measure: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	source.enabled.Store(true)
	windowDone := make(chan error, 1)
	go func() {
		_, windowErr := pager.WindowByByte(context.Background(), ByteWindowRequest{
			Revision: 7, Offset: 0, After: 8,
		})
		windowDone <- windowErr
	}()
	waitForSignal(t, source.started, "inflight Window")
	if stats := pager.Stats(); stats.ActiveTasks != 1 ||
		stats.ActiveInflightBytes != 8 || stats.MaximumInflightBytes != 8 {
		t.Fatalf("active inflight Stats = %+v", stats)
	}
	if _, err := pager.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 7, Offset: 0,
	}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Window budget = %v", err)
	}
	if _, err := pager.ReadPage(context.Background(), coreTestKey(pager, 0)); !errors.Is(err, ErrBusy) {
		t.Fatalf("ReadPage budget = %v", err)
	}
	close(source.release)
	if err := waitForError(t, windowDone, "inflight Window"); err != nil {
		t.Fatal(err)
	}
	if stats := pager.Stats(); stats.ActiveTasks != 0 || stats.ActiveInflightBytes != 0 {
		t.Fatalf("released inflight Stats = %+v", stats)
	}
}

func TestCancellationAndInflightBoundariesAcrossReadKinds(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcdefgh"), 7, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		MaximumInflightBytes: 8,
		Window:               Budget{Bytes: 8, Pages: 2, Fragments: 2, Measure: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 7, IndexedThrough: 8, Complete: true,
		Fragments: []Fragment{{ID: "all", Start: 0, End: 8, Measure: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelDuringAdmission := newCoreCancelAtContext(3)
	if _, err := pager.ReadPage(cancelDuringAdmission, coreTestKey(pager, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadPage cancellation after capture = %v", err)
	}

	pager.mu.Lock()
	pager.activeInflightBytes = pager.options.maximumInflightBytes
	pager.mu.Unlock()
	if _, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 7, Generation: stats.Generation, ID: "all",
	}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Fragment Window inflight limit = %v", err)
	}
	if _, err := pager.WindowByMeasure(context.Background(), MeasureWindowRequest{
		Revision: 7, Generation: stats.Generation, Offset: 0, Affinity: AffinityAfter,
	}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Measure Window inflight limit = %v", err)
	}
	pager.mu.Lock()
	pager.activeInflightBytes = 0
	state := pager.state
	pager.mu.Unlock()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pager.readWindow(canceled, state, 0, 0, 0, 0, pager.options.window); !errors.Is(err, context.Canceled) {
		t.Fatalf("readWindow cancellation = %v", err)
	}
}

func TestPublishDetectsCloseAtValidationAndCommitBoundaries(t *testing.T) {
	closed, err := Build(context.Background(), testSource("abcd"), 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.publish(context.Background(), Publication{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed internal publish = %v", err)
	}

	source := &blockingReadSource{
		data: []byte("abcde"), started: make(chan struct{}), release: make(chan struct{}),
	}
	pager, err := Build(context.Background(), source, 2, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	source.enabled.Store(true)
	result := make(chan error, 1)
	go func() {
		_, publishErr := pager.publish(context.Background(), Publication{
			Revision: 2, IndexedThrough: 5, Complete: true,
			Fragments: []Fragment{{ID: "middle", Start: 1, End: 3, Measure: 1}},
		})
		result <- publishErr
	}()
	waitForSignal(t, source.started, "publication validation")
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	if err := waitForError(t, result, "publication commit"); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish closed before commit = %v", err)
	}
}

func TestProgressLifecycleProviderReportingAndFailures(t *testing.T) {
	var recorder progressRecorder
	var pager *Pager
	observer := ProgressObserverFunc(func(progress Progress) {
		recorder.ObserveVirtualProgress(progress)
		if pager != nil {
			_ = pager.Stats()
		}
	})
	var err error
	pager, err = Build(context.Background(), testSource("abcd"), 3, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	stats, err := pager.Publish(context.Background(), Publication{
		Revision: 3, IndexedThrough: 4, Complete: true,
		Fragments: []Fragment{{ID: "all", Start: 0, End: 4, Measure: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProgressLifecycle(t, recorder.take(), ProgressPublish, 0, stats.Generation, nil)

	if _, err := pager.Publish(context.Background(), Publication{
		Revision: 4, BaseGeneration: stats.Generation,
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("invalid Publish = %v", err)
	} else {
		assertProgressLifecycle(t, recorder.take(), ProgressPublish, stats.Generation, 0, ErrRevisionMismatch)
	}

	var lateReport func(FragmentProgress) error
	stats, err = pager.Refresh(context.Background(), fragmentProviderFunc(
		func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
			lateReport = request.Report
			for _, progress := range []FragmentProgress{
				{IndexedThrough: 2, Fragments: 1},
				{IndexedThrough: 2, Fragments: 1},
				{IndexedThrough: 4, Fragments: 2},
			} {
				if err := request.Report(progress); err != nil {
					return FragmentResult{}, err
				}
			}
			return FragmentResult{
				IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{
					{ID: "left", Start: 0, End: 2, Measure: 1},
					{ID: "right", Start: 2, End: 4, Measure: 1},
				},
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	progress := recorder.take()
	if len(progress) != 4 || progress[0].Stage != ProgressStarted ||
		progress[1].Stage != ProgressAdvanced || progress[1].IndexedThrough != 2 ||
		progress[2].Stage != ProgressAdvanced || progress[2].IndexedThrough != 4 ||
		progress[3].Stage != ProgressCompleted ||
		progress[0].OperationID != progress[3].OperationID ||
		progress[3].PublishedGeneration != stats.Generation {
		t.Fatalf("Refresh progress = %+v", progress)
	}
	if err := lateReport(FragmentProgress{IndexedThrough: 4, Fragments: 2}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("late Report = %v", err)
	}

	_, err = pager.Refresh(context.Background(), fragmentProviderFunc(
		func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
			if err := request.Report(FragmentProgress{IndexedThrough: 3, Fragments: 1}); err != nil {
				return FragmentResult{}, err
			}
			_ = request.Report(FragmentProgress{IndexedThrough: 2, Fragments: 1})
			return FragmentResult{
				IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{{ID: "all", Start: 0, End: 4}},
			}, nil
		},
	))
	if !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("ignored invalid Report = %v", err)
	}
	failed := recorder.take()
	if len(failed) != 3 || failed[0].Stage != ProgressStarted ||
		failed[1].Stage != ProgressAdvanced || failed[2].Stage != ProgressFailed ||
		!errors.Is(failed[2].Cause, ErrInvalidPublication) {
		t.Fatalf("invalid report progress = %+v", failed)
	}

	pager.mu.Lock()
	pager.nextOperationID = math.MaxUint64
	pager.mu.Unlock()
	if _, err := pager.Publish(context.Background(), Publication{
		Revision: 3, BaseGeneration: stats.Generation,
	}); !errors.Is(err, ErrOperationOverflow) {
		t.Fatalf("operation overflow = %v", err)
	}
	if _, err := pager.Refresh(context.Background(), fragmentProviderFunc(
		func(context.Context, FragmentRequest) (FragmentResult, error) {
			t.Fatal("provider called after operation identifier exhaustion")
			return FragmentResult{}, nil
		},
	)); !errors.Is(err, ErrOperationOverflow) {
		t.Fatalf("Refresh operation overflow = %v", err)
	}
	if progress := recorder.take(); len(progress) != 0 {
		t.Fatalf("overflow published progress = %+v", progress)
	}
}

func TestProgressInternalTerminalAndCancellationBoundaries(t *testing.T) {
	var recorder progressRecorder
	pager, err := Build(context.Background(), testSource("abcd"), 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, Observer: &recorder,
	})
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	operation, err := pager.startProgress(canceled, ProgressRefresh)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := operation.report(FragmentProgress{IndexedThrough: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Report = %v", err)
	}
	if err := operation.report(FragmentProgress{IndexedThrough: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sticky canceled Report = %v", err)
	}
	operation.finish(Stats{}, context.Canceled)
	operation.finish(Stats{}, nil)
	if progress := recorder.take(); len(progress) != 2 ||
		progress[0].Stage != ProgressStarted || progress[1].Stage != ProgressFailed {
		t.Fatalf("canceled operation progress = %+v", progress)
	}

	operation, err = pager.startProgress(context.Background(), ProgressRefresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := operation.report(FragmentProgress{IndexedThrough: 3, Fragments: 1}); err != nil {
		t.Fatal(err)
	}
	if err := operation.finishProvider(FragmentResult{
		IndexedThrough: 2,
		Fragments:      []Fragment{{ID: "only"}},
	}, nil); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("regressed final provider result = %v", err)
	}
	operation.finish(Stats{}, ErrInvalidPublication)
	if progress := recorder.take(); len(progress) != 3 ||
		progress[1].Stage != ProgressAdvanced || progress[2].Stage != ProgressFailed {
		t.Fatalf("regressed result progress = %+v", progress)
	}

	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pager.startProgress(context.Background(), ProgressRefresh); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed startProgress = %v", err)
	}
}

func TestRebuildInheritsPolicyLineageAndOwnsFailureCleanup(t *testing.T) {
	lineage := NewLineage()
	var recorder progressRecorder
	previous, err := Build(context.Background(), testSource("abcd"), 1, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		MaximumTasks: 2, MaximumInflightBytes: 8,
		DisableCache: true,
		Window:       Budget{Bytes: 8, Pages: 2, Fragments: 2, Measure: 2},
		Lineage:      lineage, Observer: &recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	if !previous.BelongsTo(lineage) || previous.BelongsTo(NewLineage()) || previous.BelongsTo(nil) {
		t.Fatal("previous Pager lineage mismatch")
	}

	owned := newCoreTestSource([]byte("wxyz"))
	rebuilt, err := RebuildOwned(context.Background(), owned, 2, previous, fragmentProviderFunc(
		func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
			if request.Revision != 2 || request.BaseGeneration != 0 || request.ByteLength != 4 {
				return FragmentResult{}, errors.New("unexpected rebuild request")
			}
			if err := request.Report(FragmentProgress{IndexedThrough: 4, Fragments: 1}); err != nil {
				return FragmentResult{}, err
			}
			return FragmentResult{
				IndexedThrough: 4, Complete: true,
				Fragments: []Fragment{{ID: "all", Start: 0, End: 4, Measure: 1}},
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	stats := rebuilt.Stats()
	if stats.Revision != 2 || stats.Generation != 1 ||
		stats.MaximumTasks != 2 || stats.MaximumInflightBytes != 8 ||
		stats.MaximumCacheBytes != 0 || stats.WindowBytes != 8 ||
		!rebuilt.BelongsTo(lineage) {
		t.Fatalf("rebuilt Stats/lineage = %+v", stats)
	}
	if err := rebuilt.Close(); err != nil || owned.closeCount() != 1 {
		t.Fatalf("rebuilt Close = %v, source closes=%d", err, owned.closeCount())
	}

	if _, err := Rebuild(context.Background(), testSource("x"), 3, nil, nil); !errors.Is(err, ErrInvalidPager) {
		t.Fatalf("nil previous Rebuild = %v", err)
	}
	plain, err := Rebuild(context.Background(), testSource("xy"), 2, previous, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Stats().Revision != 2 || plain.Stats().Generation != 0 {
		t.Fatalf("plain Rebuild Stats = %+v", plain.Stats())
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	invalidSource := newCoreTestSource([]byte("x"))
	invalidSource.lenHook = func() int64 { return -1 }
	if _, err := Rebuild(context.Background(), invalidSource, 3, previous, nil); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("invalid Source Rebuild = %v", err)
	}
	failedOwned := newCoreTestSource([]byte("x"))
	if _, err := RebuildOwned(context.Background(), failedOwned, 3, nil, nil); !errors.Is(err, ErrInvalidPager) ||
		failedOwned.closeCount() != 1 {
		t.Fatalf("nil previous RebuildOwned = %v, closes=%d", err, failedOwned.closeCount())
	}
	closedPrevious, err := Build(context.Background(), testSource("x"), 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := closedPrevious.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(context.Background(), testSource("x"), 2, closedPrevious, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed previous Rebuild = %v", err)
	}
	if _, err := RebuildOwned(context.Background(), nil, 3, previous, nil); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil Source RebuildOwned = %v", err)
	}
	closedOwned := newCoreTestSource([]byte("x"))
	if _, err := RebuildOwned(context.Background(), closedOwned, 3, closedPrevious, nil); !errors.Is(err, ErrClosed) ||
		closedOwned.closeCount() != 1 {
		t.Fatalf("closed previous RebuildOwned = %v, closes=%d", err, closedOwned.closeCount())
	}
	invalidOwned := newCoreTestSource([]byte("x"))
	invalidOwned.lenHook = func() int64 { return -1 }
	if _, err := RebuildOwned(context.Background(), invalidOwned, 3, previous, nil); !errors.Is(err, ErrInvalidSource) ||
		invalidOwned.closeCount() != 1 {
		t.Fatalf("invalid Source RebuildOwned = %v, closes=%d", err, invalidOwned.closeCount())
	}

	closeFailure := errors.New("rebuild owned close")
	providerFailure := errors.New("rebuild provider")
	failedOwned = newCoreTestSource([]byte("x"))
	failedOwned.closeErr = closeFailure
	if _, err := RebuildOwned(context.Background(), failedOwned, 3, previous, fragmentProviderFunc(
		func(context.Context, FragmentRequest) (FragmentResult, error) {
			return FragmentResult{}, providerFailure
		},
	)); !errors.Is(err, providerFailure) || !errors.Is(err, closeFailure) ||
		failedOwned.closeCount() != 1 {
		t.Fatalf("provider failure cleanup = %v, closes=%d", err, failedOwned.closeCount())
	}
}

type progressRecorder struct {
	mu     sync.Mutex
	values []Progress
}

func (r *progressRecorder) ObserveVirtualProgress(progress Progress) {
	r.mu.Lock()
	r.values = append(r.values, progress)
	r.mu.Unlock()
}

func (r *progressRecorder) snapshot() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Progress(nil), r.values...)
}

func (r *progressRecorder) take() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := append([]Progress(nil), r.values...)
	r.values = nil
	return values
}

func assertProgressLifecycle(t testing.TB, progress []Progress, kind ProgressKind, base, published uint64, cause error) {
	t.Helper()
	if len(progress) != 2 || progress[0].Kind != kind ||
		progress[0].Stage != ProgressStarted || progress[0].BaseGeneration != base ||
		progress[0].OperationID == 0 || progress[0].OperationID != progress[1].OperationID {
		t.Fatalf("progress lifecycle = %+v", progress)
	}
	if cause == nil {
		if progress[1].Stage != ProgressCompleted || !progress[1].Published ||
			progress[1].PublishedGeneration != published || progress[1].Cause != nil {
			t.Fatalf("completed progress = %+v", progress[1])
		}
		return
	}
	if progress[1].Stage != ProgressFailed || progress[1].Published ||
		!errors.Is(progress[1].Cause, cause) {
		t.Fatalf("failed progress = %+v", progress[1])
	}
}
