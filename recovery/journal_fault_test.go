package recovery

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenJournalInjectedStatHeaderAndReplayFailures(t *testing.T) {
	sentinel := errors.New("injected")
	t.Run("stat", func(t *testing.T) {
		file := createFaultBase(t)
		fault := &faultJournalFile{base: file, statFaults: map[int]error{1: sentinel}}
		_, _, err := openJournal("journal", Fingerprint{}, injectedOpenOperations(fault))
		if !errors.Is(err, sentinel) || fault.closeCalls != 1 {
			t.Fatalf("openJournal = %v, closes=%d", err, fault.closeCalls)
		}
	})
	t.Run("header write", func(t *testing.T) {
		file := createFaultBase(t)
		fault := &faultJournalFile{base: file, writeAtErr: sentinel}
		_, _, err := openJournal("journal", Fingerprint{}, injectedOpenOperations(fault))
		if !errors.Is(err, sentinel) || fault.closeCalls != 1 {
			t.Fatalf("openJournal = %v, closes=%d", err, fault.closeCalls)
		}
	})
	t.Run("header sync", func(t *testing.T) {
		file := createFaultBase(t)
		fault := &faultJournalFile{base: file, syncErr: sentinel}
		_, _, err := openJournal("journal", Fingerprint{}, injectedOpenOperations(fault))
		if !errors.Is(err, sentinel) || fault.closeCalls != 1 {
			t.Fatalf("openJournal = %v, closes=%d", err, fault.closeCalls)
		}
	})
	t.Run("replay", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal")
		journal, _, err := Open(path, Fingerprint{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := journal.AppendReplace(1, 0, 0, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		fault := &faultJournalFile{base: file, statFaults: map[int]error{2: sentinel}}
		_, _, err = openJournal(path, Fingerprint{}, injectedOpenOperations(fault))
		if !errors.Is(err, sentinel) || fault.closeCalls != 1 {
			t.Fatalf("openJournal = %v, closes=%d", err, fault.closeCalls)
		}
	})
}

func TestAppendFrameInjectedHeaderAndPayloadWriteFailures(t *testing.T) {
	sentinel := errors.New("injected")
	tests := []struct {
		name   string
		faults map[int]faultIOResult
		want   error
	}{
		{name: "header error", faults: map[int]faultIOResult{1: {err: sentinel}}, want: sentinel},
		{name: "header short", faults: map[int]faultIOResult{1: {n: frameHeaderSize - 1}}, want: io.ErrShortWrite},
		{name: "payload error", faults: map[int]faultIOResult{2: {err: sentinel}}, want: sentinel},
		{name: "payload short", faults: map[int]faultIOResult{2: {n: 1}}, want: io.ErrShortWrite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := createFaultBase(t)
			fault := &faultJournalFile{base: file, writeFaults: test.faults}
			journal := &Journal{file: fault, path: "journal"}
			if _, err := journal.appendFrame(Frame{Kind: FrameReplace, Revision: 1}, []byte("payload")); !errors.Is(err, test.want) {
				t.Fatalf("appendFrame error = %v, want %v", err, test.want)
			}
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Fatalf("rollback size = %d, want 0", info.Size())
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayInjectedHeaderPayloadAndBatchMetadataReadFailures(t *testing.T) {
	sentinel := errors.New("injected")
	t.Run("frame header", func(t *testing.T) {
		path := writeReplayFixture(t, false)
		journal := openFaultJournal(t, path, &faultJournalFile{readFaults: map[int]error{1: sentinel}})
		defer journal.Close()
		if _, err := journal.Replay(); !errors.Is(err, sentinel) {
			t.Fatalf("Replay error = %v", err)
		}
	})
	t.Run("frame payload", func(t *testing.T) {
		path := writeReplayFixture(t, false)
		journal := openFaultJournal(t, path, &faultJournalFile{readFaults: map[int]error{2: sentinel}})
		defer journal.Close()
		if _, err := journal.Replay(); !errors.Is(err, sentinel) {
			t.Fatalf("Replay error = %v", err)
		}
	})
	t.Run("batch metadata", func(t *testing.T) {
		path := writeReplayFixture(t, true)
		journal := openFaultJournal(t, path, &faultJournalFile{readFaults: map[int]error{3: sentinel}})
		defer journal.Close()
		if _, err := journal.Replay(); !errors.Is(err, sentinel) {
			t.Fatalf("Replay error = %v", err)
		}
	})
}

func TestResetInjectedSeekFailure(t *testing.T) {
	sentinel := errors.New("injected")
	file := createFaultBase(t)
	fault := &faultJournalFile{base: file, seekErr: sentinel}
	journal := &Journal{file: fault}
	if err := journal.Reset(Fingerprint{}); !errors.Is(err, sentinel) {
		t.Fatalf("Reset error = %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

type faultIOResult struct {
	n   int
	err error
}

type faultJournalFile struct {
	base        *os.File
	statCalls   int
	statFaults  map[int]error
	readCalls   int
	readFaults  map[int]error
	writeCalls  int
	writeFaults map[int]faultIOResult
	writeAtErr  error
	seekErr     error
	truncateErr error
	syncErr     error
	closeCalls  int
}

func (f *faultJournalFile) ReadAt(buffer []byte, offset int64) (int, error) {
	f.readCalls++
	if err := f.readFaults[f.readCalls]; err != nil {
		return 0, err
	}
	return f.base.ReadAt(buffer, offset)
}

func (f *faultJournalFile) Write(buffer []byte) (int, error) {
	f.writeCalls++
	if result, ok := f.writeFaults[f.writeCalls]; ok {
		return result.n, result.err
	}
	return f.base.Write(buffer)
}

func (f *faultJournalFile) WriteAt(buffer []byte, offset int64) (int, error) {
	if f.writeAtErr != nil {
		return 0, f.writeAtErr
	}
	return f.base.WriteAt(buffer, offset)
}

func (f *faultJournalFile) Seek(offset int64, whence int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return f.base.Seek(offset, whence)
}

func (f *faultJournalFile) Stat() (os.FileInfo, error) {
	f.statCalls++
	if err := f.statFaults[f.statCalls]; err != nil {
		return nil, err
	}
	return f.base.Stat()
}

func (f *faultJournalFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.base.Sync()
}

func (f *faultJournalFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.base.Truncate(size)
}

func (f *faultJournalFile) Close() error {
	f.closeCalls++
	return f.base.Close()
}

func injectedOpenOperations(file journalFile) journalOpenOperations {
	return journalOpenOperations{
		mkdirAll: func(string, os.FileMode) error { return nil },
		openFile: func(string, int, os.FileMode) (journalFile, error) { return file, nil },
	}
}

func createFaultBase(t testing.TB) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "fault-journal")
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func writeReplayFixture(t testing.TB, batch bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal")
	journal, _, err := Open(path, Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	if batch {
		_, err = journal.AppendReplaceBatch(1, 1, []ReplaceOperation{{Inserted: []byte("x")}})
	} else {
		_, err = journal.AppendReplace(1, 0, 0, []byte("x"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func openFaultJournal(t testing.TB, path string, fault *faultJournalFile) *Journal {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fault.base = file
	return &Journal{file: fault, path: path}
}
