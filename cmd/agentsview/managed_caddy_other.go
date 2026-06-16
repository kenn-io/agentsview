//go:build !windows

package main

import "os/exec"

// newCaddyGuard is a no-op on POSIX. `serve stop` terminates the server with
// SIGTERM, so the server's own shutdown stops the managed Caddy child; there is
// nothing extra to confine.
func newCaddyGuard(*exec.Cmd) (caddyGuard, error) {
	return noopCaddyGuard{}, nil
}
