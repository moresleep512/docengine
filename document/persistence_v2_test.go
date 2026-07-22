package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	docsave "github.com/moresleep512/docengine/document/save"
	"github.com/moresleep512/docengine/recovery"
)

func TestOpenContextScansCompleteUTF8AndEOL(t *testing.T) {
	t.Run("invalid after former sample boundary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid")
		body := append(bytes.Repeat([]byte{'a'}, 64<<10), 0xff)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, OpenOptions{}); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("Open = %v", err)
		}
	})

	t.Run("rune crosses scan buffer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "split")
		body := append(bytes.Repeat([]byte{'a'}, scanBufferSize-1), []byte("界\r\nlast\n")...)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		session, err := Open(path, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		metadata := session.Metadata()
		if metadata.ByteLength != int64(len(body)) || metadata.EOL != EOLMixed {
			t.Fatalf("metadata = %+v", metadata)
		}
	})

	for _, test := range []struct {
		name    string
		body    []byte
		hasBOM  bool
		logical int64
	}{
		{name: "empty", body: nil},
		{name: "bom only", body: []byte{0xef, 0xbb, 0xbf}, hasBOM: true},
		{name: "short valid", body: []byte("é"), logical: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "doc")
			if err := os.WriteFile(path, test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := Open(path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if metadata := session.Metadata(); metadata.HasBOM != test.hasBOM || metadata.ByteLength != test.logical {
				t.Fatalf("metadata = %+v", metadata)
			}
		})
	}

	t.Run("incomplete final rune", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "incomplete")
		if err := os.WriteFile(path, []byte{0xe7, 0x95}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, OpenOptions{}); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("Open = %v", err)
		}
	})

	t.Run("invalid three-byte probe", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid-probe")
		if err := os.WriteFile(path, []byte{0xff, 'a', 'b'}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, OpenOptions{}); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("Open = %v", err)
		}
	})
}

func TestOpenContextCancellationCleansResources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, scanBufferSize*3), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &countingCancelContext{cancelAt: 4}
	options := OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session")}
	if _, err := OpenContext(ctx, path, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenContext = %v", err)
	}
	if _, err := os.Stat(options.SessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session directory exists after cancellation: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenContext(canceled, path, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled OpenContext = %v", err)
	}
}

func TestOpenRejectsChangeAtEndOfScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changing")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'a'}, scanBufferSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := systemSessionOperations
	operations.stat = func(statPath string) (os.FileInfo, error) {
		if err := os.WriteFile(statPath, bytes.Repeat([]byte{'b'}, scanBufferSize+1), 0o600); err != nil {
			return nil, err
		}
		return os.Stat(statPath)
	}
	session, err := openSessionContext(context.Background(), path, OpenOptions{}, operations)
	if session != nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	if !errors.Is(err, ErrExternalChange) {
		t.Fatalf("openSessionContext = %v", err)
	}
}

func TestOpenRejectsMetadataPreservingChangeAtEndOfScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changing-same-metadata")
	original := bytes.Repeat([]byte{'a'}, scanBufferSize+1)
	changed := bytes.Repeat([]byte{'b'}, len(original))
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	operations := systemSessionOperations
	operations.stat = func(statPath string) (os.FileInfo, error) {
		if err := os.WriteFile(statPath, changed, initial.Mode()); err != nil {
			return nil, err
		}
		if err := os.Chtimes(statPath, initial.ModTime(), initial.ModTime()); err != nil {
			return nil, err
		}
		return os.Stat(statPath)
	}
	session, err := openSessionContext(context.Background(), path, OpenOptions{}, operations)
	if session != nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	if !errors.Is(err, ErrExternalChange) {
		t.Fatalf("metadata-preserving openSessionContext = %v", err)
	}
}

func TestSessionPinsResolvedSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	link := filepath.Join(dir, "current.txt")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	session, err := Open(link, OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	requested, _ := filepath.Abs(link)
	resolved, err := resolvePath(first)
	if err != nil {
		t.Fatal(err)
	}
	if metadata := session.Metadata(); !samePath(metadata.Path, requested) || !samePath(metadata.ResolvedPath, resolved) {
		t.Fatalf("metadata paths = %+v", metadata)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 5, Insert: "saved"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	firstBody, _ := os.ReadFile(first)
	secondBody, _ := os.ReadFile(second)
	if string(firstBody) != "saved" || string(secondBody) != "second" {
		t.Fatalf("targets = (%q, %q)", firstBody, secondBody)
	}
}

func TestOpenUsesResolvedPathForRecoveryFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "real-document")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := recoveryFingerprintForTest(t, path)
	recoveryDir := filepath.Join(dir, "recovery")
	journalPath := filepath.Join(recoveryDir, journalPrefix(fingerprint)+".alias.docengine-journal-v2")
	journal, _, err := recovery.Open(journalPath, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Start: 3, Inserted: []byte("!")}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	requested := filepath.Join(dir, "REQUESTED-ALIAS")
	resolved, err := resolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	operations := systemSessionOperations
	operations.absolutePath = func(value string) (string, error) {
		if value == "alias" {
			return requested, nil
		}
		return filepath.Abs(value)
	}
	operations.evalSymlinks = func(value string) (string, error) {
		if value != requested {
			t.Fatalf("resolved unexpected request path %q", value)
		}
		return resolved, nil
	}
	session, err := openSessionContext(context.Background(), "alias", OpenOptions{
		RecoveryDir: recoveryDir,
		SessionDir:  filepath.Join(dir, "session"),
	}, operations)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if metadata := session.Metadata(); !metadata.Recovered || !samePath(metadata.Path, requested) || !samePath(metadata.ResolvedPath, resolved) {
		t.Fatalf("metadata = %+v", metadata)
	}
	body := make([]byte, 4)
	if n, err := session.ReadAt(body, 0); n != len(body) || err != nil || string(body) != "abc!" {
		t.Fatalf("ReadAt = (%d, %v, %q)", n, err, body)
	}
}

