//go:build linux || dragonfly || openbsd

package document

import (
	"os"
	"syscall"
)

func captureFileChange(_ readStatFile, info os.FileInfo) (fileChangeStamp, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileChangeStamp{}, nil
	}
	return fileChangeStamp{first: stat.Ctim.Sec, second: stat.Ctim.Nsec, available: true}, nil
}
