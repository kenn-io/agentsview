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

const (
	prepareHookLine   = `if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer-prepare "$@" || :; fi`
	prepareHookScript = "#!/bin/sh\n" + prepareHookLine + "\n"
	hookLine          = `if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer "$1" || :; fi`
	hookScript        = "#!/bin/sh\n" + hookLine + "\n"
)

func hookPath(repoDir, name string) (string, error) {
	out, err := (gitcmd.Runner{}).Output(context.Background(), repoDir, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoDir, dir)
	}
	return filepath.Join(dir, name), nil
}

// Install adds template tracking and session trailers without replacing hooks.
func Install(repoDir string) error {
	type hookSpec struct {
		name, line, script, path string
	}
	hooks := []hookSpec{
		{name: "prepare-commit-msg", line: prepareHookLine, script: prepareHookScript},
		{name: "commit-msg", line: hookLine, script: hookScript},
	}
	for i := range hooks {
		path, err := hookPath(repoDir, hooks[i].name)
		if err != nil {
			return err
		}
		hooks[i].path = path
		if _, err := existingHook(path, hooks[i].name, hooks[i].script, hooks[i].line); err != nil {
			return err
		}
	}
	for _, hook := range hooks {
		if err := installHook(hook.path, hook.name, hook.script, hook.line); err != nil {
			return err
		}
	}
	return nil
}

func installHook(path, name, script, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if errors.Is(err, os.ErrExist) {
		return checkExistingHook(path, name, script, line)
	}
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(script)
	return errors.Join(writeErr, file.Close())
}

func existingHook(path, name, script, line string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(data) == script {
		return true, nil
	}
	return false, fmt.Errorf("%s already exists; add this line to it:\n%s", name, line)
}

func checkExistingHook(path, name, script, line string) error {
	exists, err := existingHook(path, name, script, line)
	if err != nil || !exists {
		return err
	}
	return os.Chmod(path, 0o755)
}
