package document

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/coordinate"
	"github.com/moresleep512/docengine/recovery"
)

func TestPreparedJournalRebaseCancellationAtEveryCheckpoint(t *testing.T) {
	session, _, recoveryDir := openLifecycleTestSession(t, "a", 4, 0)
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "c"}}); err != nil {
		t.Fatal(err)
	}
	baseline, err := filepathGlobJournal(recoveryDir)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("replacement"))
	fingerprint := recovery.FingerprintFor(session.resolvedPath, int64(len("replacement")), hash)
	for cancelAt := 1; cancelAt <= 18; cancelAt++ {
		session.mu.Lock()
		prepared, prepareErr := session.prepareJournalRebaseLocked(&countingCancelContext{cancelAt: cancelAt}, fingerprint, 0)
		session.mu.Unlock()
		if !errors.Is(prepareErr, context.Canceled) || prepared != nil {
			t.Fatalf("cancelAt %d = (%+v, %v)", cancelAt, prepared, prepareErr)
		}
		current, globErr := filepathGlobJournal(recoveryDir)
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(current) != len(baseline) {
			t.Fatalf("cancelAt %d leaked journal: baseline=%v current=%v", cancelAt, baseline, current)
		}
	}

	session.mu.Lock()
	prepared, err := session.prepareJournalRebaseLocked(context.Background(), fingerprint, 0)
	if err == nil {
		err = session.discardPreparedJournal(prepared, true)
	}
	session.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleRaceGuardsRejectShutdown(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		ctx := newLifecycleSignalContext(nil)
		session.mu.Lock()
		result := make(chan error, 1)
		go func() {
			_, err := session.ApplyBatch(ctx, 0, []ReplaceOperation{{Start: 1, Insert: "x"}})
			result <- err
		}()
		<-ctx.observed
		session.closed = true
		session.mu.Unlock()
		if err := <-result; !errors.Is(err, ErrClosed) {
			t.Fatalf("ApplyBatch = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("compact checkpoint", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		ctx := newErrSignalContext()
		session.mu.Lock()
		result := make(chan error, 1)
		go func() {
			_, err := session.Compact(ctx, CompactOptions{CheckpointJournal: true})
			result <- err
		}()
		<-ctx.observed
		session.closed = true
		session.mu.Unlock()
		if err := <-result; !errors.Is(err, ErrClosed) {
			t.Fatalf("checkpoint Compact = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("compact structural", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		ctx := newErrSignalContext()
		session.mu.Lock()
		result := make(chan error, 1)
		go func() {
			_, err := session.Compact(ctx, CompactOptions{})
			result <- err
		}()
		<-ctx.observed
		session.closed = true
		session.mu.Unlock()
		if err := <-result; !errors.Is(err, ErrClosed) {
			t.Fatalf("structural Compact = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	for _, operation := range []string{"undo", "redo"} {
		t.Run(operation, func(t *testing.T) {
			session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
			if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
				t.Fatal(err)
			}
			if operation == "redo" {
				if _, err := session.Undo(); err != nil {
					t.Fatal(err)
				}
			}
			ctx := newLifecycleSignalContext(nil)
			session.mu.Lock()
			result := make(chan error, 1)
			go func() {
				var err error
				if operation == "undo" {
					_, err = session.UndoContext(ctx)
				} else {
					_, err = session.RedoContext(ctx)
				}
				result <- err
			}()
			<-ctx.observed
			session.cancelLifecycle(ErrClosed)
			session.mu.Unlock()
			if err := <-result; !errors.Is(err, ErrClosed) {
				t.Fatalf("%s = %v", operation, err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoordinateLifecycleAndLeaseLimitGuards(t *testing.T) {
	session, _, _ := openLifecycleTestSession(t, "abc", 1, 0)
	defer session.Close()
	if _, err := session.CoordinateIndex(nil, coordinate.Options{}); !errors.Is(err, coordinate.ErrInvalidContext) {
		t.Fatalf("nil CoordinateIndex = %v", err)
	}
	previous, err := session.CoordinateIndex(context.Background(), coordinate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	identity, _ := coordinate.Identity(0, 3)
	if _, err := session.RebuildCoordinateIndex(context.Background(), previous, identity); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("RebuildCoordinateIndex at limit = %v", err)
	}
	if _, err := session.RefreshCoordinateIndex(context.Background(), previous); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("RefreshCoordinateIndex at limit = %v", err)
	}

	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err = session.CoordinateIndex(context.Background(), coordinate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newLifecycleSignalContext(nil)
	session.mu.Lock()
	refreshed := make(chan error, 1)
	go func() {
		_, err := session.RefreshCoordinateIndex(ctx, previous)
		refreshed <- err
	}()
	<-ctx.observed
	session.cancelLifecycle(ErrClosed)
	session.mu.Unlock()
	if err := <-refreshed; !errors.Is(err, ErrClosed) {
		t.Fatalf("RefreshCoordinateIndex during shutdown = %v", err)
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationContextAndSaveGateRaceBoundaries(t *testing.T) {
	t.Run("lifecycle changes during context construction", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		ctx := newLifecycleSignalContext(func() { session.cancelLifecycle(ErrClosed) })
		operationContext, finish, err := session.operationContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := contextError(operationContext); !errors.Is(err, ErrClosed) {
			t.Fatalf("operation context = %v", err)
		}
		finish()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("finished operation context", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		operationContext, finish, err := session.operationContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		finish()
		if err := operationContext.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("finished context = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lifecycle closes after waiter starts", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		<-session.saveGate
		result := make(chan error, 1)
		go func() { result <- session.acquireSave(context.Background()) }()
		waitLifecycleCondition(t, session, func(stats LifecycleStats) bool { return stats.WaitingSaves == 1 })
		session.cancelLifecycle(ErrClosed)
		session.saveGate <- struct{}{}
		if err := <-result; !errors.Is(err, ErrClosed) {
			t.Fatalf("acquireSave after shutdown = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("context changes after gate selection", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		ctx := &toggleErrorContext{}
		ctx.canceled.Store(true)
		if err := session.acquireSave(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("acquireSave canceled = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lifecycle changes after gate selection", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		lifecycle := session.lifecycleContext
		session.lifecycleContext = &countingCancelContext{cancelAt: 2}
		if err := session.acquireSave(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("acquireSave closed = %v", err)
		}
		session.lifecycleContext = lifecycle
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCommitContextCancellationCheckpoints(t *testing.T) {
	if session, _, _ := openLifecycleTestSession(t, "a", 1, 0); true {
		if _, err := session.CommitAtLeastContext(nil, 0); !errors.Is(err, ErrInvalidContext) {
			t.Fatalf("nil CommitAtLeastContext = %v", err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for cancelAt := 1; cancelAt <= 6; cancelAt++ {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		if _, err := session.CommitAtLeastContext(&countingCancelContext{cancelAt: cancelAt}, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelAt %d clean CommitAtLeastContext = %v", cancelAt, err)
		}
		if stats := session.LifecycleStats(); stats.SaveActive || stats.WaitingSaves != 0 {
			t.Fatalf("cancelAt %d stats = %+v", cancelAt, stats)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cancelAt := range []int{7, 8} {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		session.mu.Lock()
		session.durabilityUncertain = true
		session.mu.Unlock()
		if _, err := session.CommitAtLeastContext(&countingCancelContext{cancelAt: cancelAt}, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelAt %d durability retry = %v", cancelAt, err)
		}
		if !session.Metadata().DurabilityUncertain {
			t.Fatalf("cancelAt %d cleared durability uncertainty", cancelAt)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("internal closed and canceled guards", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		session.cancelLifecycle(ErrClosed)
		if _, err := session.commitAtLeast(context.Background(), 0); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed commitAtLeast = %v", err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		// A fresh Session is required because CancelCauseFunc is monotonic.
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		fresh, _, _ := openLifecycleTestSession(t, "a", 1, 0)
		if _, err := fresh.commitAtLeast(canceled, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled commitAtLeast = %v", err)
		}
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSaveFinalBoundaryCancellation(t *testing.T) {
	t.Run("before identity callback", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		session.operations.atomicChecked = func(_ string, _ os.FileMode, _ []byte, writeContent func(io.Writer) (int64, error), checkIdentity func() error) (int64, error) {
			total, err := writeContent(io.Discard)
			if err != nil {
				return total, err
			}
			cancel()
			return total, checkIdentity()
		}
		if _, err := session.SaveContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveContext = %v", err)
		}
	})

	t.Run("after identity callback locks", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		session.commitHook = func(stage string) {
			if stage == "identity" {
				cancel()
			}
		}
		if _, err := session.SaveContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveContext = %v", err)
		}
	})

	t.Run("after prepared journal directory sync", func(t *testing.T) {
		session, _, _ := openLifecycleTestSession(t, "a", 2, 0)
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		syncParent := session.operations.syncParent
		session.operations.syncParent = func(path string) error {
			err := syncParent(path)
			cancel()
			return err
		}
		if _, err := session.SaveContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveContext = %v", err)
		}
	})
}

type lifecycleSignalContext struct {
	observed chan struct{}
	once     sync.Once
	onDone   func()
}

type errSignalContext struct {
	observed chan struct{}
	once     sync.Once
}

func newErrSignalContext() *errSignalContext {
	return &errSignalContext{observed: make(chan struct{})}
}

func (c *errSignalContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *errSignalContext) Done() <-chan struct{}       { return nil }
func (c *errSignalContext) Value(any) any               { return nil }
func (c *errSignalContext) Err() error {
	c.once.Do(func() { close(c.observed) })
	return nil
}

func newLifecycleSignalContext(onDone func()) *lifecycleSignalContext {
	return &lifecycleSignalContext{observed: make(chan struct{}), onDone: onDone}
}

func (c *lifecycleSignalContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *lifecycleSignalContext) Value(any) any               { return nil }
func (c *lifecycleSignalContext) Err() error                  { return nil }
func (c *lifecycleSignalContext) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.observed)
		if c.onDone != nil {
			c.onDone()
		}
	})
	return nil
}

type toggleErrorContext struct {
	canceled atomic.Bool
}

func (c *toggleErrorContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *toggleErrorContext) Done() <-chan struct{}       { return nil }
func (c *toggleErrorContext) Value(any) any               { return nil }
func (c *toggleErrorContext) Err() error {
	if c.canceled.Load() {
		return context.Canceled
	}
	return nil
}

func filepathGlobJournal(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, "*.docengine-journal-v2"))
}
