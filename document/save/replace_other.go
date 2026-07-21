//go:build !windows

package save

import "os"

func replaceFile(from, to string) error { return os.Rename(from, to) }
