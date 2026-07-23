package document

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	docsave "github.com/moresleep512/docengine/document/save"
)

const (
	crashProcessEnvironment = "DOCENGINE_PERSISTENCE_CRASH_CHILD"
	crashProcessExitCode    = 86
)

func TestPersistenceCrashProcessMatrix(t *testing.T) {
	tests := []struct {
		stage         string
		wantDisk      string
		wantRecovered string
		recovered     bool
	}{
		{stage: "wal-after-append", wantDisk: "a", wantRecovered: "a1", recovered: true},
		{stage: "save-before-replace-concurrent", wantDisk: "a", wantRecovered: "a12", recovered: true},
		{stage: "save-after-replace", wantDisk: "a1", wantRecovered: "a1"},
		{stage: "save-after-replace-concurrent", wantDisk: "a1", wantRecovered: "a12", recovered: true},
	}
	for _, test := range tests {
		t.Run(test.stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "doc")
			recoveryDir := filepath.Join(dir, "recovery")
			if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestPersistenceCrashProcessChild$")
			command.Env = append(os.Environ(),
				crashProcessEnvironment+"="+test.stage,
				"DOCENGINE_CRASH_PATH="+path,
				"DOCENGINE_CRASH_RECOVERY="+recoveryDir,
				"DOCENGINE_CRASH_SESSION="+filepath.Join(dir, "child-session"),
			)
			output, err := command.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != crashProcessExitCode {
				t.Fatalf("child = %v, output=%s", err, output)
			}
			if disk, err := os.ReadFile(path); err != nil || string(disk) != test.wantDisk {
				t.Fatalf("disk after crash = %q, %v; want %q", disk, err, test.wantDisk)
			}
			reopened, err := Open(path, OpenOptions{
				RecoveryDir:         recoveryDir,
				SessionDir:          filepath.Join(dir, "parent-session"),
				JournalSyncInterval: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := compactSessionContent(t, reopened); got != test.wantRecovered {
				_ = reopened.Close()
				t.Fatalf("recovered content = %q, want %q", got, test.wantRecovered)
			}
			if reopened.Metadata().Recovered != test.recovered {
				_ = reopened.Close()
				t.Fatalf("Recovered = %v, want %v; metadata=%+v", reopened.Metadata().Recovered, test.recovered, reopened.Metadata())
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistenceCrashProcessChild(t *testing.T) {
	stage := os.Getenv(crashProcessEnvironment)
	if stage == "" {
		return
	}
	path := os.Getenv("DOCENGINE_CRASH_PATH")
	options := OpenOptions{
		RecoveryDir:         os.Getenv("DOCENGINE_CRASH_RECOVERY"),
		SessionDir:          os.Getenv("DOCENGINE_CRASH_SESSION"),
		JournalSyncInterval: time.Hour,
	}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
		t.Fatal(err)
	}
	if stage == "wal-after-append" {
		if err := session.journal.Sync(); err != nil {
			t.Fatal(err)
		}
		os.Exit(crashProcessExitCode)
	}

	concurrent := stage == "save-before-replace-concurrent" || stage == "save-after-replace-concurrent"
	var inserted atomic.Bool
	if concurrent {
		session.commitHook = func(stage string) {
			if stage == "snapshot" && inserted.CompareAndSwap(false, true) {
				if _, applyErr := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "2"}}); applyErr != nil {
					panic(applyErr)
				}
			}
		}
	}
	switch stage {
	case "save-before-replace-concurrent":
		session.operations.atomicChecked = func(path string, mode os.FileMode, prefix []byte, write func(io.Writer) (int64, error), check func() error) (int64, error) {
			return docsave.AtomicChecked(path, mode, prefix, write, func() error {
				if err := check(); err != nil {
					return err
				}
				os.Exit(crashProcessExitCode)
				return nil
			})
		}
	case "save-after-replace", "save-after-replace-concurrent":
		session.operations.atomicChecked = func(path string, mode os.FileMode, prefix []byte, write func(io.Writer) (int64, error), check func() error) (int64, error) {
			total, atomicErr := docsave.AtomicChecked(path, mode, prefix, write, check)
			if atomicErr == nil {
				os.Exit(crashProcessExitCode)
			}
			return total, atomicErr
		}
	default:
		t.Fatalf("unknown crash stage %q", stage)
	}
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("save returned instead of crashing at %s (exit %s)", stage, strconv.Itoa(crashProcessExitCode))
}
