//go:build !darwin

package main

import (
	"context"

	"github.com/shirou/gopsutil/v4/process"
)

func launchOpenFiles(p *process.Process) func(context.Context) ([]process.OpenFilesStat, error) {
	return p.OpenFilesWithContext
}
