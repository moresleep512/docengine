package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveOpenOptionsDefaultsAndCustomValues(t *testing.T) {
	defaults, err := resolveOpenOptions(OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(defaults.RecoveryDir) || !filepath.IsAbs(defaults.SessionDir) ||
		defaults.RecoveryDirOwnership != DirectoryShared || defaults.SessionDirOwnership != DirectoryOwned ||
		defaults.Limits != (SessionLimits{MaxBatchOperations: DefaultMaxBatchOperations, MaxInsertBytes: DefaultMaxInsertBytes, UndoBytes: DefaultUndoBytes, MaxJournalBytes: DefaultMaxJournalBytes, EventHistory: DefaultEventHistory, ChangeHistory: DefaultChangeHistory, MaxAnchorBatch: DefaultMaxAnchorBatch, MaxSnapshotLeases: DefaultMaxSnapshotLeases, MaxSubscriptions: DefaultMaxSubscriptions}) ||
		defaults.JournalSyncInterval != DefaultJournalSyncInterval || defaults.AutoCheckpointJournalBytes != 0 {
		t.Fatalf("default config = %+v", defaults)
	}

	dir := t.TempDir()
	options := OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session"),
		RecoveryDirOwnership: DirectoryOwned, SessionDirOwnership: DirectoryShared,
		Limits:                     SessionLimits{MaxBatchOperations: 2, MaxInsertBytes: 3, UndoBytes: 4, MaxJournalBytes: 4_096, EventHistory: 5, ChangeHistory: 6, MaxAnchorBatch: 7, MaxSnapshotLeases: 8, MaxSubscriptions: 9},
		JournalSyncInterval:        5 * time.Millisecond,
		AutoCheckpointJournalBytes: 2_048,
	}
	custom, err := resolveOpenOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	if custom.RecoveryDir != options.RecoveryDir || custom.SessionDir != options.SessionDir ||
		custom.RecoveryDirOwnership != DirectoryOwned || custom.SessionDirOwnership != DirectoryShared ||
		custom.Limits != options.Limits || custom.JournalSyncInterval != options.JournalSyncInterval ||
		custom.AutoCheckpointJournalBytes != options.AutoCheckpointJournalBytes {
		t.Fatalf("custom config = %+v", custom)
	}

	explicitDefaults, err := resolveOpenOptions(OpenOptions{RecoveryDir: options.RecoveryDir, SessionDir: options.SessionDir})
	if err != nil || explicitDefaults.RecoveryDirOwnership != DirectoryShared || explicitDefaults.SessionDirOwnership != DirectoryShared {
		t.Fatalf("explicit default ownership = (%+v, %v)", explicitDefaults, err)
	}
}

func TestResolveOpenOptionsRejectsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	tests := []OpenOptions{
		{Limits: SessionLimits{MaxBatchOperations: -1}},
		{Limits: SessionLimits{MaxBatchOperations: DefaultMaxBatchOperations + 1}},
		{Limits: SessionLimits{MaxInsertBytes: -1}},
		{Limits: SessionLimits{MaxInsertBytes: MaximumInsertBytes + 1}},
		{Limits: SessionLimits{UndoBytes: -1}},
		{Limits: SessionLimits{MaxJournalBytes: -1}},
		{Limits: SessionLimits{MaxJournalBytes: MinimumJournalBytes - 1}},
		{Limits: SessionLimits{EventHistory: -1}},
		{Limits: SessionLimits{EventHistory: MaximumEventHistory + 1}},
		{Limits: SessionLimits{ChangeHistory: -1}},
		{Limits: SessionLimits{ChangeHistory: MaximumChangeHistory + 1}},
		{Limits: SessionLimits{MaxAnchorBatch: -1}},
		{Limits: SessionLimits{MaxAnchorBatch: MaximumAnchorBatch + 1}},
		{Limits: SessionLimits{MaxSnapshotLeases: -1}},
		{Limits: SessionLimits{MaxSnapshotLeases: MaximumSnapshotLeases + 1}},
		{Limits: SessionLimits{MaxSubscriptions: -1}},
		{Limits: SessionLimits{MaxSubscriptions: MaximumSubscriptions + 1}},
		{JournalSyncInterval: -1},
		{AutoCheckpointJournalBytes: -1},
		{AutoCheckpointJournalBytes: MinimumJournalBytes - 1},
		{Limits: SessionLimits{MaxJournalBytes: MinimumJournalBytes}, AutoCheckpointJournalBytes: MinimumJournalBytes + 1},
		{RecoveryDirOwnership: DirectoryShared},
		{SessionDirOwnership: DirectoryOwned},
		{RecoveryDir: filepath.Join(dir, "r"), RecoveryDirOwnership: DirectoryOwnership(99)},
		{RecoveryDir: filepath.Join(dir, "r"), SessionDir: filepath.Join(dir, "s"), SessionDirOwnership: DirectoryOwnership(99)},
	}
	for index, options := range tests {
		if _, err := resolveOpenOptions(options); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("case %d = %v", index, err)
		}
	}
	if _, err := Open("ignored", OpenOptions{Limits: SessionLimits{MaxBatchOperations: -1}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Open invalid options = %v", err)
	}
	sentinel := errors.New("absolute")
	if _, _, err := resolveDirectoryWith("relative", DirectoryShared, "unused", DirectoryOwned, func(string) (string, error) {
		return "", sentinel
	}); !errors.Is(err, ErrInvalidOptions) || !errors.Is(err, sentinel) {
		t.Fatalf("absolute error = %v", err)
	}
}

