//go:build !windows

package parser

import "os"

type codexThreadWatch struct{}

func (d *codexThreadDirectory) refreshWatch(os.FileInfo) error { return nil }

func (*codexThreadWatch) close() {}
