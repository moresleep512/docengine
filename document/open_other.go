//go:build !windows

package document

import (
	"os"
	"path/filepath"
)

func openBase(path string) (*os.File, error) { return os.Open(path) }

func resolvePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
