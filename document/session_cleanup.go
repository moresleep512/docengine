package document

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultStaleSessionAge limits automatic scanning to old crash artifacts.
	// A held marker lock always protects a live Session regardless of age.
	DefaultStaleSessionAge = 24 * time.Hour
	sessionMarkerName      = ".docengine-session-v1"
	sessionMarkerMagic     = "DOCSESSION1\n"
)

var ErrSessionInUse = errors.New("document: owned session directory is in use")

// ReclaimStats reports conservative owned-session directory cleanup.
type ReclaimStats struct {
	Scanned   int
	Reclaimed int
	Skipped   int
}

type sessionMarker struct {
	file *os.File
	path string
}

type sessionCleanupOperations struct {
	absolutePath func(string) (string, error)
	lstat        func(string) (os.FileInfo, error)
	readDir      func(string) ([]os.DirEntry, error)
	openFile     func(string, int, os.FileMode) (*os.File, error)
	mkdirAll     func(string, os.FileMode) error
	remove       func(string) error
	lock         func(*os.File) (bool, error)
	unlock       func(*os.File) error
	close        func(*os.File) error
	readMarker   func(*os.File) ([]byte, error)
	writeMarker  func(*os.File, string) (int, error)
	syncMarker   func(*os.File) error
	now          func() time.Time
}

var systemSessionCleanupOperations = sessionCleanupOperations{
	absolutePath: filepath.Abs,
	lstat:        os.Lstat,
	readDir:      os.ReadDir,
	openFile:     os.OpenFile,
	mkdirAll:     os.MkdirAll,
	remove:       os.Remove,
	lock:         tryLockSessionFile,
	unlock:       unlockSessionFile,
	close:        func(file *os.File) error { return file.Close() },
	readMarker: func(file *os.File) ([]byte, error) {
		return io.ReadAll(io.LimitReader(file, int64(len(sessionMarkerMagic)+1)))
	},
	writeMarker: func(file *os.File, value string) (int, error) { return file.WriteString(value) },
	syncMarker:  func(file *os.File) error { return file.Sync() },
	now:         time.Now,
}

// ReclaimStaleSessionDirectories removes crash leftovers created by Docengine
// below root and older than before. Directories with a live marker lock,
// malformed markers, symlinks, or any unrecognized entry are preserved.
func ReclaimStaleSessionDirectories(root string, before time.Time) (ReclaimStats, error) {
	return reclaimStaleSessionDirectoriesWith(root, before, systemSessionCleanupOperations)
}

func reclaimStaleSessionDirectoriesWith(root string, before time.Time, operations sessionCleanupOperations) (ReclaimStats, error) {
	absolute, err := operations.absolutePath(root)
	if err != nil {
		return ReclaimStats{}, err
	}
	info, err := operations.lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return ReclaimStats{}, nil
	}
	if err != nil {
		return ReclaimStats{}, err
	}
	if !info.IsDir() {
		return ReclaimStats{}, &os.PathError{Op: "reclaim sessions", Path: absolute, Err: os.ErrInvalid}
	}
	entries, err := operations.readDir(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return ReclaimStats{}, nil
	}
	if err != nil {
		return ReclaimStats{}, err
	}
	var result ReclaimStats
	var cleanupErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		result.Scanned++
		reclaimed, candidateErr := reclaimSessionDirectoryWith(filepath.Join(absolute, entry.Name()), before, operations)
		if candidateErr != nil {
			cleanupErr = errors.Join(cleanupErr, candidateErr)
			result.Skipped++
		} else if reclaimed {
			result.Reclaimed++
		} else {
			result.Skipped++
		}
	}
	return result, cleanupErr
}

func reclaimSessionDirectory(dir string, before time.Time) (bool, error) {
	return reclaimSessionDirectoryWith(dir, before, systemSessionCleanupOperations)
}

func reclaimSessionDirectoryWith(dir string, before time.Time, operations sessionCleanupOperations) (bool, error) {
	markerPath := filepath.Join(dir, sessionMarkerName)
	info, err := operations.lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !info.ModTime().Before(before) {
		return false, nil
	}
	marker, err := operations.openFile(markerPath, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	locked, err := operations.lock(marker)
	if err != nil {
		_ = operations.close(marker)
		return false, err
	}
	if !locked {
		_ = operations.close(marker)
		return false, nil
	}
	content, readErr := operations.readMarker(marker)
	entries, entriesErr := operations.readDir(dir)
	removable := readErr == nil && entriesErr == nil && bytes.Equal(content, []byte(sessionMarkerMagic))
	paths := make([]string, 0, len(entries))
	if removable {
		for _, entry := range entries {
			name := entry.Name()
			if name == sessionMarkerName {
				continue
			}
			if !strings.HasPrefix(name, ".docengine-undo-") || !strings.HasSuffix(name, ".store") {
				removable = false
				break
			}
			path := filepath.Join(dir, name)
			entryInfo, statErr := operations.lstat(path)
			if statErr != nil || !entryInfo.Mode().IsRegular() {
				if statErr != nil {
					entriesErr = statErr
				}
				removable = false
				break
			}
			paths = append(paths, path)
		}
	}
	unlockErr := operations.unlock(marker)
	closeErr := operations.close(marker)
	if readErr != nil || entriesErr != nil || unlockErr != nil || closeErr != nil {
		return false, errors.Join(readErr, entriesErr, unlockErr, closeErr)
	}
	if !removable {
		return false, nil
	}
	for _, path := range paths {
		if err := operations.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if err := operations.remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := operations.remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func openOwnedSessionMarker(dir string) (*sessionMarker, error) {
	return openOwnedSessionMarkerWith(dir, systemSessionCleanupOperations)
}

func openOwnedSessionMarkerWith(dir string, operations sessionCleanupOperations) (*sessionMarker, error) {
	root := filepath.Dir(dir)
	// Reclamation elsewhere in the shared root is best effort; an unrelated
	// unreadable orphan must not prevent this Session from opening. Hosts that
	// need diagnostics can call ReclaimStaleSessionDirectories directly.
	_, _ = reclaimStaleSessionDirectoriesWith(root, operations.now().Add(-DefaultStaleSessionAge), operations)
	// The exact requested directory may be a recent crash artifact. A live
	// marker lock, rather than age, is the authority for this one directory.
	if _, err := reclaimSessionDirectoryWith(dir, operations.now().Add(time.Nanosecond), operations); err != nil {
		return nil, err
	}
	if err := operations.mkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sessionMarkerName)
	file, err := operations.openFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrSessionInUse, dir)
	}
	if err != nil {
		return nil, err
	}
	cleanup := func(cause error) (*sessionMarker, error) {
		return nil, errors.Join(cause, operations.close(file), operations.remove(path), removeEmptyDirectory(dir))
	}
	if _, err := operations.writeMarker(file, sessionMarkerMagic); err != nil {
		return cleanup(err)
	}
	if err := operations.syncMarker(file); err != nil {
		return cleanup(err)
	}
	locked, err := operations.lock(file)
	if err != nil {
		return cleanup(err)
	}
	if !locked {
		return cleanup(ErrSessionInUse)
	}
	return &sessionMarker{file: file, path: path}, nil
}

func (m *sessionMarker) close() error {
	if m == nil || m.file == nil {
		return nil
	}
	file := m.file
	m.file = nil
	return errors.Join(unlockSessionFile(file), file.Close(), removeIfExists(m.path))
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
