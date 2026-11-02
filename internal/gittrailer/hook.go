package gittrailer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

const hookLine = `if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer "$1" || :; fi`
const hookScript = "#!/bin/sh\n" + hookLine + "\nexit 0\n"

func hookPath(repoDir string) (string, error) {
	out, err := (gitcmd.Runner{}).Output(context.Background(), repoDir, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoDir, dir)
	}
	return filepath.Join(dir, "commit-msg"), nil
}

// Install opts this repository into session trailers without replacing a hook.
func Install(repoDir string) error {
	path, err := hookPath(repoDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("commit-msg already exists; add this line to it:\n%s", hookLine)
	}
	if err != nil {
		return err
	}
	_, err = file.WriteString(hookScript)
	return errors.Join(err, file.Close())
}
