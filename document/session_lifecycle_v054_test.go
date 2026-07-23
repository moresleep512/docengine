package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/coordinate"
	"github.com/moresleep512/docengine/document/virtual"
)

func TestSnapshotLeaseBudgetSpansConsumersAndGenerations(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "abc", 3, 0)
	defer session.Close()

	revision, lease, err := session.Snapshot()
	if err != nil || revision != 0 {
		t.Fatalf("Snapshot = (%d, %v)", revision, err)
	}
	index, err := session.CoordinateIndex(context.Background(), coordinate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pager, err := session.VirtualPager(context.Background(), virtual.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats := session.LifecycleStats(); stats.ActiveSnapshotLeases != 3 ||
		stats.PeakSnapshotLeases != 3 || stats.MaxSnapshotLeases != 3 {
		t.Fatalf("full lease stats = %+v", stats)
	}
	if _, _, err := session.Snapshot(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("fourth Snapshot error = %v", err)
	}

	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "d"}}); err != nil {
		t.Fatal(err)
	}
	if metadata, err := session.Save(); err != nil || metadata.CommittedRevision != 1 {
		t.Fatalf("Save = (%+v, %v)", metadata, err)
	}
	old := make([]byte, 3)
	if n, err := lease.ReadAt(old, 0); n != 3 || err != nil || string(old) != "abc" {
		t.Fatalf("old lease = (%d, %q, %v)", n, old, err)
	}
	if index.Stats().Revision != 0 || pager.Stats().Revision != 0 {
		t.Fatalf("derived revisions = (%+v, %+v)", index.Stats(), pager.Stats())
	}
	if _, _, err := session.Snapshot(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("post-save Snapshot error = %v", err)
	}

	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	newRevision, current, err := session.Snapshot()
	if err != nil || newRevision != 1 {
		t.Fatalf("current Snapshot = (%d, %v)", newRevision, err)
	}
	currentBytes := make([]byte, 4)
	if n, err := current.ReadAt(currentBytes, 0); n != 4 || err != nil || string(currentBytes) != "abcd" {
		t.Fatalf("current lease = (%d, %q, %v)", n, currentBytes, err)
	}
	if err := errors.Join(current.Close(), index.Close(), lease.Close()); err != nil {
		t.Fatal(err)
	}
	if stats := session.LifecycleStats(); stats.ActiveSnapshotLeases != 0 || stats.PeakSnapshotLeases != 3 {
		t.Fatalf("released lease stats = %+v", stats)
	}
}

func TestSnapshotLeaseBudgetIsAtomicUnderConcurrency(t *testing.T) {
	const (
		maximum = 8
		callers = 64
	)
	session, _, _ := openLifecycleTestSession(t, "content", maximum, 0)
	defer session.Close()

	type result struct {
		lease SnapshotLease
		err   error
	}
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan result, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, lease, err := session.Snapshot()
			results <- result{lease: lease, err: err}
			if lease != nil {
				<-release
				_ = lease.Close()
			}
		}()
	}
	close(start)
	var acquired int
	for range callers {
		result := <-results
		switch {
		case result.err == nil && result.lease != nil:
			acquired++
		case errors.Is(result.err, ErrLimitExceeded) && result.lease == nil:
		default:
			t.Fatalf("Snapshot result = (%T, %v)", result.lease, result.err)
		}
	}
	if acquired != maximum {
		t.Fatalf("acquired = %d, want %d", acquired, maximum)
	}
	if stats := session.LifecycleStats(); stats.ActiveSnapshotLeases != maximum || stats.PeakSnapshotLeases != maximum {
		t.Fatalf("concurrent lease stats = %+v", stats)
	}
	close(release)
	group.Wait()
	if stats := session.LifecycleStats(); stats.ActiveSnapshotLeases != 0 {
		t.Fatalf("released concurrent stats = %+v", stats)
	}
}

