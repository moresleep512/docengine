package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/moresleep512/docengine/recovery"
)

func TestScanOpenedBaseFaultBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	sentinel := errors.New("injected")
	tests := []struct {
		name    string
		ctx     context.Context
		file    *scannerFile
		initial os.FileInfo
		stat    func(string) (os.FileInfo, error)
		want    error
	}{
		{name: "negative size", ctx: context.Background(), file: &scannerFile{}, initial: sizedFileInfo{FileInfo: info, size: -1}, stat: os.Stat, want: ErrExternalChange},
		{name: "read error", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, readErr: sentinel}, initial: info, stat: os.Stat, want: sentinel},
		{name: "zero read", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, zeroRead: true}, initial: info, stat: os.Stat, want: io.ErrUnexpectedEOF},
		{name: "canceled after empty scan", ctx: canceledContext(), file: &scannerFile{info: sizedFileInfo{FileInfo: info, size: 0}}, initial: sizedFileInfo{FileInfo: info, size: 0}, stat: os.Stat, want: context.Canceled},
		{name: "final stat", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, statErrors: map[int]error{1: sentinel}}, initial: info, stat: os.Stat, want: sentinel},
		{name: "path stat", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info}, initial: info, stat: func(string) (os.FileInfo, error) { return nil, sentinel }, want: sentinel},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := scanOpenedBase(test.ctx, test.file, path, test.initial, test.stat)
			if !errors.Is(err, test.want) {
				t.Fatalf("scanOpenedBase = %v", err)
			}
		})
	}
}

func TestScanDiskIdentityFaultBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	sentinel := errors.New("injected")
	operations := systemSessionOperations
	operations.openBase = func(string) (*os.File, error) { return nil, sentinel }
	if _, err := scanDiskIdentity(context.Background(), path, operations); !errors.Is(err, sentinel) {
		t.Fatalf("open error = %v", err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pipeInfo, _ := reader.Stat()
	_ = reader.Close()
	_ = writer.Close()
	tests := []struct {
		name string
		ctx  context.Context
		file *scannerFile
		stat func(string) (os.FileInfo, error)
		want error
	}{
		{name: "initial stat", ctx: context.Background(), file: &scannerFile{statErrors: map[int]error{1: sentinel}}, stat: os.Stat, want: sentinel},
		{name: "non regular", ctx: context.Background(), file: &scannerFile{info: pipeInfo}, stat: os.Stat, want: ErrExternalChange},
		{name: "canceled", ctx: canceledContext(), file: &scannerFile{body: []byte("abc"), info: info}, stat: os.Stat, want: context.Canceled},
		{name: "read error", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, readErr: sentinel}, stat: os.Stat, want: sentinel},
		{name: "zero read", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, zeroRead: true}, stat: os.Stat, want: io.ErrUnexpectedEOF},
		{name: "final stat", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info, statErrors: map[int]error{2: sentinel}}, stat: os.Stat, want: sentinel},
		{name: "path stat", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info}, stat: func(string) (os.FileInfo, error) { return nil, sentinel }, want: sentinel},
		{name: "changed", ctx: context.Background(), file: &scannerFile{body: []byte("abc"), info: info}, stat: func(string) (os.FileInfo, error) { return sizedFileInfo{FileInfo: info, size: 4}, nil }, want: ErrExternalChange},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := scanDiskIdentityOpened(test.ctx, path, test.file, test.stat)
			if !errors.Is(err, test.want) {
				t.Fatalf("scanDiskIdentityOpened = %v", err)
			}
		})
	}
}

func TestFileChangeCaptureFaultBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("change stamp")
	initialFailure := func(readStatFile, os.FileInfo) (fileChangeStamp, error) {
		return fileChangeStamp{}, sentinel
	}
	finalFailure := func() fileChangeCapture {
		calls := 0
		return func(readStatFile, os.FileInfo) (fileChangeStamp, error) {
			calls++
			if calls == 2 {
				return fileChangeStamp{}, sentinel
			}
			return fileChangeStamp{}, nil
		}
	}
	for _, test := range []struct {
		name    string
		capture fileChangeCapture
	}{
		{name: "initial", capture: initialFailure},
		{name: "final", capture: finalFailure()},
	} {
		t.Run("base "+test.name, func(t *testing.T) {
			file := &scannerFile{body: []byte("abc"), info: info}
			if _, err := scanOpenedBaseWithChange(context.Background(), file, path, info, os.Stat, test.capture); !errors.Is(err, sentinel) {
				t.Fatalf("scanOpenedBaseWithChange = %v", err)
			}
		})
	}
	for _, test := range []struct {
		name    string
		capture fileChangeCapture
	}{
		{name: "initial", capture: initialFailure},
		{name: "final", capture: finalFailure()},
	} {
		t.Run("identity "+test.name, func(t *testing.T) {
			file := &scannerFile{body: []byte("abc"), info: info}
			if _, err := scanDiskIdentityOpenedWithChange(context.Background(), path, file, os.Stat, test.capture); !errors.Is(err, sentinel) {
				t.Fatalf("scanDiskIdentityOpenedWithChange = %v", err)
			}
		})
	}
}

func TestScannersKeepSingleContentPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large-base")
	body := bytes.Repeat([]byte{'x'}, 2*scanBufferSize+1)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wantReads := 3
	base := &scannerFile{body: body, info: info}
	if _, err := scanOpenedBase(context.Background(), base, path, info, os.Stat); err != nil {
		t.Fatal(err)
	}
	if base.readCalls != wantReads {
		t.Fatalf("base scan ReadAt calls = %d, want one pass (%d)", base.readCalls, wantReads)
	}
	identity := &scannerFile{body: body, info: info}
	if _, err := scanDiskIdentityOpened(context.Background(), path, identity, os.Stat); err != nil {
		t.Fatal(err)
	}
	if identity.readCalls != wantReads {
		t.Fatalf("identity scan ReadAt calls = %d, want one pass (%d)", identity.readCalls, wantReads)
	}
}

func TestSameFileChangeStamp(t *testing.T) {
	unavailable := fileChangeStamp{}
	first := fileChangeStamp{first: 1, second: 2, available: true}
	same := fileChangeStamp{first: 1, second: 2, available: true}
	different := fileChangeStamp{first: 1, second: 3, available: true}
	if !sameFileChange(unavailable, unavailable) || sameFileChange(unavailable, first) ||
		!sameFileChange(first, same) || sameFileChange(first, different) {
		t.Fatal("file change stamp comparison is inconsistent")
	}
}

func TestRecoveryOpenErrorAndQuarantineFailure(t *testing.T) {
	sentinel := errors.New("injected")
	recoveryErr := &RecoveryOpenError{JournalPath: "old", QuarantinedPath: "new", Reason: "test", Err: sentinel}
	if recoveryErr.Error() == "" || !errors.Is(recoveryErr, sentinel) {
		t.Fatalf("RecoveryOpenError = %v", recoveryErr)
	}
	err := quarantineRecoveryWith("journal", "rename", sentinel, func(string, string) error { return errors.New("rename failed") })
	var typed *RecoveryOpenError
	if !errors.As(err, &typed) || typed.QuarantinedPath != "" || !errors.Is(err, sentinel) {
		t.Fatalf("quarantine error = %v", err)
	}
}

func TestOpenMatchingJournalInvalidHeaderAndOpenFailure(t *testing.T) {
	dir := t.TempDir()
	fingerprint := recovery.Fingerprint{PathHash: [32]byte{3}}
	path := filepath.Join(dir, journalPrefix(fingerprint)+".one.docengine-journal-v2")
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openMatchingJournal(dir, fingerprint); err == nil {
		t.Fatal("invalid header accepted")
	}
	quarantined, _ := filepath.Glob(filepath.Join(dir, "*.quarantine-invalid-header-*"))
	if len(quarantined) != 1 {
		t.Fatalf("quarantine = %v", quarantined)
	}
	created, _, err := recovery.Open(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_ = created.Close()
	sentinel := errors.New("open recovery")
	if _, _, err := openMatchingJournalWith(dir, fingerprint, func(string, recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
		return nil, recovery.ReplayResult{}, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("open failure = %v", err)
	}
}

func TestCommitPostStatSizeAndRebaseShortReadFaults(t *testing.T) {
	t.Run("committed size mismatch", func(t *testing.T) {
		session := openDirtyFaultSession(t)
		defer session.Close()
		calls := 0
		session.operations.stat = func(path string) (os.FileInfo, error) {
			calls++
			info, err := os.Stat(path)
			if err == nil && calls == 3 {
				return sizedFileInfo{FileInfo: info, size: info.Size() + 1}, nil
			}
			return info, err
		}
		metadata, err := session.Save()
		if !errors.Is(err, ErrFaulted) || !metadata.PersistenceFaulted {
			t.Fatalf("Save = (%+v, %v), calls=%d", metadata, err, calls)
		}
	})
	t.Run("rebase short read", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		session.operations.readRecovery = func(*recovery.Journal, []byte, int64) (int, error) { return 0, nil }
		close(proceed)
		if err := <-saved; !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Save = %v", err)
		}
		if session.Fault() != nil {
			t.Fatalf("pre-commit short read faulted Session: %v", session.Fault())
		}
	})
}

func TestClosedUndoAndRedo(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Undo = %v", err)
	}
	if _, err := session.Redo(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Redo = %v", err)
	}
}

func TestDetectEOLAllStyles(t *testing.T) {
	if detectEOL([]byte("a\r\nb")) != EOLCRLF || detectEOL([]byte("a\nb")) != EOLLF {
		t.Fatal("EOL detection failed")
	}
}

type scannerFile struct {
	body       []byte
	info       os.FileInfo
	readErr    error
	zeroRead   bool
	readCalls  int
	statCalls  int
	statErrors map[int]error
}

func (f *scannerFile) ReadAt(buffer []byte, offset int64) (int, error) {
	f.readCalls++
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.zeroRead {
		return 0, nil
	}
	return bytes.NewReader(f.body).ReadAt(buffer, offset)
}

func (f *scannerFile) Stat() (os.FileInfo, error) {
	f.statCalls++
	if err := f.statErrors[f.statCalls]; err != nil {
		return nil, err
	}
	return f.info, nil
}

type sizedFileInfo struct {
	os.FileInfo
	size int64
}

func (i sizedFileInfo) Size() int64 { return i.size }

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