func TestRemoveEmptyDirectoryBoundaries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	if err := removeEmptyDirectory(dir); err != nil {
		t.Fatalf("missing directory = %v", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("nonempty directory removed: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty directory remains: %v", err)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirectory(file); err != nil {
		t.Fatalf("file cleanup = %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file was removed: %v", err)
	}
	sentinel := errors.New("operation")
	if err := removeEmptyDirectoryWith("ignored", func(string) (os.FileInfo, error) { return nil, sentinel }, os.ReadDir, os.Remove); !errors.Is(err, sentinel) {
		t.Fatalf("Lstat error = %v", err)
	}
	directoryInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lstatDirectory := func(string) (os.FileInfo, error) { return directoryInfo, nil }
	readError := func(string) ([]os.DirEntry, error) { return nil, sentinel }
	if err := removeEmptyDirectoryWith("ignored", lstatDirectory, readError, os.Remove); !errors.Is(err, sentinel) {
		t.Fatalf("ReadDir error = %v", err)
	}
	if err := removeEmptyDirectoryWith("ignored", lstatDirectory, func(string) ([]os.DirEntry, error) {
		return nil, os.ErrNotExist
	}, os.Remove); err != nil {
		t.Fatalf("ReadDir not-exist = %v", err)
	}
	empty := func(string) ([]os.DirEntry, error) { return nil, nil }
	if err := removeEmptyDirectoryWith("ignored", lstatDirectory, empty, func(string) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Remove error = %v", err)
	}
	if err := removeEmptyDirectoryWith("ignored", lstatDirectory, empty, func(string) error { return os.ErrNotExist }); err != nil {
		t.Fatalf("Remove not-exist = %v", err)
	}
}

func TestConfiguredLimitsAndConfigLifetime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session"),
		Limits:                     SessionLimits{MaxBatchOperations: 2, MaxInsertBytes: 2, UndoBytes: 3, MaxJournalBytes: 4_096, EventHistory: 4, ChangeHistory: 5, MaxAnchorBatch: 6, MaxSnapshotLeases: 7, MaxSubscriptions: 8},
		JournalSyncInterval:        5 * time.Millisecond,
		AutoCheckpointJournalBytes: 2_048,
	}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	config := session.Config()
	if config.Limits != options.Limits || config.JournalSyncInterval != options.JournalSyncInterval ||
		config.AutoCheckpointJournalBytes != options.AutoCheckpointJournalBytes {
		t.Fatalf("Config = %+v", config)
	}
	if _, err := session.ApplyBatch(nil, 0, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil ApplyBatch context = %v", err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, make([]ReplaceOperation, 3)); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("batch limit error = %v", err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Insert: "abc"}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("insert limit error = %v", err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Insert: "\xff"}}); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Insert: "é"}, {Start: 2, Insert: "x"}})
	if err != nil || result.Revision != 2 || result.ByteLength != 5 {
		t.Fatalf("accepted batch = (%+v, %v)", result, err)
	}
	if _, err := session.ApplyBatch(context.Background(), 2, []ReplaceOperation{{DeleteLength: 5}}); err != nil {
		t.Fatal(err)
	}
	if session.undoEpoch != 1 {
		t.Fatalf("undo epoch = %d", session.undoEpoch)
	}
	if _, err := session.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("Undo after configured quota reset = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if after := session.Config(); after != config {
		t.Fatalf("Config changed after close: %+v -> %+v", config, after)
	}
}

