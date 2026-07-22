//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package document

import "os"

func captureFileChange(_ readStatFile, _ os.FileInfo) (fileChangeStamp, error) {
	return fileChangeStamp{}, nil
}
