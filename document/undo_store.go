package document

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

var errUndoQuota = errors.New("document: undo store quota exceeded")

const undoRewriteBufferBytes = 64 << 10

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
	create func(string, string) (*os.File, error)
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
	return &undoStore{file: file, path: file.Name(), quota: quota, remove: operations.remove, create: operations.createTemp}, nil
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

// rewrite compacts the store to the unique live references supplied by the
// caller. A non-nil mapping always describes the active replacement store,
// even when removing the retired temporary file reports an error.
func (s *undoStore) rewrite(refs []textRef) (map[textRef]textRef, error) {
	return s.rewriteContext(context.Background(), refs, nil)
}

func (s *undoStore) rewriteContext(ctx context.Context, refs []textRef, report func(int64, int64)) (map[textRef]textRef, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		if report != nil {
			report(0, 0)
		}
		return map[textRef]textRef{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil, ErrClosed
	}
	unique := make([]textRef, 0, len(refs))
	seen := make(map[textRef]struct{}, len(refs))
	var liveBytes int64
	for _, ref := range refs {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if ref.length == 0 {
			continue
		}
		if ref.offset < 0 || ref.length < 0 || ref.offset > s.size || ref.length > s.size-ref.offset {
			return nil, io.ErrUnexpectedEOF
		}
		if _, ok := seen[ref]; !ok {
			if ref.length > math.MaxInt64-liveBytes {
				return nil, ErrLimitExceeded
			}
			seen[ref] = struct{}{}
			unique = append(unique, ref)
			liveBytes += ref.length
		}
	}
	if report != nil {
		report(0, liveBytes)
	}
	if len(unique) == 0 {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if err := s.file.Truncate(0); err != nil {
			return nil, err
		}
		s.size = 0
		return map[textRef]textRef{}, nil
	}
	file, err := s.create(filepath.Dir(s.path), ".docengine-undo-*.store")
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
			_ = s.remove(file.Name())
		}
	}()
	mapping := make(map[textRef]textRef, len(unique))
	var size int64
	buffer := make([]byte, undoRewriteBufferBytes)
	for _, ref := range unique {
		if err := copyUndoReference(ctx, file, s.file, ref, buffer, func(copied int64) {
			if report != nil {
				report(size+copied, liveBytes)
			}
		}); err != nil {
			return nil, err
		}
		mapping[ref] = textRef{offset: size, length: ref.length}
		size += ref.length
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	oldFile, oldPath := s.file, s.path
	s.file, s.path, s.size = file, file.Name(), size
	committed = true
	closeErr := oldFile.Close()
	removeErr := s.remove(oldPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return mapping, errors.Join(closeErr, removeErr)
}

type undoReferenceReader interface {
	ReadAt([]byte, int64) (int, error)
}

type undoReferenceWriter interface {
	Write([]byte) (int, error)
}

func copyUndoReference(ctx context.Context, destination undoReferenceWriter, source undoReferenceReader, ref textRef, buffer []byte, report func(int64)) error {
	var copied int64
	for copied < ref.length {
		if err := contextError(ctx); err != nil {
			return err
		}
		want := min(int64(len(buffer)), ref.length-copied)
		n, readErr := source.ReadAt(buffer[:int(want)], ref.offset+copied)
		if n < 0 || int64(n) > want {
			return io.ErrUnexpectedEOF
		}
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			copied += int64(written)
			report(copied)
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil && !(errors.Is(readErr, io.EOF) && copied == ref.length) {
			return readErr
		}
		if n == 0 && copied < ref.length {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func (s *undoStore) bytes() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
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
