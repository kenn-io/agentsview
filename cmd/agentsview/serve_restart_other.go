//go:build !unix

package main

import (
	"errors"
	"os"
)

func execForegroundRestart(next *foregroundRestartError) error {
	for {
		os.Args = append([]string{next.exe}, next.args...)
		err := executeCLI()
		restart, ok := errors.AsType[*foregroundRestartError](err)
		if !ok {
			return err
		}
		next = restart
	}
}
