//go:build !windows

package save

import (
	"errors"
	"os"
	"path/filepath"
)

type directoryHandle interface {
	Sync() error
	Close() error
}

type replaceOperations struct {
	rename  func(string, string) error
	openDir func(string) (directoryHandle, error)
}

var systemReplaceOperations = replaceOperations{
	rename: os.Rename,
	openDir: func(path string) (directoryHandle, error) {
		return os.Open(path)
	},
}

func replaceFile(from, to string) error {
	return replaceFileWithOperations(from, to, systemReplaceOperations)
}

func replaceFileWithOperations(from, to string, operations replaceOperations) error {
	if err := operations.rename(from, to); err != nil {
		return err
	}
	if err := syncParentWithOperations(to, operations); err != nil {
		return &DurabilityError{Path: to, Err: err}
	}
	return nil
}

func syncParent(path string) error {
	if err := syncParentWithOperations(path, systemReplaceOperations); err != nil {
		return &DurabilityError{Path: path, Err: err}
	}
	return nil
}

func syncParentWithOperations(path string, operations replaceOperations) error {
	directory, err := operations.openDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
