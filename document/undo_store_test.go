package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestUndoStoreLifecycleQuotaAndErrors(t *testing.T) {
	store, err := openUndoStore("", DefaultUndoBytes)
	if err != nil || store != nil {
		t.Fatalf("empty open = (%v, %v)", store, err)
	}
	if _, err := store.append([]byte("x")); !errors.Is(err, errUndoQuota) {
		t.Fatalf("nil append error = %v", err)
	}
	if value, err := store.read(textRef{length: 1}); !errors.Is(err, errUndoQuota) || value != "" {
		t.Fatalf("nil read = (%q, %v)", value, err)
	}
	if err := store.reset(); err != nil {
		t.Fatalf("nil reset: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}

	dir := t.TempDir()
	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := store.append(nil); err != nil || ref != (textRef{}) {
		t.Fatalf("empty append = (%+v, %v)", ref, err)
	}
	store.quota = 3
	ref, err := store.append([]byte("abc"))
	if err != nil || ref != (textRef{length: 3}) {
		t.Fatalf("append = (%+v, %v)", ref, err)
	}
	if _, err := store.append([]byte("d")); !errors.Is(err, errUndoQuota) {
		t.Fatalf("quota error = %v", err)
	}
	if value, err := store.read(textRef{}); err != nil || value != "" {
		t.Fatalf("empty read = (%q, %v)", value, err)
	}
	if value, err := store.read(ref); err != nil || value != "abc" {
		t.Fatalf("read = (%q, %v)", value, err)
	}
	if _, err := store.read(textRef{offset: 99, length: 1}); err == nil {
		t.Fatal("expected out-of-range read error")
	}
	if err := store.reset(); err != nil || store.size != 0 {
		t.Fatalf("reset = %v, size=%d", err, store.size)
	}
	path := store.path
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undo file remains after close: %v", err)
	}
	if _, err := store.append([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed append error = %v", err)
	}
	if err := store.reset(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed reset error = %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOpenUndoStoreFilesystemFailures(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openUndoStore(filepath.Join(parentFile, "session"), DefaultUndoBytes); err == nil {
		t.Fatal("expected MkdirAll error")
	}
	dir := t.TempDir()
	sentinel := errors.New("create temp")
	operations := systemUndoStoreOperations
	operations.createTemp = func(string, string) (*os.File, error) { return nil, sentinel }
	if _, err := openUndoStoreWith(dir, DefaultUndoBytes, operations); !errors.Is(err, sentinel) {
		t.Fatalf("CreateTemp error = %v", err)
	}
}

func TestUndoStoreCleanupErrors(t *testing.T) {
	dir := t.TempDir()
	store, err := openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("remove")
	store.remove = func(string) error { return sentinel }
	if err := store.close(); !errors.Is(err, sentinel) {
		t.Fatalf("remove error = %v", err)
	}

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	store.remove = func(string) error { return os.ErrNotExist }
	if err := store.close(); err != nil {
		t.Fatalf("missing cleanup = %v", err)
	}
}

func TestUndoStoreRewriteLiveReferencesAndFailures(t *testing.T) {
	var nilStore *undoStore
	if mapping, err := nilStore.rewrite(nil); err != nil || len(mapping) != 0 || nilStore.bytes() != 0 {
		t.Fatalf("nil rewrite = (%v, %v)", mapping, err)
	}
	dir := t.TempDir()
	store, err := openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.append([]byte("dead"))
	second, _ := store.append([]byte("live"))
	third, _ := store.append([]byte("also-live"))
	oldPath := store.path
	mapping, err := store.rewrite([]textRef{{}, second, third, second})
	if err != nil {
		t.Fatal(err)
	}
	if store.bytes() != second.length+third.length || mapping[second].offset != 0 || mapping[third].offset != second.length || len(mapping) != 2 {
		t.Fatalf("rewrite mapping = %+v, bytes = %d", mapping, store.bytes())
	}
	for ref, want := range map[textRef]string{mapping[second]: "live", mapping[third]: "also-live"} {
		if got, err := store.read(ref); err != nil || got != want {
			t.Fatalf("rewritten read = (%q, %v), want %q", got, err, want)
		}
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired undo store remains: %v", err)
	}
	if mapping, err := store.rewrite(nil); err != nil || len(mapping) != 0 || store.bytes() != 0 {
		t.Fatalf("empty rewrite = (%v, %v), bytes=%d", mapping, err, store.bytes())
	}
	if _, err := store.rewrite([]textRef{{offset: -1, length: 1}}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("invalid rewrite ref = %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.rewrite(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed rewrite = %v", err)
	}

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := store.append([]byte("value"))
	sentinel := errors.New("create")
	originalCreate := store.create
	store.create = func(string, string) (*os.File, error) { return nil, sentinel }
	if _, err := store.rewrite([]textRef{ref}); !errors.Is(err, sentinel) {
		t.Fatalf("rewrite create error = %v", err)
	}
	store.create = originalCreate
	if err := store.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.rewrite([]textRef{ref}); err == nil {
		t.Fatal("expected rewrite copy error")
	}
	_ = store.close()

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.rewrite(nil); err == nil {
		t.Fatal("expected empty rewrite truncate error")
	}
	_ = store.close()

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ = store.append([]byte("x"))
	store.remove = func(string) error { return os.ErrNotExist }
	if mapping, err := store.rewrite([]textRef{ref}); err != nil || mapping[ref].length != 1 {
		t.Fatalf("rewrite missing retired file = (%+v, %v)", mapping, err)
	}
	store.remove = os.Remove
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ = store.append([]byte("x"))
	removeErr := errors.New("remove retired")
	store.remove = func(string) error { return removeErr }
	mapping, err = store.rewrite([]textRef{ref})
	if !errors.Is(err, removeErr) || mapping == nil || mapping[ref].length != 1 {
		t.Fatalf("rewrite cleanup error = (%+v, %v)", mapping, err)
	}
	store.remove = os.Remove
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
}

func TestUndoStoreRewriteCancellationPreservesActiveStore(t *testing.T) {
	dir := t.TempDir()
	store, err := openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	payload := bytes.Repeat([]byte("x"), 2*undoRewriteBufferBytes+17)
	ref, err := store.append(payload)
	if err != nil {
		t.Fatal(err)
	}
	beforePath, beforeSize := store.path, store.bytes()
	ctx, cancel := context.WithCancel(context.Background())
	var last, total int64
	mapping, err := store.rewriteContext(ctx, []textRef{ref}, func(completed, reportedTotal int64) {
		if completed < last || reportedTotal != int64(len(payload)) {
			t.Fatalf("rewrite progress = (%d/%d) after %d", completed, reportedTotal, last)
		}
		last, total = completed, reportedTotal
		if completed >= undoRewriteBufferBytes {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || mapping != nil {
		t.Fatalf("canceled rewrite = (%+v, %v)", mapping, err)
	}
	if total != int64(len(payload)) || last != undoRewriteBufferBytes {
		t.Fatalf("canceled progress = %d/%d", last, total)
	}
	if store.path != beforePath || store.bytes() != beforeSize {
		t.Fatalf("active store changed: path=%q size=%d", store.path, store.bytes())
	}
	if got, err := store.read(ref); err != nil || !bytes.Equal([]byte(got), payload) {
		t.Fatalf("active content after cancellation = (%d bytes, %v)", len(got), err)
	}
	files, err := filepath.Glob(filepath.Join(dir, ".docengine-undo-*.store"))
	if err != nil || len(files) != 1 || files[0] != beforePath {
		t.Fatalf("candidate cleanup = (%v, %v), active=%q", files, err, beforePath)
	}
}

func TestUndoStoreRewriteContextAndSizeBoundaries(t *testing.T) {
	var nilStore *undoStore
	reported := false
	if mapping, err := nilStore.rewriteContext(context.Background(), nil, func(completed, total int64) {
		reported = completed == 0 && total == 0
	}); err != nil || len(mapping) != 0 || !reported {
		t.Fatalf("nil store rewrite = (%+v, %v), reported=%v", mapping, err, reported)
	}
	if _, err := nilStore.rewriteContext(nil, nil, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := nilStore.rewriteContext(canceled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context = %v", err)
	}

	store, err := openUndoStore(t.TempDir(), DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	validationCanceled := &cancelAfterErrChecksContext{Context: context.Background(), failAt: 3}
	if _, err := store.rewriteContext(validationCanceled, []textRef{{}}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation cancellation = %v", err)
	}
	emptyCanceled := &cancelAfterErrChecksContext{Context: context.Background(), failAt: 3}
	if _, err := store.rewriteContext(emptyCanceled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty-store cancellation = %v", err)
	}
	ref, err := store.append([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	beforePath := store.path
	finalContext, cancelFinal := context.WithCancel(context.Background())
	mapping, err := store.rewriteContext(finalContext, []textRef{ref}, func(completed, total int64) {
		if completed == total && total > 0 {
			cancelFinal()
		}
	})
	if mapping != nil || !errors.Is(err, context.Canceled) || store.path != beforePath {
		t.Fatalf("final-check cancellation = (%+v, %v), path=%q", mapping, err, store.path)
	}
	store.size = math.MaxInt64
	refs := []textRef{
		{offset: 0, length: math.MaxInt64},
		{offset: 1, length: math.MaxInt64 - 1},
	}
	if mapping, err := store.rewriteContext(context.Background(), refs, nil); mapping != nil ||
		!errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("live-byte overflow = (%+v, %v)", mapping, err)
	}
}

func TestCopyUndoReferenceDefendsBrokenIOContracts(t *testing.T) {
	sentinel := errors.New("copy I/O")
	fullWriter := undoWriteFunc(func(value []byte) (int, error) { return len(value), nil })
	fullReader := undoReadAtFunc(func(value []byte, _ int64) (int, error) {
		copy(value, "x")
		return len(value), nil
	})
	cases := []struct {
		name        string
		reader      undoReferenceReader
		writer      undoReferenceWriter
		want        error
		wantReports int
	}{
		{
			name: "negative read count",
			reader: undoReadAtFunc(func([]byte, int64) (int, error) {
				return -1, nil
			}),
			writer: fullWriter, want: io.ErrUnexpectedEOF,
		},
		{
			name: "oversized read count",
			reader: undoReadAtFunc(func(value []byte, _ int64) (int, error) {
				return len(value) + 1, nil
			}),
			writer: fullWriter, want: io.ErrUnexpectedEOF,
		},
		{
			name:   "write error",
			reader: fullReader,
			writer: undoWriteFunc(func([]byte) (int, error) {
				return 0, sentinel
			}),
			want: sentinel, wantReports: 1,
		},
		{
			name:   "short write without error",
			reader: fullReader,
			writer: undoWriteFunc(func([]byte) (int, error) {
				return 0, nil
			}),
			want: io.ErrShortWrite, wantReports: 1,
		},
		{
			name: "read error",
			reader: undoReadAtFunc(func([]byte, int64) (int, error) {
				return 0, sentinel
			}),
			writer: fullWriter, want: sentinel,
		},
		{
			name: "zero progress",
			reader: undoReadAtFunc(func([]byte, int64) (int, error) {
				return 0, nil
			}),
			writer: fullWriter, want: io.ErrUnexpectedEOF,
		},
		{
			name: "terminal EOF with full read",
			reader: undoReadAtFunc(func(value []byte, _ int64) (int, error) {
				copy(value, "x")
				return len(value), io.EOF
			}),
			writer: fullWriter, wantReports: 1,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reports := 0
			err := copyUndoReference(context.Background(), test.writer, test.reader, textRef{length: 1}, make([]byte, 1), func(int64) {
				reports++
			})
			if !errors.Is(err, test.want) || reports != test.wantReports {
				t.Fatalf("copy error/reports = (%v, %d), want (%v, %d)", err, reports, test.want, test.wantReports)
			}
		})
	}
}

type cancelAfterErrChecksContext struct {
	context.Context
	checks int
	failAt int
}

func (c *cancelAfterErrChecksContext) Err() error {
	c.checks++
	if c.checks >= c.failAt {
		return context.Canceled
	}
	return nil
}

type undoReadAtFunc func([]byte, int64) (int, error)

func (f undoReadAtFunc) ReadAt(value []byte, offset int64) (int, error) {
	return f(value, offset)
}

type undoWriteFunc func([]byte) (int, error)

func (f undoWriteFunc) Write(value []byte) (int, error) {
	return f(value)
}
