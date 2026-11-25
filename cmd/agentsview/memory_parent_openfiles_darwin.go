package main

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
)

// gopsutil does not implement OpenFiles on macOS.
func launchOpenFiles(p *process.Process) func(context.Context) ([]process.OpenFilesStat, error) {
	return func(ctx context.Context) ([]process.OpenFilesStat, error) {
		out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-p", strconv.Itoa(int(p.Pid)), "-Fn0").Output()
		if err != nil {
			return nil, err
		}
		var files []process.OpenFilesStat
		for _, field := range bytes.Split(out, []byte{0}) {
			field = bytes.TrimPrefix(field, []byte{'\n'})
			if len(field) > 1 && field[0] == 'n' {
				files = append(files, process.OpenFilesStat{Path: string(field[1:])})
			}
		}
		return files, nil
	}
}
