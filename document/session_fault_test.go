package document

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/recovery"
)

func TestOpenSessionInjectedStatTreeAndRepairFailures(t *testing.T) {
	sentinel := errors.New("injected")
	t.Run("absolute path failure", func(t *testing.T) {
		operations := systemSessionOperations
		operations.absolutePath = func(string) (string, error) { return "", sentinel }
		if _, err := openSession("ignored", OpenOptions{}, operations); !errors.Is(err, sentinel) {
			t.Fatalf("openSession error = %v", err)
		}
	})

	t.Run("resolved absolute path failure", func(t *testing.T) {
		operations := systemSessionOperations
		calls := 0
		operations.absolutePath = func(path string) (string, error) {
			calls++
			if calls == 2 {
				return "", sentinel
			}
			return path, nil
		}
		operations.evalSymlinks = func(path string) (string, error) { return path, nil }
		if _, err := openSession("ignored", OpenOptions{}, operations); !errors.Is(err, sentinel) {
			t.Fatalf("openSession error = %v", err)
		}
	})

	t.Run("base open failure", func(t *testing.T) {
		operations := systemSessionOperations
		operations.absolutePath = func(path string) (string, error) { return path, nil }
		operations.evalSymlinks = func(path string) (string, error) { return path, nil }
		operations.openBase = func(string) (*os.File, error) { return nil, sentinel }
		if _, err := openSession("ignored", OpenOptions{}, operations); !errors.Is(err, sentinel) {
			t.Fatalf("openSession error = %v", err)
		}
	})

	t.Run("non-regular base is rejected and closed", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = reader.Close()
			_ = writer.Close()
		})

		operations := systemSessionOperations
		operations.evalSymlinks = func(path string) (string, error) { return path, nil }
		operations.openBase = func(string) (*os.File, error) { return reader, nil }
		if _, err := openSession("ignored", OpenOptions{}, operations); err == nil || err.Error() != "document: path is not a regular file" {
			t.Fatalf("openSession error = %v", err)
		}
		if _, err := reader.Stat(); err == nil {
			t.Fatal("non-regular base was not closed")
		}
	})

	t.Run("base stat", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
		operations := systemSessionOperations
		operations.openBase = func(path string) (*os.File, error) {
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
			return file, nil
		}
		if _, err := openSession(path, OpenOptions{}, operations); err == nil {
			t.Fatal("expected base Stat error")
		}
	})

	t.Run("tree creation closes matching journal", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
		fingerprint := recoveryFingerprintForTest(t, path)
		recoveryDir := filepath.Join(dir, "recovery")
		journalPath := filepath.Join(recoveryDir, journalPrefix(fingerprint)+".test.docengine-journal-v2")
		journal, _, err := recovery.Open(journalPath, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		operations := systemSessionOperations
		operations.newTree = func(io.ReaderAt, store.Piece) (*store.Tree, error) { return nil, sentinel }
		if _, err := openSession(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")}, operations); !errors.Is(err, sentinel) {
			t.Fatalf("openSession error = %v", err)
		}
	})

	t.Run("truncated journal repair", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
		fingerprint := recoveryFingerprintForTest(t, path)
		recoveryDir := filepath.Join(dir, "recovery")
		journalPath := filepath.Join(recoveryDir, journalPrefix(fingerprint)+".test.docengine-journal-v2")
		journal, _, err := recovery.Open(journalPath, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		operations := systemSessionOperations
		operations.openRecovery = func(path string, fingerprint recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
			journal, replay, err := recovery.Open(path, fingerprint)
			if err == nil {
				_ = journal.Close()
			}
			return journal, replay, err
		}
		if _, err := openSession(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")}, operations); !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("openSession error = %v", err)
		}
	})
}

func recoveryFingerprintForTest(t testing.TB, path string) recovery.Fingerprint {
	t.Helper()
	resolved, err := resolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return recovery.FingerprintFor(resolved, int64(len(body)), sha256.Sum256(body))
}

