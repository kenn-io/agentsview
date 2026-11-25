package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/shellquote"
)

var nodeShellExec = regexp.MustCompile(`(?m)^\s*exec\s+node(?:\s|$)`)

// lookupIsolation hides the operator's home from the agent while leaving the
// isolated corpus and the selected client executable available.
type lookupIsolation struct {
	command      string
	env          []string
	providerRoot string
	credentials  []string
}

func lookupExecutable(ctx context.Context, client string) (string, error) {
	path, err := exec.LookPath(client)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Base(resolved) == "mise" {
		cmd := exec.CommandContext(ctx, resolved, "which", client)
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	return resolved, nil
}

func prepareLookupIsolation(ctx context.Context, client clientName, clientDir, repository, project, bin, runDir string) (result lookupIsolation, err error) {
	var credentials []string
	defer func() {
		if err != nil {
			lookupIsolation{credentials: credentials}.removeCredentials()
		}
	}()
	if runtime.GOOS != "linux" {
		return lookupIsolation{}, errors.New("lookup isolation requires Linux bubblewrap")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return lookupIsolation{}, fmt.Errorf("lookup isolation requires bubblewrap: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return lookupIsolation{}, err
	}
	if err := requireHiddenLookupPaths(home, clientDir, repository); err != nil {
		return lookupIsolation{}, err
	}
	executable, err := lookupExecutable(ctx, string(client))
	if err != nil {
		return lookupIsolation{}, err
	}
	node := ""
	needsNode, err := lookupExecutableNeedsNode(executable)
	if err != nil {
		return lookupIsolation{}, err
	}
	if needsNode {
		node, err = lookupExecutable(ctx, "node")
		if err != nil {
			return lookupIsolation{}, err
		}
	}
	privateHome := filepath.Join(runDir, "home")
	providerHome := filepath.Join(privateHome, ".codex")
	if client == clientClaude {
		providerHome = filepath.Join(privateHome, ".claude")
	}
	if err := os.MkdirAll(providerHome, 0o700); err != nil {
		return lookupIsolation{}, err
	}
	if err := os.Symlink(filepath.Join(clientDir, "agentsview-data"), filepath.Join(privateHome, ".agentsview")); err != nil {
		return lookupIsolation{}, err
	}
	copyCredential := func(source, dest string) error {
		data, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			return fmt.Errorf("lookup isolation: missing %s credentials", client)
		}
		if err != nil {
			return err
		}
		credentials = append(credentials, dest)
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return err
		}
		return nil
	}
	if client == clientCodex {
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		if err := copyCredential(filepath.Join(codexHome, "auth.json"), filepath.Join(providerHome, "auth.json")); err != nil {
			return lookupIsolation{}, err
		}
	} else {
		claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
		if claudeHome == "" {
			claudeHome = filepath.Join(home, ".claude")
		}
		if err := copyCredential(filepath.Join(claudeHome, ".credentials.json"), filepath.Join(providerHome, ".credentials.json")); err != nil {
			return lookupIsolation{}, err
		}
		if err := os.WriteFile(filepath.Join(privateHome, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0o600); err != nil {
			return lookupIsolation{}, err
		}
	}
	root := filepath.Join(home, ".local", "share", "mise", "installs")
	dataDir := filepath.Join(clientDir, "agentsview-data")
	mounts := []string{project, dataDir, privateHome, bin}
	if _, err := os.Stat(root); err == nil && (strings.HasPrefix(executable, root+string(os.PathSeparator)) || strings.HasPrefix(node, root+string(os.PathSeparator))) {
		mounts = append(mounts, root)
	}
	for _, p := range []string{executable, node} {
		if strings.HasPrefix(p, home+string(os.PathSeparator)) && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
			mounts = append(mounts, filepath.Dir(p))
		}
	}
	dirs := map[string]bool{}
	for _, p := range mounts {
		for parent := filepath.Dir(p); strings.HasPrefix(parent, home+string(os.PathSeparator)); parent = filepath.Dir(parent) {
			dirs[parent] = true
		}
	}
	var parents []string
	for p := range dirs {
		parents = append(parents, p)
	}
	sort.Slice(parents, func(i, j int) bool { return len(parents[i]) < len(parents[j]) })
	argv := []string{bwrap, "--die-with-parent", "--unshare-pid", "--ro-bind", "/", "/", "--tmpfs", home, "--tmpfs", "/tmp", "--proc", "/proc"}
	for _, p := range parents {
		argv = append(argv, "--dir", p)
	}
	argv = append(argv, "--bind", project, project, "--bind", dataDir, dataDir, "--bind", privateHome, privateHome, "--ro-bind", bin, bin)
	for _, p := range mounts[4:] {
		argv = append(argv, "--ro-bind", p, p)
	}
	argv = append(argv, "--chdir", project, executable)
	var script strings.Builder
	script.WriteString("#!/bin/sh\nexec")
	for _, arg := range argv {
		script.WriteByte(' ')
		script.WriteString(shellquote.POSIXLiteral(arg))
	}
	script.WriteString(" \"$@\"\n")
	wrapper := filepath.Join(runDir, string(client))
	if err := os.WriteFile(wrapper, []byte(script.String()), 0o700); err != nil {
		return lookupIsolation{}, err
	}
	path := filepath.Dir(bin) + string(os.PathListSeparator)
	if node != "" {
		path += filepath.Dir(node) + string(os.PathListSeparator)
	}
	path += "/usr/bin:/bin"
	env := []string{
		"HOME=" + privateHome,
		"CODEX_HOME=" + filepath.Join(privateHome, ".codex"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(privateHome, ".claude"),
		"AGENTSVIEW_DATA_DIR=" + filepath.Join(clientDir, "agentsview-data"),
		"PATH=" + path,
		"DO_NOT_TRACK=1",
		"TMPDIR=/tmp",
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ", "NO_COLOR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	rootDir := filepath.Join(providerHome, "sessions")
	if client == clientClaude {
		rootDir = filepath.Join(providerHome, "projects")
	}
	return lookupIsolation{command: wrapper, env: env, providerRoot: rootDir, credentials: credentials}, nil
}

func lookupExecutableNeedsNode(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	buf := make([]byte, 4096)
	n, readErr := file.Read(buf)
	closeErr := file.Close()
	if readErr != nil && n == 0 {
		return false, readErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	shebang := string(buf[:n])
	if line, _, ok := strings.Cut(shebang, "\n"); ok {
		shebang = line
	}
	return strings.HasPrefix(shebang, "#!") && (strings.Contains(strings.ToLower(shebang), "node") || nodeShellExec.Match(buf[:n])), nil
}

func copyLookupTranscriptsForClient(client clientName, source, privateHome, project string) error {
	destination, err := lookupTranscriptRoot(client, privateHome, project)
	if err != nil {
		return err
	}
	return os.CopyFS(destination, os.DirFS(source))
}

func lookupTranscriptRoot(client clientName, privateHome, project string) (string, error) {
	switch client {
	case clientCodex:
		return filepath.Join(privateHome, ".codex", "sessions"), nil
	case clientClaude:
		projectName := strings.ReplaceAll(filepath.Clean(project), string(os.PathSeparator), "-")
		return filepath.Join(privateHome, ".claude", "projects", projectName), nil
	default:
		return "", fmt.Errorf("lookup isolation: unsupported client %q", client)
	}
}

func requireHiddenLookupPaths(home string, paths ...string) error {
	allowed := make([]string, 0, 4)
	for _, root := range []string{home, "/tmp"} {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("lookup isolation: resolve hidden root %q: %w", root, err)
		}
		allowed = append(allowed, root)
		allowed = append(allowed, resolved)
	}
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("lookup isolation: resolve %q: %w", path, err)
		}
		hidden := false
		for _, root := range allowed {
			rel, err := filepath.Rel(root, resolved)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
				hidden = true
				break
			}
		}
		if !hidden {
			return fmt.Errorf("lookup isolation: fixture and output paths must be under the home directory or /tmp: %q", path)
		}
	}
	return nil
}

func (i lookupIsolation) removeCredentials() {
	for _, p := range i.credentials {
		_ = os.Remove(p)
	}
}
