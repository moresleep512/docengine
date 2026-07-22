package document

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"

	docsave "github.com/moresleep512/docengine/document/save"
	"github.com/moresleep512/docengine/document/store"
)

func TestSessionPersistenceEventsSuccessFailureAndCleanNoop(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "a")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		subscription, err := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := session.Save()
		if err != nil {
			t.Fatal(err)
		}
		started := receiveEvent(t, subscription.Events())
		progress := receiveEvent(t, subscription.Events())
		saved := receiveEvent(t, subscription.Events())
		if started.Kind != EventSaveStarted || progress.Kind != EventSaveProgress || saved.Kind != EventSaved ||
			started.Persistence.OperationID == 0 || progress.Persistence.OperationID != started.Persistence.OperationID ||
			saved.Persistence.OperationID != started.Persistence.OperationID || started.Persistence.TargetRevision != 1 ||
			progress.Persistence.CompletedBytes != 2 || progress.Persistence.TotalBytes != 2 || !saved.Persistence.Committed ||
			saved.Metadata != metadata || saved.Cause != nil {
			t.Fatalf("persistence events = (%+v, %+v, %+v)", started, progress, saved)
		}
		if _, err := session.Save(); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-subscription.Events():
			t.Fatalf("clean Save published %+v", event)
		default:
		}
		if err := errors.Join(subscription.Close(), session.Close()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("pre-commit failure", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "a")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 4, FutureOnly: true})
		sentinel := errors.New("stat")
		originalStat := session.operations.stat
		session.operations.stat = func(string) (os.FileInfo, error) { return nil, sentinel }
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("Save error = %v", err)
		}
		started, failed := receiveEvent(t, subscription.Events()), receiveEvent(t, subscription.Events())
		if started.Kind != EventSaveStarted || failed.Kind != EventSaveFailed || failed.Persistence.Committed || !errors.Is(failed.Cause, sentinel) {
			t.Fatalf("failed persistence events = (%+v, %+v)", started, failed)
		}
		session.operations.stat = originalStat
		if err := errors.Join(subscription.Close(), session.Close()); err != nil {
			t.Fatal(err)
		}
	})

}