func TestCloseContextDeadlineContinuesSharedCleanup(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "abc", 1, 0)
	_, lease, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	removeFailure := errors.New("remove undo")
	session.undoStore.remove = func(string) error { return removeFailure }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := session.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed CloseContext = %v", err)
	}
	if stats := session.LifecycleStats(); !stats.Closing || stats.Closed || stats.ActiveSnapshotLeases != 1 {
		t.Fatalf("closing stats = %+v", stats)
	}
	if _, _, err := session.Snapshot(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot during close = %v", err)
	}
	if _, err := session.Save(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Save during close = %v", err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Insert: "x"}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("ApplyBatch during close = %v", err)
	}

	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); !errors.Is(err, removeFailure) {
		t.Fatalf("final Close = %v", err)
	}
	if err := session.CloseContext(context.Background()); !errors.Is(err, removeFailure) {
		t.Fatalf("repeated CloseContext = %v", err)
	}
	canceledAfterClose, cancelAfterClose := context.WithCancel(context.Background())
	cancelAfterClose()
	if err := session.CloseContext(canceledAfterClose); !errors.Is(err, removeFailure) {
		t.Fatalf("canceled CloseContext after completion = %v", err)
	}
	if stats := session.LifecycleStats(); stats.Closing || !stats.Closed || stats.ActiveSnapshotLeases != 0 {
		t.Fatalf("closed stats = %+v", stats)
	}
}

func TestCloseContextValidationDoesNotStartShutdown(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "abc", 1, 0)
	if err := session.CloseContext(nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil CloseContext = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.CloseContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled CloseContext = %v", err)
	}
	if stats := session.LifecycleStats(); stats.Closing || stats.Closed {
		t.Fatalf("premature close stats = %+v", stats)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, nil); err != nil {
		t.Fatalf("Session unusable after rejected CloseContext: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveContextCancellationWhileQueuedHasNoAttempt(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "abc", 4, 0)
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "d"}}); err != nil {
		t.Fatal(err)
	}
	subscription, err := session.Subscribe(SubscribeOptions{FutureOnly: true, Buffer: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			once.Do(func() {
				close(entered)
				<-proceed
			})
		}
	}
	first := make(chan error, 1)
	go func() {
		_, err := session.Save()
		first <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := session.SaveContext(ctx)
		second <- err
	}()
	waitLifecycleCondition(t, session, func(stats LifecycleStats) bool {
		return stats.SaveActive && stats.WaitingSaves == 1
	})
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued SaveContext = %v", err)
	}
	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Save = %v", err)
	}

	started := 0
	for {
		select {
		case event := <-subscription.Events():
			if event.Kind == EventSaveStarted {
				started++
			}
			if event.Kind == EventSaved {
				if started != 1 {
					t.Fatalf("save attempts = %d, events ended with %+v", started, event)
				}
				if stats := session.LifecycleStats(); stats.SaveActive || stats.WaitingSaves != 0 {
					t.Fatalf("settled save stats = %+v", stats)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for saved event")
		}
	}
}

func TestCloseWakesAllQueuedManualSaves(t *testing.T) {
	const callers = 32
	session, _, _ := openLifecycleTestSession(t, "abc", 4, 0)
	<-session.saveGate
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, err := session.Save()
			results <- err
		}()
	}
	waitLifecycleCondition(t, session, func(stats LifecycleStats) bool {
		return stats.WaitingSaves == callers
	})
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	for range callers {
		if err := <-results; !errors.Is(err, ErrClosed) {
			t.Fatalf("queued Save = %v", err)
		}
	}
	if stats := session.LifecycleStats(); stats.WaitingSaves != 0 || !stats.Closing {
		t.Fatalf("woken save stats = %+v", stats)
	}
	session.saveGate <- struct{}{}
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestSaveContextCancellationDuringStreamingIsPrecommit(t *testing.T) {
	session, path, _ := openLifecycleTestSession(t, "ab", 4, 0)
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "X"}}); err != nil {
		t.Fatal(err)
	}

	writer := newLifecycleBlockingWriter()
	var checked atomic.Bool
	atomicChecked := session.operations.atomicChecked
	session.operations.atomicChecked = func(_ string, _ os.FileMode, _ []byte, writeContent func(io.Writer) (int64, error), checkIdentity func() error) (int64, error) {
		total, err := writeContent(writer)
		if err != nil {
			return total, err
		}
		checked.Store(true)
		return total, checkIdentity()
	}
	ctx, cancel := context.WithCancel(context.Background())
	saved := make(chan error, 1)
	go func() {
		_, err := session.SaveContext(ctx)
		saved <- err
	}()
	<-writer.entered
	cancel()
	close(writer.proceed)
	if err := <-saved; !errors.Is(err, context.Canceled) {
		t.Fatalf("streaming SaveContext = %v", err)
	}
	if checked.Load() {
		t.Fatal("canceled stream reached final identity check")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "ab" {
		t.Fatalf("disk after canceled stream = %q, %v", content, err)
	}
	if metadata := session.Metadata(); metadata.CommittedRevision != 0 || !metadata.Dirty || metadata.PersistenceFaulted {
		t.Fatalf("metadata after canceled stream = %+v", metadata)
	}
	session.operations.atomicChecked = atomicChecked
	if metadata, err := session.Save(); err != nil || metadata.CommittedRevision != 1 {
		t.Fatalf("Save after cancellation = (%+v, %v)", metadata, err)
	}
}

