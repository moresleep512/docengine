//go:build !windows

package save

import (
	"errors"
	"testing"
)

func TestReplaceFileDurabilityFailureBoundaries(t *testing.T) {
	sentinel := errors.New("injected")
	tests := []struct {
		name      string
		renameErr error
		openErr   error
		directory *fakeDirectory
		committed bool
	}{
		{name: "rename", renameErr: sentinel},
		{name: "open parent", openErr: sentinel, committed: true},
		{name: "sync parent", directory: &fakeDirectory{syncErr: sentinel}, committed: true},
		{name: "close parent", directory: &fakeDirectory{closeErr: sentinel}, committed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			renames, opens := 0, 0
			operations := replaceOperations{
				rename: func(from, to string) error {
					renames++
					if from != "from" || to != "dir/to" {
						t.Fatalf("rename paths = (%q, %q)", from, to)
					}
					return test.renameErr
				},
				openDir: func(path string) (directoryHandle, error) {
					opens++
					if path != "dir" {
						t.Fatalf("parent = %q", path)
					}
					if test.openErr != nil {
						return nil, test.openErr
					}
					if test.directory == nil {
						return &fakeDirectory{}, nil
					}
					return test.directory, nil
				},
			}
			err := replaceFileWithOperations("from", "dir/to", operations)
			if !errors.Is(err, sentinel) {
				t.Fatalf("replace = %v", err)
			}
			var durability *DurabilityError
			if errors.As(err, &durability) != test.committed || renames != 1 || opens != boolInt(test.committed) {
				t.Fatalf("durability=%+v renames=%d opens=%d", durability, renames, opens)
			}
		})
	}
}

func TestSyncParentSuccessAndFailure(t *testing.T) {
	directory := &fakeDirectory{}
	operations := replaceOperations{openDir: func(string) (directoryHandle, error) { return directory, nil }}
	if err := syncParentWithOperations("dir/doc", operations); err != nil || directory.syncCalls != 1 || directory.closeCalls != 1 {
		t.Fatalf("sync = %v calls=(%d,%d)", err, directory.syncCalls, directory.closeCalls)
	}
	if err := syncParent("dir/does-not-exist/doc"); err == nil {
		t.Fatal("missing parent sync succeeded")
	}
}

type fakeDirectory struct {
	syncErr, closeErr error
	syncCalls         int
	closeCalls        int
}

func (d *fakeDirectory) Sync() error  { d.syncCalls++; return d.syncErr }
func (d *fakeDirectory) Close() error { d.closeCalls++; return d.closeErr }

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
