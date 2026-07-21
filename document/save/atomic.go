// Package save contains streaming, crash-safe document persistence.
package save

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type temporaryFile interface {
	io.Writer
	Name() string
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type atomicOperations struct {
	createTemp func(string, string) (temporaryFile, error)
	remove     func(string) error
	replace    func(string, string) error
}

var systemAtomicOperations = atomicOperations{
	createTemp: func(dir, pattern string) (temporaryFile, error) { return os.CreateTemp(dir, pattern) },
	remove:     os.Remove,
	replace:    replaceFile,
}

// Atomic streams content into a same-directory temporary file, flushes it, and
// atomically replaces path. The caller supplies an immutable document snapshot.
func Atomic(path string, mode os.FileMode, prefix []byte, writeContent func(io.Writer) (int64, error)) (int64, error) {
	return AtomicChecked(path, mode, prefix, writeContent, nil)
}

// AtomicChecked performs a final conflict check after the potentially long
// stream write and immediately before replacing the original file.
func AtomicChecked(path string, mode os.FileMode, prefix []byte, writeContent func(io.Writer) (int64, error), beforeReplace func() error) (int64, error) {
	return atomicChecked(path, mode, prefix, writeContent, beforeReplace, systemAtomicOperations)
}

func atomicChecked(path string, mode os.FileMode, prefix []byte, writeContent func(io.Writer) (int64, error), beforeReplace func() error, operations atomicOperations) (int64, error) {
	dir := filepath.Dir(path)
	temp, err := operations.createTemp(dir, ".docengine-save-*.tmp")
	if err != nil {
		return 0, err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = operations.remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode.Perm()); err != nil {
		return 0, err
	}
	var total int64
	if len(prefix) > 0 {
		n, err := temp.Write(prefix)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	n, err := writeContent(temp)
	total += n
	if err != nil {
		return total, err
	}
	if err := temp.Sync(); err != nil {
		return total, err
	}
	if err := temp.Close(); err != nil {
		return total, err
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return total, err
		}
	}
	if err := operations.replace(tempPath, path); err != nil {
		return total, fmt.Errorf("replace original: %w", err)
	}
	committed = true
	return total, nil
}
