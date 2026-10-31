//go:build !unix

package gittrailer

import "os/exec"

// CommandContext kills the direct child on platforms without Unix groups.
func cancelGitGroup(cmd *exec.Cmd) {}