func TestSessionPersistenceEventsPostCommitFaultAndDurabilityRetry(t *testing.T) {
	t.Run("post-commit fault", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "a")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
		sentinel := errors.New("new tree")
		session.operations.newTree = func(io.ReaderAt, store.Piece) (*store.Tree, error) { return nil, sentinel }
		if metadata, err := session.Save(); !errors.Is(err, ErrFaulted) || !errors.Is(err, sentinel) || !metadata.PersistenceFaulted {
			t.Fatalf("faulted Save = (%+v, %v)", metadata, err)
		}
		started := receiveEvent(t, subscription.Events())
		progress := receiveEvent(t, subscription.Events())
		failed := receiveEvent(t, subscription.Events())
		if started.Kind != EventSaveStarted || progress.Kind != EventSaveProgress || failed.Kind != EventSaveFailed ||
			!failed.Persistence.Committed || !failed.Metadata.PersistenceFaulted || !errors.Is(failed.Cause, sentinel) {
			t.Fatalf("post-commit events = (%+v, %+v, %+v)", started, progress, failed)
		}
		if err := errors.Join(subscription.Close(), session.Close()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("durability retry", func(t *testing.T) {
		session, path, _ := openAtomicTestSession(t, "a")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 16, FutureOnly: true})
		sentinel := errors.New("directory sync")
		session.operations.atomicChecked = func(path string, mode os.FileMode, prefix []byte, write func(io.Writer) (int64, error), check func() error) (int64, error) {
			total, err := docsave.AtomicChecked(path, mode, prefix, write, check)
			if err != nil {
				return total, err
			}
			return total, &docsave.DurabilityError{Path: path, Err: sentinel}
		}
		if metadata, err := session.Save(); !errors.Is(err, sentinel) || !metadata.DurabilityUncertain {
			t.Fatalf("uncertain Save = (%+v, %v)", metadata, err)
		}
		first := drainEvents(subscription.Events(), 3, t)
		if first[2].Kind != EventSaved || !first[2].Persistence.Committed || !errors.Is(first[2].Cause, sentinel) {
			t.Fatalf("uncertain events = %+v", first)
		}
		session.operations.syncParent = func(string) error { return &docsave.DurabilityError{Path: path, Err: sentinel} }
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("failed durability retry = %v", err)
		}
		failedRetry := drainEvents(subscription.Events(), 2, t)
		if failedRetry[0].Kind != EventSaveStarted || failedRetry[1].Kind != EventSaveFailed || !failedRetry[1].Persistence.Committed {
			t.Fatalf("failed retry events = %+v", failedRetry)
		}
		session.operations.syncParent = func(string) error { return nil }
		if metadata, err := session.Save(); err != nil || metadata.DurabilityUncertain {
			t.Fatalf("successful durability retry = (%+v, %v)", metadata, err)
		}
		successfulRetry := drainEvents(subscription.Events(), 2, t)
		if successfulRetry[0].Kind != EventSaveStarted || successfulRetry[1].Kind != EventSaved || !successfulRetry[1].Persistence.Committed {
			t.Fatalf("successful retry events = %+v", successfulRetry)
		}
		if err := errors.Join(subscription.Close(), session.Close()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSaveProgressWriterBoundaries(t *testing.T) {
	var reports []int64
	w := newSaveProgressWriter(&shortErrorWriter{}, 0, 10, func(value int64) { reports = append(reports, value) })
	if n, err := w.Write([]byte("abcd")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error write = (%d, %v)", n, err)
	}
	if len(reports) != 1 || reports[0] != 2 {
		t.Fatalf("error reports = %v", reports)
	}
	w.finish()
	w.finish()
	if len(reports) != 2 || reports[1] != 10 {
		t.Fatalf("finish reports = %v", reports)
	}

	reports = nil
	w = newSaveProgressWriter(io.Discard, 3, 5, func(value int64) { reports = append(reports, value) })
	if n, err := w.Write([]byte("overflow")); n != 8 || err != nil {
		t.Fatalf("capped write = (%d, %v)", n, err)
	}
	w.finish()
	if len(reports) != 1 || reports[0] != 5 {
		t.Fatalf("capped reports = %v", reports)
	}

	session, _, _ := openAtomicTestSession(t, "")
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 2, FutureOnly: true})
	session.mu.Lock()
	session.nextPersistenceID = math.MaxUint64
	progress, metadata := session.beginPersistenceLocked(0, 0)
	session.mu.Unlock()
	if progress.OperationID != 1 || metadata.ByteLength != 0 || receiveEvent(t, subscription.Events()).Persistence.OperationID != 1 {
		t.Fatalf("wrapped persistence operation = (%+v, %+v)", progress, metadata)
	}
	session.publishPersistenceEvent(EventSaveProgress, metadata, progress, nil)
	if receiveEvent(t, subscription.Events()).Kind != EventSaveProgress {
		t.Fatal("publishPersistenceEvent did not publish")
	}
	if err := errors.Join(subscription.Close(), session.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRejectsBOMLengthOverflowBeforeWriting(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "")
	huge, err := store.New(strings.NewReader(""), math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.tree = huge
	session.revision = 1
	session.dirty = true
	session.hasBOM = true
	session.mu.Unlock()
	if _, err := session.Save(); !errors.Is(err, store.ErrLengthOverflow) {
		t.Fatalf("overflowing Save = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

type shortErrorWriter struct{}

func (*shortErrorWriter) Write(value []byte) (int, error) {
	return len(value) / 2, io.ErrShortWrite
}

func drainEvents(events <-chan SessionEvent, count int, t testing.TB) []SessionEvent {
	t.Helper()
	result := make([]SessionEvent, count)
	for index := range result {
		result[index] = receiveEvent(t, events)
	}
	return result
}