func TestOpenContextRejectsNilContext(t *testing.T) {
	if _, err := OpenContext(nil, "ignored", OpenOptions{}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("OpenContext nil = %v", err)
	}
}

func TestDirectoryOwnershipLifecycle(t *testing.T) {
	t.Run("default session directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		session, err := Open(path, OpenOptions{RecoveryDir: filepath.Join(dir, "shared-recovery")})
		if err != nil {
			t.Fatal(err)
		}
		config, undoPath := session.Config(), session.undoStore.path
		if config.SessionDirOwnership != DirectoryOwned {
			t.Fatalf("ownership = %+v", config)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range []string{config.SessionDir, undoPath} {
			if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned path remains %q: %v", candidate, err)
			}
		}
	})

	t.Run("shared concurrent session directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		sessionDir := filepath.Join(dir, "sessions")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(sessionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(sessionDir, "host-owned")
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		options := OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: sessionDir}
		first, err := Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Open(path, options)
		if err != nil {
			first.Close()
			t.Fatal(err)
		}
		firstUndo, secondUndo := first.undoStore.path, second.undoStore.path
		if firstUndo == secondUndo {
			t.Fatalf("shared undo paths collide: %q", firstUndo)
		}
		if err := errors.Join(first.Close(), second.Close()); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range []string{firstUndo, secondUndo} {
			if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("undo path remains %q: %v", candidate, err)
			}
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("shared marker removed: %v", err)
		}
	})

	t.Run("owned recovery survives dirty close", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		recoveryDir, sessionDir := filepath.Join(dir, "owned-recovery"), filepath.Join(dir, "owned-session")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		options := OpenOptions{
			RecoveryDir: recoveryDir, SessionDir: sessionDir,
			RecoveryDirOwnership: DirectoryOwned, SessionDirOwnership: DirectoryOwned,
		}
		session, err := Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "y"}}); err != nil {
			t.Fatal(err)
		}
		journalPath := session.journal.Path()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned session directory remains: %v", err)
		}
		if _, err := os.Stat(journalPath); err != nil {
			t.Fatalf("dirty journal removed: %v", err)
		}
		reopened, err := Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.Save(); err != nil {
			t.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(recoveryDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clean owned recovery directory remains: %v", err)
		}
	})

	t.Run("snapshot delays owned recovery cleanup", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		recoveryDir := filepath.Join(dir, "owned-recovery")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		session, err := Open(path, OpenOptions{
			RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "owned-session"),
			RecoveryDirOwnership: DirectoryOwned, SessionDirOwnership: DirectoryOwned,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "y"}}); err != nil {
			t.Fatal(err)
		}
		journalPath := session.journal.Path()
		_, lease, err := session.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Save(); err != nil {
			lease.Close()
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			lease.Close()
			t.Fatal(err)
		}
		if _, err := os.Stat(journalPath); err != nil {
			lease.Close()
			t.Fatalf("leased journal removed early: %v", err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(recoveryDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned recovery directory remains after lease close: %v", err)
		}
	})
}

func TestOwnedDirectoryCleanupPreservesUnexpectedFilesAndOpenFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(dir, "owned-session")
	recoveryDir := filepath.Join(dir, "owned-recovery")
	for _, target := range []string{sessionDir, recoveryDir} {
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "host-file"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	options := OpenOptions{
		RecoveryDir: recoveryDir, SessionDir: sessionDir,
		RecoveryDirOwnership: DirectoryOwned, SessionDirOwnership: DirectoryOwned,
	}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{sessionDir, recoveryDir} {
		if _, err := os.Stat(filepath.Join(target, "host-file")); err != nil {
			t.Fatalf("unexpected file in %q removed: %v", target, err)
		}
	}

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failedSessionDir := filepath.Join(dir, "failed-session")
	_, err = Open(path, OpenOptions{
		RecoveryDir: filepath.Join(blocker, "recovery"), SessionDir: failedSessionDir,
		RecoveryDirOwnership: DirectoryOwned, SessionDirOwnership: DirectoryOwned,
	})
	if err == nil {
		t.Fatal("expected recovery directory failure")
	}
	if _, statErr := os.Stat(failedSessionDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("owned session directory leaked after Open failure: %v", statErr)
	}
}
