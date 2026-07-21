//go:build !windows

package document

import "os"

func openBase(path string) (*os.File, error) { return os.Open(path) }
