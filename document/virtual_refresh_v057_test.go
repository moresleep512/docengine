package document

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/moresleep512/docengine/document/virtual"
)

func TestSessionVirtualProgressAndCrossRevisionRefresh(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcd")
	t.Cleanup(func() { _ = session.Close() })
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 16, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	var userProgress documentProgressRecorder
	pager, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
		MaximumInflightBytes: 8,
		Window:               virtual.Budget{Bytes: 8, Pages: 2, Fragments: 2, Measure: 2},
		Observer:             &userProgress,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })

	stats, err := pager.Refresh(context.Background(), documentFragmentProviderFunc(
		func(_ context.Context, request virtual.FragmentRequest) (virtual.FragmentResult, error) {
			if err := request.Report(virtual.FragmentProgress{IndexedThrough: 2, Fragments: 1}); err != nil {
				return virtual.FragmentResult{}, err
			}
			return virtual.FragmentResult{
				IndexedThrough: 4, Complete: true,
				Fragments: []virtual.Fragment{{ID: "all", Start: 0, End: 4, Measure: 1}},
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	assertSessionVirtualEvents(t, subscription.Events(), stats.Generation, 0)
	if progress := userProgress.take(); len(progress) != 3 ||
		progress[0].Stage != virtual.ProgressStarted ||
		progress[1].Stage != virtual.ProgressAdvanced ||
		progress[2].Stage != virtual.ProgressCompleted {
		t.Fatalf("user progress = %+v", progress)
	}

	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{
		Start: 4, Insert: "x",
	}}); err != nil {
		t.Fatal(err)
	}
	if event := receiveEvent(t, subscription.Events()); event.Kind != EventChanged {
		t.Fatalf("edit event = %+v", event)
	}
	current, err := session.RefreshVirtualPager(context.Background(), pager, documentFragmentProviderFunc(
		func(_ context.Context, request virtual.FragmentRequest) (virtual.FragmentResult, error) {
			if request.Revision != 1 || request.BaseGeneration != 0 || request.ByteLength != 5 {
				return virtual.FragmentResult{}, errors.New("unexpected current revision request")
			}
			if err := request.Report(virtual.FragmentProgress{IndexedThrough: 5, Fragments: 1}); err != nil {
				return virtual.FragmentResult{}, err
			}
			return virtual.FragmentResult{
				IndexedThrough: 5, Complete: true,
				Fragments: []virtual.Fragment{{ID: "current", Start: 0, End: 5, Measure: 2}},
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	currentStats := current.Stats()
	if currentStats.Revision != 1 || currentStats.Generation != 1 ||
		currentStats.MaximumInflightBytes != 8 || currentStats.WindowBytes != 8 {
		t.Fatalf("current Pager Stats = %+v", currentStats)
	}
	assertSessionVirtualEvents(t, subscription.Events(), currentStats.Generation, 1)
	assertV057PagerText(t, pager, "abcd")
	assertV057PagerText(t, current, "abcdx")

	beforeLeases := session.LifecycleStats().ActiveSnapshotLeases
	sentinel := errors.New("provider refresh failed")
	if _, err := session.RefreshVirtualPager(context.Background(), current, documentFragmentProviderFunc(
		func(context.Context, virtual.FragmentRequest) (virtual.FragmentResult, error) {
			return virtual.FragmentResult{}, sentinel
		},
	)); !errors.Is(err, sentinel) {
		t.Fatalf("provider failure = %v", err)
	}
	if after := session.LifecycleStats().ActiveSnapshotLeases; after != beforeLeases {
		t.Fatalf("provider failure leaked lease: before=%d after=%d", beforeLeases, after)
	}
	failedStarted := receiveEvent(t, subscription.Events())
	failed := receiveEvent(t, subscription.Events())
	if failedStarted.Kind != EventVirtualizationStarted ||
		failed.Kind != EventVirtualizationFailed || !errors.Is(failed.Cause, sentinel) {
		t.Fatalf("provider failure events = (%+v, %+v)", failedStarted, failed)
	}

	fallback, err := session.RefreshVirtualPager(context.Background(), current, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fallback.Close() })
	if fallback.Stats().Revision != 1 || fallback.Stats().Generation != 0 {
		t.Fatalf("fallback Stats = %+v", fallback.Stats())
	}
	if err := fallback.Close(); err != nil {
		t.Fatal(err)
	}

	foreign, err := virtual.Build(context.Background(), virtualDocumentSource("abcdx"), 1, virtual.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.RefreshVirtualPager(context.Background(), foreign, nil); !errors.Is(err, virtual.ErrLineageMismatch) {
		t.Fatalf("foreign Pager refresh = %v", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RefreshVirtualPager(context.Background(), nil, nil); !errors.Is(err, virtual.ErrInvalidPager) {
		t.Fatalf("nil Pager refresh = %v", err)
	}

	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	beforeLeases = session.LifecycleStats().ActiveSnapshotLeases
	if _, err := session.RefreshVirtualPager(context.Background(), pager, nil); !errors.Is(err, virtual.ErrClosed) {
		t.Fatalf("closed Pager refresh = %v", err)
	}
	if after := session.LifecycleStats().ActiveSnapshotLeases; after != beforeLeases {
		t.Fatalf("closed Pager refresh leaked lease: before=%d after=%d", beforeLeases, after)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRefreshVirtualPagerContextAndSessionBoundaries(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	pager, err := session.VirtualPager(context.Background(), virtual.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.RefreshVirtualPager(nil, pager, nil); !errors.Is(err, virtual.ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.RefreshVirtualPager(canceled, pager, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RefreshVirtualPager(context.Background(), pager, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Session = %v", err)
	}
}

func TestSessionRefreshVirtualPagerPropagatesSnapshotFailure(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	pager, err := session.VirtualPager(context.Background(), virtual.Options{})
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.closed = true
	session.mu.Unlock()
	if _, err := session.RefreshVirtualPager(context.Background(), pager, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot failure = %v", err)
	}
	session.mu.Lock()
	session.closed = false
	session.mu.Unlock()
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSessionVirtualEvents(t testing.TB, events <-chan SessionEvent, generation, revision uint64) {
	t.Helper()
	started := receiveEvent(t, events)
	progress := receiveEvent(t, events)
	completed := receiveEvent(t, events)
	if started.Kind != EventVirtualizationStarted ||
		progress.Kind != EventVirtualizationProgress ||
		completed.Kind != EventVirtualizationCompleted ||
		started.Virtualization.OperationID == 0 ||
		started.Virtualization.OperationID != progress.Virtualization.OperationID ||
		started.Virtualization.OperationID != completed.Virtualization.OperationID ||
		completed.Virtualization.PublishedGeneration != generation ||
		completed.Virtualization.Revision != revision ||
		started.Metadata.Revision != revision || completed.Metadata.Revision != revision {
		t.Fatalf("virtual events = (%+v, %+v, %+v)", started, progress, completed)
	}
}

func assertV057PagerText(t testing.TB, pager *virtual.Pager, want string) {
	t.Helper()
	stats := pager.Stats()
	window, err := pager.WindowByByte(context.Background(), virtual.ByteWindowRequest{
		Revision: stats.Revision, Generation: stats.Generation,
		Offset: 0, After: int64(len(want)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, page := range window.Pages {
		got += string(page.Content)
	}
	if got != want {
		t.Fatalf("Pager text = %q, want %q", got, want)
	}
}

type documentFragmentProviderFunc func(context.Context, virtual.FragmentRequest) (virtual.FragmentResult, error)

func (f documentFragmentProviderFunc) Fragments(ctx context.Context, request virtual.FragmentRequest) (virtual.FragmentResult, error) {
	return f(ctx, request)
}

type documentProgressRecorder struct {
	mu     sync.Mutex
	values []virtual.Progress
}

func (r *documentProgressRecorder) ObserveVirtualProgress(progress virtual.Progress) {
	r.mu.Lock()
	r.values = append(r.values, progress)
	r.mu.Unlock()
}

func (r *documentProgressRecorder) take() []virtual.Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := append([]virtual.Progress(nil), r.values...)
	r.values = nil
	return values
}

type virtualDocumentSource string

func (s virtualDocumentSource) Len() int64 { return int64(len(s)) }

func (s virtualDocumentSource) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset >= int64(len(s)) {
		return 0, io.EOF
	}
	n := copy(buffer, string(s)[offset:])
	if n != len(buffer) {
		return n, io.EOF
	}
	return n, nil
}
