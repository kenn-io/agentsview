package main

import (
	"errors"
	"os"
)

func execForegroundRestart(next *foregroundRestartError) error {
	exe := os.Args[0]
	release := next.release
	defer func() { release() }()
	for {
		os.Args = append([]string{exe}, next.args...)
		err := executeCLI()
		restart, ok := errors.AsType[*foregroundRestartError](err)
		if !ok {
			return err
		}
		release()
		next = restart
		release = next.release
	}
}
