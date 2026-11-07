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
	// Integration steps name this path, so it must not depend on the caller's directory.
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Install adds template tracking and session trailers without replacing hooks.
// The hooks only work as a pair, so it writes nothing when either needs manual
// integration and explains how to set up both.
func Install(repoDir string) error {
	type hookSpec struct {
		name, line, script, path string
		state                    hookState
		executable               bool
	}
	hooks := []hookSpec{
		{name: "prepare-commit-msg", line: prepareHookLine, script: prepareHookScript},
		{name: "commit-msg", line: hookLine, script: hookScript},
	}
	conflict := false
	for i := range hooks {
		path, err := hookPath(repoDir, hooks[i].name)
		if err != nil {
			return err
		}
		hooks[i].path = path
		if hooks[i].state, hooks[i].executable, err = existingHook(path, hooks[i].script); err != nil {
			return err
		}
		conflict = conflict || hooks[i].state == hookOther
	}
	if conflict {
		var b strings.Builder
		b.WriteString("existing Git hooks need manual integration; nothing was installed")
		for _, hook := range hooks {
			switch hook.state {
			case hookOther:
				fmt.Fprintf(&b, "\nadd this line to %s:\n%s", hook.path, hook.line)
			case hookMissing:
				fmt.Fprintf(&b, "\ncreate executable %s containing:\n%s", hook.path, strings.TrimSuffix(hook.script, "\n"))
				continue
			case hookInstalled:
			}
			if !hook.executable {
				fmt.Fprintf(&b, "\nmake %s executable", hook.path)
			}
		}
		return errors.New(b.String())
	}
	for _, hook := range hooks {
		if err := installHook(hook.path, hook.script); err != nil {
			return err
		}
	}
	return nil
}

type hookState int

const (
	hookMissing hookState = iota
	hookInstalled
	hookOther
)

func installHook(path, script string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if errors.Is(err, os.ErrExist) {
		state, _, err := existingHook(path, script)
		if err != nil {
			return err
		}
		if state != hookInstalled {
			return fmt.Errorf("%s changed during installation", path)
		}
		return os.Chmod(path, 0o755)
	}
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(script)
	return errors.Join(writeErr, file.Close())
}

// existingHook reports whether the hook matches script and whether Git can run it.
func existingHook(path, script string) (hookState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hookMissing, false, nil
	}
	if err != nil {
		return hookMissing, false, err
	}
	state := hookOther
	if string(data) == script {
		state = hookInstalled
	}
	return state, executable(path), nil
}
