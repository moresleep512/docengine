package document

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimStaleSessionDirectoriesConservativeCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if stats, err := ReclaimStaleSessionDirectories(root, time.Now()); err != nil || stats != (ReclaimStats{}) {
		t.Fatalf("missing root = (%+v, %v)", stats, err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "host-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := makeSessionArtifact(t, root, "stale", sessionMarkerMagic, true)
	recent := makeSessionArtifact(t, root, "recent", sessionMarkerMagic, true)
	malformed := makeSessionArtifact(t, root, "malformed", "not-docengine", true)
	unexpected := makeSessionArtifact(t, root, "unexpected", sessionMarkerMagic, true)
	if err := os.WriteFile(filepath.Join(unexpected, "host-owned"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withoutMarker := filepath.Join(root, "without-marker")
	if err := os.Mkdir(withoutMarker, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, dir := range []string{stale, malformed, unexpected} {
		if err := os.Chtimes(filepath.Join(dir, sessionMarkerName), old, old); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := ReclaimStaleSessionDirectories(root, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 5 || stats.Reclaimed != 1 || stats.Skipped != 4 {
		t.Fatalf("reclaim stats = %+v", stats)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory remains: %v", err)
	}
	for _, dir := range []string{recent, malformed, unexpected, withoutMarker} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("conservative cleanup removed %q: %v", dir, err)
		}
	}
}

func TestOwnedSessionMarkerLockAndCrashReclamation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	dir := filepath.Join(root, "owned")
	marker, err := openOwnedSessionMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openOwnedSessionMarker(dir); !errors.Is(err, ErrSessionInUse) {
		marker.close()
		t.Fatalf("second owner = %v", err)
	}
	old := time.Now().Add(-2 * DefaultStaleSessionAge)
	if err := os.Chtimes(marker.path, old, old); err != nil {
		marker.close()
		t.Fatal(err)
	}
	stats, err := ReclaimStaleSessionDirectories(root, time.Now())
	if err != nil || stats.Reclaimed != 0 || stats.Skipped != 1 {
		marker.close()
		t.Fatalf("live reclaim = (%+v, %v)", stats, err)
	}
	if err := unlockSessionFile(marker.file); err != nil {
		marker.close()
		t.Fatal(err)
	}
	if err := marker.file.Close(); err != nil {
		t.Fatal(err)
	}
	marker.file = nil // Simulate process exit without normal marker removal.
	if err := os.WriteFile(filepath.Join(dir, ".docengine-undo-crash.store"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err = ReclaimStaleSessionDirectories(root, time.Now())
	if err != nil || stats.Reclaimed != 1 {
		t.Fatalf("crash reclaim = (%+v, %v)", stats, err)
	}
	if err := marker.close(); err != nil {
		t.Fatalf("closed simulated marker = %v", err)
	}

	marker, err = openOwnedSessionMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.close(); err != nil {
		t.Fatal(err)
	}
	if err := marker.close(); err != nil {
		t.Fatalf("second marker close = %v", err)
	}
	if err := removeEmptyDirectory(dir); err != nil {
		t.Fatal(err)
	}
}

func TestSessionOwnedDirectoryRejectsConcurrentOwnerAndReopens(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doc")
	sessionDir := filepath.Join(root, "owned-session")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{
		RecoveryDir: filepath.Join(root, "recovery"), SessionDir: sessionDir,
		SessionDirOwnership: DirectoryOwned,
	}
	first, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, options); !errors.Is(err, ErrSessionInUse) {
		first.Close()
		t.Fatalf("concurrent owned Open = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionFileLockBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		owner.Close()
		t.Fatal(err)
	}
	locked, err := tryLockSessionFile(owner)
	if err != nil || !locked {
		t.Fatalf("owner lock = (%v, %v)", locked, err)
	}
	if locked, err := tryLockSessionFile(contender); err != nil || locked {
		t.Fatalf("contended lock = (%v, %v)", locked, err)
	}
	if err := unlockSessionFile(owner); err != nil {
		t.Fatal(err)
	}
	if locked, err := tryLockSessionFile(contender); err != nil || !locked {
		t.Fatalf("lock after release = (%v, %v)", locked, err)
	}
	if err := unlockSessionFile(contender); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(owner.Close(), contender.Close()); err != nil {
		t.Fatal(err)
	}
	if locked, err := tryLockSessionFile(owner); err == nil || locked {
		t.Fatalf("closed lock = (%v, %v)", locked, err)
	}
	if err := unlockSessionFile(owner); err == nil {
		t.Fatal("closed unlock succeeded")
	}
}

func TestReclaimSessionRootErrorsAndUnsafeMarkers(t *testing.T) {
	rootFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(rootFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReclaimStaleSessionDirectories(rootFile, time.Now()); err == nil {
		t.Fatal("expected root ReadDir error")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "candidate")
	if err := os.MkdirAll(filepath.Join(dir, sessionMarkerName), 0o700); err != nil {
		t.Fatal(err)
	}
	stats, err := ReclaimStaleSessionDirectories(root, time.Now())
	if err != nil || stats.Skipped != 1 {
		t.Fatalf("directory marker = (%+v, %v)", stats, err)
	}
	if err := removeIfExists(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("missing remove = %v", err)
	}
}

func TestSessionCleanupInjectedFailures(t *testing.T) {
	sentinel := errors.New("cleanup operation")
	t.Run("root operations", func(t *testing.T) {
		operations := systemSessionCleanupOperations
		operations.absolutePath = func(string) (string, error) { return "", sentinel }
		if _, err := reclaimStaleSessionDirectoriesWith("root", time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("absolute error = %v", err)
		}

		operations = systemSessionCleanupOperations
		operations.lstat = func(string) (os.FileInfo, error) { return nil, sentinel }
		if _, err := reclaimStaleSessionDirectoriesWith("root", time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("root Lstat error = %v", err)
		}

		root := t.TempDir()
		operations = systemSessionCleanupOperations
		operations.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist }
		if stats, err := reclaimStaleSessionDirectoriesWith(root, time.Now(), operations); err != nil || stats != (ReclaimStats{}) {
			t.Fatalf("root disappeared = (%+v, %v)", stats, err)
		}
		operations.readDir = func(string) ([]os.DirEntry, error) { return nil, sentinel }
		if _, err := reclaimStaleSessionDirectoriesWith(root, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("root ReadDir error = %v", err)
		}

		candidate := filepath.Join(root, "candidate")
		if err := os.Mkdir(candidate, 0o700); err != nil {
			t.Fatal(err)
		}
		operations = systemSessionCleanupOperations
		operations.lstat = func(path string) (os.FileInfo, error) {
			if path == filepath.Join(candidate, sessionMarkerName) {
				return nil, sentinel
			}
			return os.Lstat(path)
		}
		stats, err := reclaimStaleSessionDirectoriesWith(root, time.Now(), operations)
		if !errors.Is(err, sentinel) || stats.Skipped != 1 {
			t.Fatalf("candidate error = (%+v, %v)", stats, err)
		}
	})

	t.Run("candidate operations", func(t *testing.T) {
		root := t.TempDir()
		dir := makeSessionArtifact(t, root, "candidate", sessionMarkerMagic, true)
		markerPath := filepath.Join(dir, sessionMarkerName)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(markerPath, old, old); err != nil {
			t.Fatal(err)
		}
		base := systemSessionCleanupOperations
		operations := base
		operations.openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, sentinel }
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("open marker error = %v", err)
		}

		operations = base
		operations.lock = func(*os.File) (bool, error) { return false, sentinel }
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("lock marker error = %v", err)
		}

		operations = base
		operations.readMarker = func(*os.File) ([]byte, error) { return nil, sentinel }
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("read marker error = %v", err)
		}

		operations = base
		operations.readDir = func(path string) ([]os.DirEntry, error) {
			if path == dir {
				return nil, sentinel
			}
			return os.ReadDir(path)
		}
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("candidate ReadDir error = %v", err)
		}

		operations = base
		operations.unlock = func(file *os.File) error {
			_ = unlockSessionFile(file)
			return sentinel
		}
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("unlock error = %v", err)
		}

		operations = base
		operations.close = func(file *os.File) error {
			_ = file.Close()
			return sentinel
		}
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("close error = %v", err)
		}

		undoPath := filepath.Join(dir, ".docengine-undo-test.store")
		operations = base
		operations.lstat = func(path string) (os.FileInfo, error) {
			if path == undoPath {
				return nil, sentinel
			}
			return os.Lstat(path)
		}
		if _, err := reclaimSessionDirectoryWith(dir, time.Now(), operations); !errors.Is(err, sentinel) {
			t.Fatalf("undo Lstat error = %v", err)
		}

		directoryUndo := filepath.Join(dir, ".docengine-undo-directory.store")
		if err := os.Mkdir(directoryUndo, 0o700); err != nil {
			t.Fatal(err)
		}
		if reclaimed, err := reclaimSessionDirectory(dir, time.Now()); err != nil || reclaimed {
			t.Fatalf("directory undo = (%v, %v)", reclaimed, err)
		}
		if err := os.Remove(directoryUndo); err != nil {
			t.Fatal(err)
		}

		for name, target := range map[string]string{"undo": undoPath, "marker": markerPath, "directory": dir} {
			t.Run("remove "+name, func(t *testing.T) {
				copyRoot := t.TempDir()
				copyDir := makeSessionArtifact(t, copyRoot, "candidate", sessionMarkerMagic, true)
				copyMarker := filepath.Join(copyDir, sessionMarkerName)
				copyUndo := filepath.Join(copyDir, ".docengine-undo-test.store")
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(copyMarker, old, old); err != nil {
					t.Fatal(err)
				}
				selected := map[string]string{"undo": copyUndo, "marker": copyMarker, "directory": copyDir}[name]
				operations := base
				operations.remove = func(path string) error {
					if path == selected {
						return sentinel
					}
					return os.Remove(path)
				}
				if _, err := reclaimSessionDirectoryWith(copyDir, time.Now(), operations); !errors.Is(err, sentinel) {
					t.Fatalf("remove error = %v", err)
				}
				_ = target
			})
		}

		copyRoot := t.TempDir()
		copyDir := makeSessionArtifact(t, copyRoot, "candidate", sessionMarkerMagic, true)
		copyMarker := filepath.Join(copyDir, sessionMarkerName)
		old = time.Now().Add(-time.Hour)
		if err := os.Chtimes(copyMarker, old, old); err != nil {
			t.Fatal(err)
		}
		operations = base
		operations.remove = func(path string) error {
			err := os.Remove(path)
			if err == nil {
				return os.ErrNotExist
			}
			return err
		}
		if reclaimed, err := reclaimSessionDirectoryWith(copyDir, time.Now(), operations); err != nil || !reclaimed {
			t.Fatalf("remove races = (%v, %v)", reclaimed, err)
		}
	})

	t.Run("marker creation operations", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "owned")
		base := systemSessionCleanupOperations
		operations := base
		operations.lstat = func(path string) (os.FileInfo, error) {
			if path == filepath.Join(dir, sessionMarkerName) {
				return nil, sentinel
			}
			return os.Lstat(path)
		}
		if _, err := openOwnedSessionMarkerWith(dir, operations); !errors.Is(err, sentinel) {
			t.Fatalf("exact reclaim error = %v", err)
		}

		for name, mutate := range map[string]func(*sessionCleanupOperations){
			"mkdir": func(ops *sessionCleanupOperations) {
				ops.mkdirAll = func(string, os.FileMode) error { return sentinel }
			},
			"open": func(ops *sessionCleanupOperations) {
				ops.openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, sentinel }
			},
			"write": func(ops *sessionCleanupOperations) {
				ops.writeMarker = func(*os.File, string) (int, error) { return 0, sentinel }
			},
			"sync": func(ops *sessionCleanupOperations) { ops.syncMarker = func(*os.File) error { return sentinel } },
			"lock-error": func(ops *sessionCleanupOperations) {
				ops.lock = func(*os.File) (bool, error) { return false, sentinel }
			},
			"lock-busy": func(ops *sessionCleanupOperations) {
				ops.lock = func(*os.File) (bool, error) { return false, nil }
			},
		} {
			t.Run(name, func(t *testing.T) {
				candidate := filepath.Join(t.TempDir(), "owned")
				operations := base
				mutate(&operations)
				_, err := openOwnedSessionMarkerWith(candidate, operations)
				if name == "lock-busy" {
					if !errors.Is(err, ErrSessionInUse) {
						t.Fatalf("busy error = %v", err)
					}
				} else if !errors.Is(err, sentinel) {
					t.Fatalf("operation error = %v", err)
				}
			})
		}
	})
}

func makeSessionArtifact(t testing.TB, root, name, marker string, undo bool) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionMarkerName), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if undo {
		if err := os.WriteFile(filepath.Join(dir, ".docengine-undo-test.store"), []byte("undo"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
