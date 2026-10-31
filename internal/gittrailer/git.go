package gittrailer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HookTimeout bounds Git subprocess work across one hook invocation.
const (
	HookTimeout  = 3 * time.Second
	maxGitOutput = 4 << 20
)

type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxGitOutput-b.buffer.Len() {
		return 0, errors.New("git output exceeds byte limit")
	}
	return b.buffer.Write(p)
}

func runGit(ctx context.Context, dir string, env []string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	cancelGitGroup(cmd)
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}

func commentPrefix(ctx context.Context) (string, error) {
	// These settings are aliases. Reading them separately loses precedence
	// between files, includes, worktree settings and command-scope overrides.
	out, err := runGit(ctx, "", nil, nil, "config", "--null", "--get-regexp", `^core\.(commentchar|commentstring)$`)
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return "#", nil
	}
	if err != nil {
		return "", errors.New("cannot read Git comment setting")
	}
	stringSupported := true
	if bytes.Contains(out, []byte("core.commentstring\n")) {
		// Older Git retains unknown keys in config output but ignores this
		// alias. Probe behavior instead of assuming a version boundary.
		probeText := []byte("// agentsview-comment-probe\n")
		probe, err := runGit(ctx, "", nil, probeText, "-c", "core.commentChar=#", "-c", "core.commentString=//", "stripspace", "--strip-comments")
		if err != nil {
			return "", errors.New("cannot check Git comment alias support")
		}
		switch {
		case len(probe) == 0:
		case bytes.Equal(probe, probeText):
			stringSupported = false
		default:
			return "", errors.New("unexpected Git comment alias behavior")
		}
	}
	prefix := "#"
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(entry, []byte("\n"))
		if !ok {
			return "", errors.New("invalid Git comment setting")
		}
		if !stringSupported && string(key) == "core.commentstring" {
			continue
		}
		prefix = string(value)
	}
	if prefix == "auto" {
		return "", errors.New("automatic Git comment prefixes are unsupported; skipping trailer")
	}
	if prefix == "" || strings.ContainsAny(prefix, "\r\n\x00") {
		return "", errors.New("invalid Git comment prefix")
	}
	return prefix, nil
}

func replaying(ctx context.Context) (bool, error) {
	out, err := runGit(ctx, "", nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge", "--git-path", "rebase-apply", "--git-path", "CHERRY_PICK_HEAD")
	if err != nil {
		return false, errors.New("cannot resolve Git replay state")
	}
	paths := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(paths) != 3 {
		return false, errors.New("cannot resolve Git replay paths")
	}
	for _, path := range paths {
		_, err := os.Lstat(path)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("cannot inspect Git replay state")
		}
	}
	return false, nil
}

// isolatedGitEnv prevents local, included, command-line, system and global
// trailer configuration from changing validated provenance or running commands.
func isolatedGitEnv(dir string) []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_COUNT=0", "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
}

func formatTrailer(ctx context.Context, head []byte, prefix string, session Session) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agentsview-trailer-")
	if err != nil {
		return nil, errors.New("cannot create isolated trailer formatter")
	}
	defer os.RemoveAll(dir)
	key := "core.commentString="
	if len(prefix) == 1 {
		key = "core.commentChar="
	}
	out, err := runGit(ctx, dir, isolatedGitEnv(dir), head, "-c", key+prefix, "interpret-trailers", "--no-divider", "--if-exists", "add", "--if-missing", "add", "--where", "end", "--trailer", "Agent-Session: "+session.Agent+":"+session.ID)
	if err != nil {
		return nil, errors.New("cannot format session trailer")
	}
	if !containsSession(out, session) {
		return nil, errors.New("formatter did not produce the session trailer")
	}
	return out, nil
}
