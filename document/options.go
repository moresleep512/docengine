package document

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultMaxBatchOperations  = 256
	DefaultMaxInsertBytes      = int64(1 << 20)
	DefaultUndoBytes           = int64(256 << 20)
	MaximumInsertBytes         = int64((1 << 30) - 24)
	DefaultChangeHistory       = 256
	MaximumChangeHistory       = 4_096
	DefaultMaxAnchorBatch      = 65_536
	MaximumAnchorBatch         = 1_048_576
	DefaultJournalSyncInterval = time.Second
)

var (
	ErrInvalidOptions = errors.New("document: invalid open options")
	ErrLimitExceeded  = errors.New("document: configured resource limit exceeded")
)

type DirectoryOwnership uint8

const (
	DirectoryOwnershipDefault DirectoryOwnership = iota
	DirectoryShared
	DirectoryOwned
)

type SessionLimits struct {
	MaxBatchOperations int
	MaxInsertBytes     int64
	UndoBytes          int64
	EventHistory       int
	ChangeHistory      int
	MaxAnchorBatch     int
}

type OpenOptions struct {
	RecoveryDir          string
	SessionDir           string
	RecoveryDirOwnership DirectoryOwnership
	SessionDirOwnership  DirectoryOwnership
	Limits               SessionLimits
	JournalSyncInterval  time.Duration
}

// SessionConfig is the fully resolved immutable configuration of an open
// Session. DirectoryOwnershipDefault never appears in a resolved config.
type SessionConfig struct {
	RecoveryDir          string
	SessionDir           string
	RecoveryDirOwnership DirectoryOwnership
	SessionDirOwnership  DirectoryOwnership
	Limits               SessionLimits
	JournalSyncInterval  time.Duration
}

func resolveOpenOptions(options OpenOptions) (SessionConfig, error) {
	limits := options.Limits
	if limits.MaxBatchOperations == 0 {
		limits.MaxBatchOperations = DefaultMaxBatchOperations
	}
	if limits.MaxInsertBytes == 0 {
		limits.MaxInsertBytes = DefaultMaxInsertBytes
	}
	if limits.UndoBytes == 0 {
		limits.UndoBytes = DefaultUndoBytes
	}
	if limits.EventHistory == 0 {
		limits.EventHistory = DefaultEventHistory
	}
	if limits.ChangeHistory == 0 {
		limits.ChangeHistory = DefaultChangeHistory
	}
	if limits.MaxAnchorBatch == 0 {
		limits.MaxAnchorBatch = DefaultMaxAnchorBatch
	}
	if limits.MaxBatchOperations < 0 || limits.MaxBatchOperations > DefaultMaxBatchOperations {
		return SessionConfig{}, fmt.Errorf("%w: MaxBatchOperations must be between 1 and %d", ErrInvalidOptions, DefaultMaxBatchOperations)
	}
	if limits.MaxInsertBytes < 0 || limits.MaxInsertBytes > MaximumInsertBytes {
		return SessionConfig{}, fmt.Errorf("%w: MaxInsertBytes must be between 1 and %d", ErrInvalidOptions, MaximumInsertBytes)
	}
	if limits.UndoBytes < 0 {
		return SessionConfig{}, fmt.Errorf("%w: UndoBytes must be positive", ErrInvalidOptions)
	}
	if limits.EventHistory < 0 || limits.EventHistory > MaximumEventHistory {
		return SessionConfig{}, fmt.Errorf("%w: EventHistory must be between 1 and %d", ErrInvalidOptions, MaximumEventHistory)
	}
	if limits.ChangeHistory < 0 || limits.ChangeHistory > MaximumChangeHistory {
		return SessionConfig{}, fmt.Errorf("%w: ChangeHistory must be between 1 and %d", ErrInvalidOptions, MaximumChangeHistory)
	}
	if limits.MaxAnchorBatch < 0 || limits.MaxAnchorBatch > MaximumAnchorBatch {
		return SessionConfig{}, fmt.Errorf("%w: MaxAnchorBatch must be between 1 and %d", ErrInvalidOptions, MaximumAnchorBatch)
	}
	syncInterval := options.JournalSyncInterval
	if syncInterval == 0 {
		syncInterval = DefaultJournalSyncInterval
	}
	if syncInterval < 0 {
		return SessionConfig{}, fmt.Errorf("%w: JournalSyncInterval must be positive", ErrInvalidOptions)
	}
	recoveryDir, recoveryOwnership, err := resolveDirectory(
		options.RecoveryDir,
		options.RecoveryDirOwnership,
		filepath.Join(os.TempDir(), "docengine", "recovery"),
		DirectoryShared,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	sessionDir, sessionOwnership, err := resolveDirectory(
		options.SessionDir,
		options.SessionDirOwnership,
		filepath.Join(os.TempDir(), "docengine", "sessions", randomSuffix()),
		DirectoryOwned,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	return SessionConfig{
		RecoveryDir: recoveryDir, SessionDir: sessionDir,
		RecoveryDirOwnership: recoveryOwnership, SessionDirOwnership: sessionOwnership,
		Limits: limits, JournalSyncInterval: syncInterval,
	}, nil
}

func resolveDirectory(path string, ownership DirectoryOwnership, defaultPath string, defaultOwnership DirectoryOwnership) (string, DirectoryOwnership, error) {
	return resolveDirectoryWith(path, ownership, defaultPath, defaultOwnership, filepath.Abs)
}

func resolveDirectoryWith(path string, ownership DirectoryOwnership, defaultPath string, defaultOwnership DirectoryOwnership, absolutePath func(string) (string, error)) (string, DirectoryOwnership, error) {
	if ownership > DirectoryOwned {
		return "", DirectoryOwnershipDefault, ErrInvalidOptions
	}
	if path == "" {
		if ownership != DirectoryOwnershipDefault {
			return "", DirectoryOwnershipDefault, fmt.Errorf("%w: directory ownership requires an explicit path", ErrInvalidOptions)
		}
		path, ownership = defaultPath, defaultOwnership
	} else if ownership == DirectoryOwnershipDefault {
		ownership = DirectoryShared
	}
	absolute, err := absolutePath(path)
	if err != nil {
		return "", DirectoryOwnershipDefault, fmt.Errorf("%w: resolve directory: %w", ErrInvalidOptions, err)
	}
	return filepath.Clean(absolute), ownership, nil
}

func removeEmptyDirectory(path string) error {
	return removeEmptyDirectoryWith(path, os.Lstat, os.ReadDir, os.Remove)
}

func removeEmptyDirectoryWith(path string, lstat func(string) (os.FileInfo, error), readDir func(string) ([]os.DirEntry, error), remove func(string) error) error {
	info, err := lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := readDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func cleanupOwnedDirectories(config SessionConfig, includeRecovery bool) error {
	var cleanupErr error
	if config.SessionDirOwnership == DirectoryOwned {
		cleanupErr = errors.Join(cleanupErr, removeEmptyDirectory(config.SessionDir))
	}
	if includeRecovery && config.RecoveryDirOwnership == DirectoryOwned {
		cleanupErr = errors.Join(cleanupErr, removeEmptyDirectory(config.RecoveryDir))
	}
	return cleanupErr
}