func TestSaveStrongIdentityDetectsMetadataPreservingChange(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: " local"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("evil"), info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Save(); !errors.Is(err, ErrExternalChange) {
		t.Fatalf("Save = %v", err)
	}
	if body, _ := os.ReadFile(path); string(body) != "evil" {
		t.Fatalf("external body overwritten: %q", body)
	}
}

func TestSaveAcceptsTimestampOnlyChangeWhenContentMatches(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "base!" {
		t.Fatalf("saved body = %q", body)
	}
}

func TestSaveTimestampQuickCheckFailures(t *testing.T) {
	t.Run("scan error", func(t *testing.T) {
		session, path, _ := openAtomicTestSession(t, "base")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := info.ModTime().Add(time.Hour)
		if err := os.Chtimes(path, changed, changed); err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("identity scan")
		session.operations.openBase = func(string) (*os.File, error) { return nil, sentinel }
		if _, err := session.Save(); !errors.Is(err, sentinel) {
			t.Fatalf("Save = %v", err)
		}
	})

	t.Run("content mismatch", func(t *testing.T) {
		session, path, _ := openAtomicTestSession(t, "base")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("evil"), info.Mode()); err != nil {
			t.Fatal(err)
		}
		changed := info.ModTime().Add(time.Hour)
		if err := os.Chtimes(path, changed, changed); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Save(); !errors.Is(err, ErrExternalChange) {
			t.Fatalf("Save = %v", err)
		}
	})
}

func TestCommittedDurabilityErrorCanBeRetried(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("directory sync")
	session.operations.atomicChecked = func(path string, mode os.FileMode, prefix []byte, write func(io.Writer) (int64, error), check func() error) (int64, error) {
		total, err := docsave.AtomicChecked(path, mode, prefix, write, check)
		if err != nil {
			return total, err
		}
		return total, &docsave.DurabilityError{Path: path, Err: sentinel}
	}
	metadata, err := session.Save()
	var durability *docsave.DurabilityError
	if !errors.As(err, &durability) || !metadata.DurabilityUncertain || metadata.Dirty || metadata.CommittedRevision != 1 {
		t.Fatalf("Save = (%+v, %v)", metadata, err)
	}
	if body, _ := os.ReadFile(path); string(body) != "base!" {
		t.Fatalf("committed body = %q", body)
	}
	calls := 0
	session.operations.syncParent = func(string) error { calls++; return &docsave.DurabilityError{Path: path, Err: sentinel} }
	if metadata, err = session.Save(); !errors.As(err, &durability) || !metadata.DurabilityUncertain {
		t.Fatalf("failed retry = (%+v, %v)", metadata, err)
	}
	session.operations.syncParent = func(string) error { calls++; return nil }
	if metadata, err = session.Save(); err != nil || metadata.DurabilityUncertain || calls != 2 {
		t.Fatalf("successful retry = (%+v, %v), calls=%d", metadata, err, calls)
	}
}

func TestPostCommitRebindFailureMakesSessionReadOnly(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reopen committed base")
	originalOpen := session.operations.openBase
	calls := 0
	session.operations.openBase = func(path string) (*os.File, error) {
		calls++
		if calls == 2 {
			return nil, sentinel
		}
		return originalOpen(path)
	}
	metadata, err := session.Save()
	if !errors.Is(err, ErrFaulted) || !errors.Is(err, sentinel) || !metadata.PersistenceFaulted || metadata.CommittedRevision != 1 || metadata.Dirty {
		t.Fatalf("Save = (%+v, %v), open calls=%d", metadata, err, calls)
	}
	if !errors.Is(session.Fault(), sentinel) {
		t.Fatalf("Fault = %v", session.Fault())
	}
	if body, _ := os.ReadFile(path); string(body) != "base!" {
		t.Fatalf("disk body = %q", body)
	}
	buffer := make([]byte, metadata.ByteLength)
	if n, readErr := session.ReadAt(buffer, 0); readErr != nil || n != len(buffer) || string(buffer) != "base!" {
		t.Fatalf("ReadAt = (%d, %v, %q)", n, readErr, buffer)
	}
	_, lease, snapshotErr := session.Snapshot()
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 5, Insert: "x"}}); !errors.Is(err, ErrFaulted) || !errors.Is(err, sentinel) {
		t.Fatalf("ApplyBatch = %v", err)
	}
	if _, err := session.Undo(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("Undo = %v", err)
	}
	if _, err := session.Redo(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("Redo = %v", err)
	}
	if _, err := session.Save(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("second Save = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func samePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	return left == right || strings.EqualFold(left, right)
}
