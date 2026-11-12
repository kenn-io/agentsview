//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func execForegroundRestart(r *foregroundRestartError) error {
	return fmt.Errorf("restarting foreground server: %w", syscall.Exec(r.exe, append([]string{r.exe}, r.args...), os.Environ()))
}
