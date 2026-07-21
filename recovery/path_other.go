//go:build !windows

package recovery

import "path/filepath"

func normalizeFingerprintPath(path string) string {
	return filepath.Clean(path)
}