func TestCloseCancelsAutomaticCheckpointAndPreservesRecovery(t *testing.T) {
	session, path, recoveryDir := openLifecycleTestSession(t, "a", 4, MinimumJournalBytes)
	writer := newLifecycleBlockingWriter()
	var checked atomic.Bool
	session.operations.atomicChecked = func(_ string, _ os.FileMode, _ []byte, writeContent func(io.Writer) (int64, error), checkIdentity func() error) (int64, error) {
		total, err := writeContent(writer)
		if err != nil {
			return total, err
		}
		checked.Store(true)
		return total, checkIdentity()
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	<-writer.entered
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	waitLifecycleCondition(t, session, func(stats LifecycleStats) bool { return stats.Closing })
	close(writer.proceed)
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
	if checked.Load() {
		t.Fatal("canceled automatic checkpoint reached final identity check")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "a" {
		t.Fatalf("disk after automatic cancellation = %q, %v", content, err)
	}

	reopened, err := Open(path, OpenOptions{
		RecoveryDir: recoveryDir,
		SessionDir:  filepath.Join(t.TempDir(), "reopened-session"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if metadata := reopened.Metadata(); !metadata.Recovered || metadata.Revision != 1 || metadata.ByteLength != 2 {
		t.Fatalf("reopened metadata = %+v", metadata)
	}
	content := make([]byte, 2)
	if n, err := reopened.ReadAt(content, 0); n != 2 || err != nil || string(content) != "ax" {
		t.Fatalf("reopened content = (%d, %q, %v)", n, content, err)
	}
}

func TestUndoRedoContextCancellationIsAtomic(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "a", 4, 0)
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 1, Insert: "b"},
		{Start: 2, Insert: "c"},
	}); err != nil {
		t.Fatal(err)
	}
	beforeUndo := session.Metadata()
	if _, err := session.UndoContext(nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil UndoContext = %v", err)
	}
	canceledUndo, cancelUndo := context.WithCancel(context.Background())
	cancelUndo()
	if _, err := session.UndoContext(canceledUndo); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled UndoContext = %v", err)
	}
	if _, err := session.UndoContext(&countingCancelContext{cancelAt: 3}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled UndoContext = %v", err)
	}
	if metadata := session.Metadata(); metadata != beforeUndo {
		t.Fatalf("UndoContext changed metadata: before=%+v after=%+v", beforeUndo, metadata)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	beforeRedo := session.Metadata()
	if _, err := session.RedoContext(nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil RedoContext = %v", err)
	}
	canceledRedo, cancelRedo := context.WithCancel(context.Background())
	cancelRedo()
	if _, err := session.RedoContext(canceledRedo); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled RedoContext = %v", err)
	}
	if _, err := session.RedoContext(&countingCancelContext{cancelAt: 3}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RedoContext = %v", err)
	}
	if metadata := session.Metadata(); metadata != beforeRedo {
		t.Fatalf("RedoContext changed metadata: before=%+v after=%+v", beforeRedo, metadata)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatal(err)
	}
}

type lifecycleBlockingWriter struct {
	once    sync.Once
	entered chan struct{}
	proceed chan struct{}
	buffer  bytes.Buffer
}

func newLifecycleBlockingWriter() *lifecycleBlockingWriter {
	return &lifecycleBlockingWriter{entered: make(chan struct{}), proceed: make(chan struct{})}
}

func (w *lifecycleBlockingWriter) Write(value []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.proceed
	})
	return w.buffer.Write(value)
}

func waitLifecycleCondition(t testing.TB, session *Session, condition func(LifecycleStats) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if condition(session.LifecycleStats()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle condition timed out: %+v", session.LifecycleStats())
		}
		time.Sleep(time.Millisecond)
	}
}

func openLifecycleTestSession(t testing.TB, content string, maxSnapshotLeases int, autoCheckpointBytes int64) (*Session, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	recoveryDir := filepath.Join(dir, "recovery")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir: recoveryDir,
		SessionDir:  filepath.Join(dir, "session"),
		Limits: SessionLimits{
			MaxSnapshotLeases: maxSnapshotLeases,
		},
		AutoCheckpointJournalBytes: autoCheckpointBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session, path, recoveryDir
}
