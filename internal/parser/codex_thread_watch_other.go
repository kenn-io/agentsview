//go:build !windows && !linux

package parser

import "os"

type codexThreadWatch struct{}

type codexThreadWatchCache struct{}

func (*codexThreadWatchCache) prepare() error { return nil }

func (d *codexThreadDirectory) refreshWatch(os.FileInfo, *codexThreadWatchCache) error {
	return nil
}

func (*codexThreadWatch) close() {}
