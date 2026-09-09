//go:build !darwin && !linux && !windows

package parser

import "os"

func codexIndexChangeTimeForFile(_ *os.File, _ os.FileInfo) (int64, bool) {
	return 0, false
}

func codexIndexChangeTime(_ string, info os.FileInfo) (int64, bool) {
	return codexIndexChangeTimeForFile(nil, info)
}
