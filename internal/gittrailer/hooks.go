package gittrailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var hookNames = []string{"prepare-commit-msg", "commit-msg"}

const (
	markerStart = "# >>> agentsview git-trailer"
	markerEnd   = "# <<< agentsview git-trailer v1\n"
)

// HookStatus describes whether one hook contains an intact AgentsView block.
type HookStatus struct {
	Name      string
	Installed bool
}

func managedBlock(name string, created bool) []byte {
	kind := "existing"
	if created {
		kind = "created"
	}
	args := `"${1:-}"`
	if name == "prepare-commit-msg" {
		args += ` "${2:-editor}"`
	} else {
		args += ` commit-msg`
	}
	args += ` "${PPID:-}"`
	return []byte(markerStart + " v1 (" + kind + ")\n(\n  if command -v agentsview >/dev/null 2>&1; then\n    agentsview git-trailer " + args + " || :\n  fi\n  :\n) || :\n" + markerEnd)
}

func removeBlock(data []byte, name string) (remaining []byte, owned, created bool, err error) {
	if data == nil {
		return nil, false, false, nil
	}
	starts := bytes.Count(data, []byte(markerStart))
	ends := bytes.Count(data, []byte("# <<< agentsview git-trailer"))
	if starts == 0 && ends == 0 {
		return data, false, false, nil
	}
	if starts != 1 || ends != 1 {
		return nil, false, false, errors.New("malformed AgentsView hook markers")
	}
	for _, isCreated := range []bool{false, true} {
		block := managedBlock(name, isCreated)
		if before, after, found := bytes.Cut(data, block); found {
			remaining = make([]byte, 0, len(data)-len(block))
			remaining = append(remaining, before...)
			remaining = append(remaining, after...)
			return remaining, true, isCreated, nil
		}
	}
	return nil, false, false, errors.New("modified AgentsView hook block; leaving it untouched")
}

func shellHook(data []byte) bool {
	line, _, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.HasPrefix(line, []byte("#!")) {
		return false
	}
	fields := strings.Fields(strings.TrimPrefix(string(line), "#!"))
	if len(fields) == 0 {
		return false
	}
	shell := filepath.Base(fields[0])
	if shell == "env" {
		if len(fields) != 2 {
			return false
		}
		shell = fields[1]
	}
	return shell == "sh" || shell == "bash" || shell == "dash"
}

type hookUpdate struct {
	path            string
	data            []byte
	mode            os.FileMode
	remove, changed bool
}

func planHooks(dir string, install bool) ([]hookUpdate, error) {
	var updates []hookUpdate
	for _, name := range hookNames {
		path := filepath.Join(dir, name)
		data, info, err := readRegular(path, maxMessageBytes)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		if data == nil {
			data = make([]byte, 0)
		}
		remaining, owned, created, err := removeBlock(data, name)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		update := hookUpdate{path: path, mode: 0o755}
		if info != nil {
			update.mode = info.Mode()
		}
		if install {
			if !missing && (!shellHook(data) || runtime.GOOS != "windows" && update.mode&0o111 == 0) {
				return nil, fmt.Errorf("%s must be an executable sh, bash or dash script", name)
			}
			if !owned {
				if missing {
					data = []byte("#!/bin/sh\n")
				}
				block := managedBlock(name, missing)
				if len(data) > maxMessageBytes-len(block) {
					return nil, fmt.Errorf("%s would exceed the hook byte limit", name)
				}
				at := bytes.IndexByte(data, '\n') + 1
				update.data = append(update.data, data[:at]...)
				update.data = append(update.data, block...)
				update.data = append(update.data, data[at:]...)
				update.changed = true
			}
		} else if owned {
			update.data = remaining
			update.changed = true
			update.remove = created && bytes.Equal(remaining, []byte("#!/bin/sh\n"))
		}
		updates = append(updates, update)
	}
	return updates, nil
}

// Install prepends guarded blocks to supported shell hooks without changing
// core.hooksPath. Both existing files are validated before either is changed.
func Install(dir string) error {
	updates, err := planHooks(dir, true)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return applyHooks(updates)
}

// Uninstall removes only intact owned blocks, restoring original bytes/modes.
func Uninstall(dir string) error {
	updates, err := planHooks(dir, false)
	if err != nil {
		return err
	}
	return applyHooks(updates)
}

func applyHooks(updates []hookUpdate) error {
	for _, u := range updates {
		if !u.changed {
			continue
		}
		var err error
		if u.remove {
			err = os.Remove(u.path)
		} else {
			err = atomicWrite(u.path, u.data, u.mode)
		}
		if err != nil {
			return fmt.Errorf("update %s (earlier hooks may already be updated): %w", filepath.Base(u.path), err)
		}
	}
	return nil
}

// Status reads hook ownership without installing, chmodding or creating files.
func Status(dir string) ([]HookStatus, error) {
	statuses := make([]HookStatus, 0, len(hookNames))
	for _, name := range hookNames {
		data, _, err := readRegular(filepath.Join(dir, name), maxMessageBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		_, owned, _, err := removeBlock(data, name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, HookStatus{Name: name, Installed: owned})
	}
	return statuses, nil
}

// HooksDir resolves effective repository hooks, or the stable global default.
// Global conditional includes depending on a repository require --repo instead.
func HooksDir(ctx context.Context, repo string, global bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	var out []byte
	var err error
	if global {
		dir, e := os.MkdirTemp("", "agentsview-hooks-")
		if e != nil {
			return "", e
		}
		defer os.RemoveAll(dir)
		env := make([]string, 0, len(os.Environ()))
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			upper := strings.ToUpper(key)
			if !strings.HasPrefix(upper, "GIT_") || upper == "GIT_CONFIG_GLOBAL" || upper == "GIT_CONFIG_SYSTEM" || upper == "GIT_CONFIG_NOSYSTEM" {
				env = append(env, entry)
			}
		}
		env = append(env, "GIT_CONFIG_COUNT=0", "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
		out, err = runGit(ctx, dir, env, nil, "config", "--global", "--includes", "--path", "--get", "core.hooksPath")
		if err != nil {
			return "", errors.New("global core.hooksPath is unavailable; configure it first or use --repo")
		}
	} else {
		if repo == "" {
			repo = "."
		}
		out, err = runGit(ctx, "", nil, nil, "-C", repo, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
		if err != nil {
			return "", errors.New("cannot resolve repository hooks directory")
		}
	}
	path := strings.TrimSuffix(string(out), "\n")
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("hooks directory must resolve to an absolute path; use --repo for relative global hooksPath")
	}
	return filepath.Clean(path), nil
}
