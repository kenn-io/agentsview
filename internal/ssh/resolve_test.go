package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildResolveScript(t *testing.T) {
	script := buildResolveScript()

	// Claude has CLAUDE_PROJECTS_DIR env var — must be referenced.
	assert.Contains(t, script, "CLAUDE_PROJECTS_DIR")

	// Non-file-based agents must not appear.
	for _, def := range parser.Registry {
		if def.FileBased || def.DiscoverFunc != nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		assert.NotContains(t, script, marker,
			"non-file-based agent %s in script", def.Type)
	}

	// Every file-based agent with DiscoverFunc must appear.
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		assert.Contains(t, script, marker,
			"file-based agent %s missing from script", def.Type)
	}
}

func TestResolveScriptExitsZero(t *testing.T) {
	// The resolve script must exit 0 even when no agent
	// dirs exist. Verify by running it against an empty
	// HOME so no default dirs are found.
	script := buildResolveScript()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=/nonexistent"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve script failed: output: %s", out)
	// No dirs should be found.
	assert.Empty(t, strings.TrimSpace(string(out)))
}

// TestResolveScriptIncludesCodexIndex verifies the resolve script emits the
// Codex session_index.jsonl as an extra file when it exists, so renamed
// titles get transferred and imported during remote SSH sync. Runs the real
// script through sh against a temp HOME rather than mocking it.
func TestResolveScriptIncludesCodexIndex(t *testing.T) {
	home := t.TempDir()
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755), "mkdir sessions")
	indexPath := filepath.Join(home, ".codex", "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte("{}\n"), 0o644), "write index")

	script := buildResolveScript()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + home}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve script failed: output: %s", out)

	// The script runs in a POSIX shell (MSYS on Windows), so it emits
	// forward-slash paths that differ from native filepath.Join output.
	// Match by POSIX suffix, which also guards against the parent
	// expansion collapsing the index path to /session_index.jsonl.
	dirs, extraFiles := parseResolvedDirs(string(out))
	assert.Truef(t, hasSuffix(dirs[parser.AgentCodex], ".codex/sessions"),
		"codex sessions dir should be resolved, got %v", dirs[parser.AgentCodex])
	assert.Truef(t, hasSuffix(extraFiles, ".codex/session_index.jsonl"),
		"codex session_index.jsonl should be an extra file, got %v", extraFiles)
}

// hasSuffix reports whether any element of paths ends with suffix.
func hasSuffix(paths []string, suffix string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// TestResolveScriptSkipsMissingCodexIndex verifies that a missing index
// produces no extra-file entry, so the transfer's tar command never names a
// nonexistent path (which would be a fatal, non-benign error).
func TestResolveScriptSkipsMissingCodexIndex(t *testing.T) {
	home := t.TempDir()
	require.NoError(t,
		os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755),
		"mkdir sessions")

	script := buildResolveScript()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + home}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve script failed: output: %s", out)

	_, extraFiles := parseResolvedDirs(string(out))
	assert.Empty(t, extraFiles,
		"no extra files when session_index.jsonl is absent")
}

func TestParseResolvedDirs(t *testing.T) {
	input := "claude:/home/wes/.claude/projects\n" +
		"codex:/home/wes/.codex/sessions\n" +
		"codex:\n" +
		"copilot:/home/wes/.copilot\n" +
		"@file:/home/wes/.codex/session_index.jsonl\n" +
		"@file:/home/wes/.codex/session_index.jsonl\n" +
		"\n"

	dirs, extraFiles := parseResolvedDirs(input)

	// codex has one valid dir and one empty (excluded) entry.
	assert.Equal(t, []string{"/home/wes/.codex/sessions"}, dirs[parser.AgentCodex])

	// claude and copilot present.
	assert.Equal(t, []string{"/home/wes/.claude/projects"}, dirs[parser.AgentClaude])
	assert.Equal(t, []string{"/home/wes/.copilot"}, dirs[parser.AgentCopilot])

	assert.Len(t, dirs, 3)

	// The duplicate index file line is deduplicated.
	assert.Equal(t,
		[]string{"/home/wes/.codex/session_index.jsonl"}, extraFiles)
}
