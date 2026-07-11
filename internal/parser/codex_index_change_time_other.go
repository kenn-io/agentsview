//go:build !darwin && !linux && !windows

package parser

import "os"

func codexIndexChangeTime(_ string, _ os.FileInfo) (int64, bool) {
	return 0, false
}
