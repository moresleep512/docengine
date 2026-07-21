package save

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWritesPrefixContentModeAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "document.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	total, err := Atomic(path, 0o640, []byte("BOM:"), func(writer io.Writer) (int64, error) {
		n, err := writer.Write([]byte("content"))
		return int64(n), err
	})
	if err != nil || total != 11 {
		t.Fatalf("Atomic = (%d, %v), want (11, nil)", total, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "BOM:content" {
		t.Fatalf("content = %q, error = %v", content, err)
	}
	assertNoSaveTemps(t, dir)
}

func TestAtomicCheckedRealFilesystemFailuresPreserveOriginal(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		_, err := Atomic(filepath.Join(t.TempDir(), "missing", "doc"), 0o600, nil, func(io.Writer) (int64, error) { return 0, nil })
		if err == nil {
			t.Fatal("expected create-temp error")
		}
	})

	t.Run("conflict", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		conflict := errors.New("conflict")
		total, err := AtomicChecked(path, 0o600, []byte("p"), func(writer io.Writer) (int64, error) {
			n, writeErr := writer.Write([]byte("new"))
			return int64(n), writeErr
		}, func() error { return conflict })
		if total != 4 || !errors.Is(err, conflict) {
			t.Fatalf("AtomicChecked = (%d, %v)", total, err)
		}
		content, _ := os.ReadFile(path)
		if string(content) != "old" {
			t.Fatalf("original changed to %q", content)
		}
		assertNoSaveTemps(t, dir)
	})

	t.Run("directory replacement target", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "target-directory")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Atomic(path, 0o600, nil, func(writer io.Writer) (int64, error) {
			n, writeErr := writer.Write([]byte("new"))
			return int64(n), writeErr
		}); err == nil {
			t.Fatal("expected replace error")
		}
		assertNoSaveTemps(t, dir)
	})
}

func TestAtomicCheckedInjectedFailureBoundaries(t *testing.T) {
	sentinel := errors.New("injected")
	tests := []struct {
		name       string
		file       *fakeTemporaryFile
		createErr  error
		writer     func(io.Writer) (int64, error)
		check      func() error
		replaceErr error
		wantTotal  int64
	}{
		{name: "create", createErr: sentinel},
		{name: "chmod", file: &fakeTemporaryFile{chmodErr: sentinel}},
		{name: "prefix write", file: &fakeTemporaryFile{writeErr: sentinel, writeCount: 1}, wantTotal: 1},
		{name: "content write", file: &fakeTemporaryFile{}, writer: func(io.Writer) (int64, error) { return 2, sentinel }, wantTotal: 5},
		{name: "sync", file: &fakeTemporaryFile{syncErr: sentinel}, wantTotal: 6},
		{name: "close", file: &fakeTemporaryFile{closeErr: sentinel}, wantTotal: 6},
		{name: "check", file: &fakeTemporaryFile{}, check: func() error { return sentinel }, wantTotal: 6},
		{name: "replace", file: &fakeTemporaryFile{}, replaceErr: sentinel, wantTotal: 6},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := test.file
			removed := 0
			operations := atomicOperations{
				createTemp: func(string, string) (temporaryFile, error) {
					if test.createErr != nil {
						return nil, test.createErr
					}
					return file, nil
				},
				remove:  func(string) error { removed++; return sentinel },
				replace: func(string, string) error { return test.replaceErr },
			}
			writer := test.writer
			if writer == nil {
				writer = func(writer io.Writer) (int64, error) {
					n, err := writer.Write([]byte("new"))
					return int64(n), err
				}
			}
			total, err := atomicChecked("doc", 0o640, []byte("pre"), writer, test.check, operations)
			if total != test.wantTotal || !errors.Is(err, sentinel) {
				t.Fatalf("atomicChecked = (%d, %v), want (%d, injected)", total, err, test.wantTotal)
			}
			if test.createErr == nil && removed != 1 {
				t.Fatalf("remove calls = %d, want 1", removed)
			}
		})
	}
}

func TestAtomicCheckedInjectedSuccessCommitsWithoutRemoval(t *testing.T) {
	file := &fakeTemporaryFile{}
	removed, replaced := 0, 0
	operations := atomicOperations{
		createTemp: func(string, string) (temporaryFile, error) { return file, nil },
		remove:     func(string) error { removed++; return nil },
		replace:    func(from, to string) error { replaced++; return nil },
	}
	total, err := atomicChecked("doc", 0o600, nil, func(writer io.Writer) (int64, error) {
		n, writeErr := writer.Write([]byte("ok"))
		return int64(n), writeErr
	}, nil, operations)
	if err != nil || total != 2 || removed != 0 || replaced != 1 || file.closeCalls != 2 || !bytes.Equal(file.content.Bytes(), []byte("ok")) {
		t.Fatalf("result=(%d,%v) removed=%d replaced=%d closes=%d content=%q", total, err, removed, replaced, file.closeCalls, file.content.Bytes())
	}
}

type fakeTemporaryFile struct {
	content    bytes.Buffer
	chmodErr   error
	writeErr   error
	writeCount int
	syncErr    error
	closeErr   error
	closeCalls int
}

func (f *fakeTemporaryFile) Name() string            { return "temporary" }
func (f *fakeTemporaryFile) Chmod(os.FileMode) error { return f.chmodErr }
func (f *fakeTemporaryFile) Sync() error             { return f.syncErr }
func (f *fakeTemporaryFile) Close() error            { f.closeCalls++; return f.closeErr }
func (f *fakeTemporaryFile) Write(value []byte) (int, error) {
	if f.writeErr != nil {
		return f.writeCount, f.writeErr
	}
	return f.content.Write(value)
}

func assertNoSaveTemps(t testing.TB, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, ".docengine-save-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("temporary files remain: %v", paths)
	}
}
