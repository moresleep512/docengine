//go:build windows

package recovery

import (
	"path/filepath"
	"strings"
)

func normalizeFingerprintPath(path string) string {
	return strings.ToLower(filepath.Clean(path))
}