func TestApplyBatchInjectedCloneAndInvariantFailuresAreAtomic(t *testing.T) {
	sentinel := errors.New("injected")
	t.Run("staging clone", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		session.operations.cloneTree = func(*store.Tree) (*store.Tree, error) { return nil, sentinel }
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); !errors.Is(err, sentinel) {
			t.Fatalf("ApplyBatch error = %v", err)
		}
		assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
	})

	t.Run("final clone", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		calls := 0
		session.operations.cloneTree = func(source *store.Tree) (*store.Tree, error) {
			calls++
			if calls == 2 {
				return nil, sentinel
			}
			return cloneDocumentTree(source), nil
		}
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); !errors.Is(err, sentinel) {
			t.Fatalf("ApplyBatch error = %v", err)
		}
		assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
		if info, err := os.Stat(session.journal.Path()); err != nil || info.Size() != 96 {
			t.Fatalf("journal after rollback = (%v, %v)", info, err)
		}
	})

	t.Run("staging length overflow", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		session.operations.cloneTree = func(*store.Tree) (*store.Tree, error) {
			return store.New(zeroReaderAt{}, math.MaxInt64)
		}
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "x"}}); !errors.Is(err, store.ErrLengthOverflow) {
			t.Fatalf("ApplyBatch error = %v", err)
		}
		assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
	})

	t.Run("final tree invariant", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		calls := 0
		session.operations.cloneTree = func(source *store.Tree) (*store.Tree, error) {
			calls++
			if calls == 2 {
				return store.New(nil, 0)
			}
			return cloneDocumentTree(source), nil
		}
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); !errors.Is(err, store.ErrInvalidRange) {
			t.Fatalf("ApplyBatch error = %v", err)
		}
		assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
	})
}

func TestCommitInjectedPostWriteAndRebaseFailures(t *testing.T) {
	sentinel := errors.New("injected")
	t.Run("post-write stat", func(t *testing.T) {
		session := openDirtyFaultSession(t)
		defer session.Close()
		calls := 0
		session.operations.stat = func(path string) (os.FileInfo, error) {
			calls++
			if calls == 3 {
				return nil, sentinel
			}
			return os.Stat(path)
		}
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("Save error = %v, stat calls=%d", err, calls)
		}
	})

	t.Run("new base open", func(t *testing.T) {
		session := openDirtyFaultSession(t)
		defer session.Close()
		session.operations.openBase = func(string) (*os.File, error) { return nil, sentinel }
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("Save error = %v", err)
		}
	})

	t.Run("new tree", func(t *testing.T) {
		session := openDirtyFaultSession(t)
		defer session.Close()
		session.operations.newTree = func(io.ReaderAt, store.Piece) (*store.Tree, error) { return nil, sentinel }
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("Save error = %v", err)
		}
	})

	t.Run("rebase source read", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		session.operations.readRecovery = func(*recovery.Journal, []byte, int64) (int, error) {
			return 0, recovery.ErrClosed
		}
		close(proceed)
		if err := <-saved; !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("Save error = %v", err)
		}
		if session.Fault() != nil {
			t.Fatalf("pre-commit rebase read faulted Session: %v", session.Fault())
		}
	})

	t.Run("rebase append", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		session.operations.openRecovery = func(path string, fingerprint recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
			journal, replay, err := recovery.Open(path, fingerprint)
			if err == nil {
				_ = journal.Close()
			}
			return journal, replay, err
		}
		close(proceed)
		if err := <-saved; !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("Save error = %v", err)
		}
	})

	t.Run("rebase tree invariant", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		session.operations.newTree = func(io.ReaderAt, store.Piece) (*store.Tree, error) { return store.New(nil, 0) }
		close(proceed)
		if err := <-saved; !errors.Is(err, store.ErrInvalidRange) {
			t.Fatalf("Save error = %v", err)
		}
	})
}

func openDirtyFaultSession(t testing.TB) *Session {
	t.Helper()
	session, _, _ := openAtomicTestSession(t, "abc")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	return session
}

func startConcurrentFaultSave(t testing.TB) (*Session, chan struct{}, chan error) {
	t.Helper()
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 1, Insert: "A"}}); err != nil {
		t.Fatal(err)
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(started)
			<-proceed
		}
	}
	saved := make(chan error, 1)
	go func() {
		_, err := session.Save()
		saved <- err
	}()
	<-started
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 1, Insert: "B"}}); err != nil {
		t.Fatal(err)
	}
	return session, proceed, saved
}

type zeroReaderAt struct{}

func (zeroReaderAt) ReadAt(buffer []byte, _ int64) (int, error) {
	clear(buffer)
	return len(buffer), nil
}
