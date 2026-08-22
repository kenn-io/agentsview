//go:build windows

package capture

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveChildCommand(command string) (string, error) {
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".bat", ".cmd":
		return "", fmt.Errorf(
			"Windows capture requires a native producer executable; batch shim %q is unsupported",
			filepath.Base(resolved),
		)
	default:
		return resolved, nil
	}
}
