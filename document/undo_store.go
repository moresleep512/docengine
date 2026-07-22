package document

import (
	"errors"
	"io"
	"os"
	"sync"
)

var errUndoQuota = errors.New("document: undo store quota exceeded")

type textRef struct {
	offset int64
	length int64
}

type undoStore struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	size   int64
	quota  int64
	remove func(string) error
}

type undoStoreOperations struct {
	mkdirAll   func(string, os.FileMode) error
	createTemp func(string, string) (*os.File, error)
	remove     func(string) error
}

var systemUndoStoreOperations = undoStoreOperations{
	mkdirAll: os.MkdirAll, createTemp: os.CreateTemp, remove: os.Remove,
}

func openUndoStore(sessionDir string, quota int64) (*undoStore, error) {
	return openUndoStoreWith(sessionDir, quota, systemUndoStoreOperations)
}

func openUndoStoreWith(sessionDir string, quota int64, operations undoStoreOperations) (*undoStore, error) {
	if sessionDir == "" {
		return nil, nil
	}
	if err := operations.mkdirAll(sessionDir, 0o700); err != nil {
		return nil, err
	}
	file, err := operations.createTemp(sessionDir, ".docengine-undo-*.store")
	if err != nil {
		return nil, err
	}
	return &undoStore{file: file, path: file.Name(), quota: quota, remove: operations.remove}, nil
}

func (s *undoStore) append(value []byte) (textRef, error) {
	if len(value) == 0 {
		return textRef{}, nil
	}
	if s == nil {
		return textRef{}, errUndoQuota
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return textRef{}, ErrClosed
	}
	if int64(len(value)) > s.quota-s.size {
		return textRef{}, errUndoQuota
	}
	ref := textRef{offset: s.size, length: int64(len(value))}
	n, err := s.file.WriteAt(value, ref.offset)
	s.size += int64(n)
	return ref, err
}

func (s *undoStore) read(ref textRef) (string, error) {
	if ref.length == 0 {
		return "", nil
	}
	if s == nil {
		return "", errUndoQuota
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return "", ErrClosed
	}
	buffer := make([]byte, ref.length)
	n, err := s.file.ReadAt(buffer, ref.offset)
	if err != nil && !(errors.Is(err, io.EOF) && int64(n) == ref.length) {
		return "", err
	}
	return string(buffer[:n]), nil
}

func (s *undoStore) reset() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return ErrClosed
	}
	if err := s.file.Truncate(0); err != nil {
		return err
	}
	s.size = 0
	return nil
}

func (s *undoStore) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	remove := s.remove
	s.remove = nil
	if remove != nil {
		removeErr := remove(s.path)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}
