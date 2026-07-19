package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	assert.Contains(t, script, "CLAUDE_CONFIG_DIR")

	// Only file-backed provider-authoritative agents belong in the resolver.
	for _, def := range parser.Registry {
		want := def.FileBased &&
			parser.ProviderMigrationModes()[def.Type] ==
				parser.ProviderMigrationProviderAuthoritative
		if want {
			assert.True(t, resolveScriptMentionsAgent(script, def.Type),
				"file-backed provider-authoritative agent %s missing from script", def.Type)
			continue
		}
		assert.False(t, resolveScriptMentionsAgent(script, def.Type),
			"unsupported agent %s must stay out of the SSH resolver", def.Type)
	}
}

func resolveScriptMentionsAgent(script string, agent parser.AgentType) bool {
	name := string(agent)
	return strings.Contains(script, "\""+name+":") ||
		strings.Contains(script, " "+name+"\n")
}

func TestResolveScriptExcludesDevinProviderRoot(t *testing.T) {
	home := t.TempDir()
	devinRoot := filepath.Join(home, ".local", "share", "devin")
	require.NoError(t, os.MkdirAll(devinRoot, 0o755))

	out := runResolveScriptForTest(t, "HOME="+home, "DEVIN_DIR="+devinRoot)

	dirs, _ := parseResolvedDirs(string(out))
	assert.NotContains(t, dirs, parser.AgentDevin)
}

func TestResolveScriptHonorsClaudeConfigDirRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "claude personal")
	projectsDir := filepath.Join(root, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755), "mkdir projects")

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"CLAUDE_CONFIG_DIR="+root,
	)

	dirs, _ := parseResolvedDirs(string(out))
	assert.Contains(t, dirs[parser.AgentClaude], root+"/projects")
	assert.NotContains(t, dirs[parser.AgentClaude], home+"/.claude/projects")
}

func TestResolveScriptTreatsEnvValuesAsData(t *testing.T) {
	home := t.TempDir()
	projectsDir := filepath.Join(home, "config root", "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755), "mkdir projects")

	script := buildResolveScript()
	require.NotContains(t, script, "eval")
	out := runResolveScriptForTest(t,
		"HOME="+home,
		"CLAUDE_PROJECTS_DIR="+projectsDir,
	)

	dirs, _ := parseResolvedDirs(string(out))
	assert.Contains(t, dirs[parser.AgentClaude], projectsDir)
}

func TestResolveScriptExitsZero(t *testing.T) {
	// The resolve script must exit 0 even when no agent
	// dirs exist. Verify by running it against an empty
	// HOME so no default dirs are found.
	out := runResolveScriptForTest(t, "HOME=/nonexistent")
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

	out := runResolveScriptForTest(t, "HOME="+home)

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

	out := runResolveScriptForTest(t, "HOME="+home)

	_, extraFiles := parseResolvedDirs(string(out))
	assert.Empty(t, extraFiles,
		"no extra files when session_index.jsonl is absent")
}

// TestResolveScriptSkipsAiderHomeDefault verifies the resolve script does
// NOT infer a bare-$HOME Aider root. The remote resolver tars every emitted
// target, so Aider must stay opt-in even when a history file exists at home
// root. Runs the real script through sh against a temp HOME.
func TestResolveScriptSkipsAiderHomeDefault(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2024-01-01 00:00:00\n"),
		0o644,
	), "write history")

	out := runResolveScriptForTest(t, "HOME="+home)

	dirs, _ := parseResolvedDirs(string(out))
	assert.Empty(t, dirs[parser.AgentAider],
		"aider bare-$HOME default must not be resolved for remote tar, got %v",
		dirs[parser.AgentAider])
	// Guard against $HOME ever appearing as a tar target via aider.
	assert.NotContains(t, string(out), "aider:"+home,
		"aider must not resolve to the whole home dir")
}

// TestResolveScriptAiderScopedByEnvFindsHistoryFiles verifies that an explicit
// AIDER_DIR discovers only aider history files for transfer. The remote sync
// treats resolved entries as tar targets, so emitting the code root would
// archive the entire repository instead of just .aider.chat.history.md files.
func TestResolveScriptAiderScopedByEnvFindsHistoryFiles(t *testing.T) {
	home := t.TempDir()
	codeRoot := filepath.Join(home, "code")
	repoA := filepath.Join(codeRoot, "repo-a")
	repoB := filepath.Join(codeRoot, "nested", "repo-b")
	require.NoError(t, os.MkdirAll(repoA, 0o755), "mkdir repo A")
	require.NoError(t, os.MkdirAll(repoB, 0o755), "mkdir repo B")
	historyA := filepath.Join(repoA, parser.AiderHistoryFileName())
	historyB := filepath.Join(repoB, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(historyA, []byte("# aider\n"), 0o644))
	require.NoError(t, os.WriteFile(historyB, []byte("# aider\n"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoA, "source.go"), []byte("package main\n"), 0o644,
	))
	skippedDir := filepath.Join(codeRoot, "node_modules", "dep")
	require.NoError(t, os.MkdirAll(skippedDir, 0o755), "mkdir skipped dir")
	skippedHistory := filepath.Join(skippedDir, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(skippedHistory, []byte("# aider\n"), 0o644))
	deepDir := filepath.Join(codeRoot, "a", "b", "c", "d", "e")
	require.NoError(t, os.MkdirAll(deepDir, 0o755), "mkdir deep dir")
	deepHistory := filepath.Join(deepDir, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(deepHistory, []byte("# aider\n"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home, "AIDER_DIR="+codeRoot)

	dirs, _ := parseResolvedDirs(string(out))
	aiderTargets := slashPaths(dirs[parser.AgentAider])
	assert.ElementsMatch(t, []string{filepath.ToSlash(historyA), filepath.ToSlash(historyB)}, aiderTargets,
		"explicit AIDER_DIR must resolve only aider history files")
	assert.NotContains(t, aiderTargets, filepath.ToSlash(codeRoot),
		"AIDER_DIR itself must not become a tar target")
	assert.NotContains(t, aiderTargets, filepath.ToSlash(skippedHistory),
		"remote aider discovery must prune local-discovery skip dirs")
	assert.NotContains(t, aiderTargets, filepath.ToSlash(deepHistory),
		"remote aider discovery must enforce the local depth cap")
}

func TestResolveScriptAiderNewlinePathCannotInjectTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows path APIs reject embedded newlines; this regression covers POSIX remote shell output")
	}
	home := t.TempDir()
	codeRoot := filepath.Join(home, "code")
	injected := "/home/victim/" + parser.AiderHistoryFileName()
	maliciousDir := filepath.Join(codeRoot, "repo\naider:", "home", "victim")
	require.NoError(t, os.MkdirAll(maliciousDir, 0o755), "mkdir malicious dir")
	maliciousHistory := filepath.Join(maliciousDir, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(maliciousHistory, []byte("# aider\n"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home, "AIDER_DIR="+codeRoot)

	dirs, _ := parseResolvedDirs(string(out))
	assert.NotContains(t, dirs[parser.AgentAider], injected,
		"newline-bearing repository paths must not inject a second transfer target")
	for _, target := range dirs[parser.AgentAider] {
		assert.NotContains(t, target, "\n",
			"aider transfer target must not contain record separators")
	}
}

func slashPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.ToSlash(p)
	}
	return out
}

// TestResolveScriptAiderRejectsHomeOverride verifies that setting AIDER_DIR
// to literal $HOME (the very thing the home-default skip prevents) is also
// dropped, so an unscoped override cannot reintroduce a whole-home tar.
func TestResolveScriptAiderRejectsHomeOverride(t *testing.T) {
	home := t.TempDir()

	for _, override := range []string{home, home + "/"} {
		out := runResolveScriptForTest(t, "HOME="+home, "AIDER_DIR="+override)

		dirs, _ := parseResolvedDirs(string(out))
		assert.Empty(t, dirs[parser.AgentAider],
			"AIDER_DIR=%q (== $HOME) must not resolve to a whole-home tar, got %v",
			override, dirs[parser.AgentAider])
	}
}

func TestResolveScriptWindsurfTargetsOnlySessionFiles(t *testing.T) {
	home := t.TempDir()
	userRoot := filepath.Join(home, "AppData", "Roaming", "Windsurf", "User")
	workspaceRoot := filepath.Join(userRoot, "workspaceStorage")
	workspaceDir := filepath.Join(workspaceRoot, "workspace-a")
	stateDB := filepath.Join(workspaceDir, parser.WindsurfStateDBName)
	stateWAL := stateDB + "-wal"
	stateSHM := stateDB + "-shm"
	workspaceJSON := filepath.Join(workspaceDir, "workspace.json")
	secretPath := filepath.Join(workspaceDir, "extension-secret.json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(stateDB, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(stateWAL, []byte("wal"), 0o644))
	require.NoError(t, os.WriteFile(stateSHM, []byte("shm"), 0o644))
	require.NoError(t, os.WriteFile(workspaceJSON, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(secretPath, []byte("secret"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home)

	records := resolveOutputRecords(string(out))
	userRootSuffix := filepath.ToSlash(filepath.Join("AppData", "Roaming", "Windsurf", "User"))
	workspaceRootSuffix := filepath.ToSlash(filepath.Join(userRootSuffix, "workspaceStorage"))
	workspaceSuffix := filepath.ToSlash(filepath.Join(workspaceRootSuffix, "workspace-a"))
	agentFilePrefix := resolveAgentFilePrefix + ":" + string(parser.AgentWindsurf)
	assert.True(t, hasRecordWithPathSuffix(records, string(parser.AgentWindsurf), userRootSuffix))
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		filepath.ToSlash(filepath.Join(workspaceSuffix, parser.WindsurfStateDBName))))
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		filepath.ToSlash(filepath.Join(workspaceSuffix, parser.WindsurfStateDBName+"-wal"))))
	assert.False(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		filepath.ToSlash(filepath.Join(workspaceSuffix, parser.WindsurfStateDBName+"-shm"))))
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		filepath.ToSlash(filepath.Join(workspaceSuffix, "workspace.json"))))
	assert.False(t, hasRecordWithPathSuffix(records, string(parser.AgentWindsurf), workspaceRootSuffix))
	assert.False(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		filepath.ToSlash(filepath.Join(workspaceSuffix, filepath.Base(secretPath)))))
}

func hasRecordWithPathSuffix(records []string, prefix, suffix string) bool {
	for _, record := range records {
		if strings.HasPrefix(record, prefix+":") && strings.HasSuffix(record, suffix) {
			return true
		}
	}
	return false
}

func runResolveScriptForTest(t *testing.T, env ...string) []byte {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(buildResolveScript())
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve script failed: output: %s", out)
	return out
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

func TestParseResolvedDirsNULRecords(t *testing.T) {
	input := "claude:/home/wes/.claude/projects\x00" +
		"aider:/home/wes/code/repo/.aider.chat.history.md\x00" +
		"@file:/home/wes/.codex/session_index.jsonl\x00"

	dirs, extraFiles := parseResolvedDirs(input)

	assert.Equal(t, []string{"/home/wes/.claude/projects"}, dirs[parser.AgentClaude])
	assert.Equal(t,
		[]string{"/home/wes/code/repo/.aider.chat.history.md"},
		dirs[parser.AgentAider])
	assert.Equal(t,
		[]string{"/home/wes/.codex/session_index.jsonl"}, extraFiles)
}

func TestParseResolvedTargetsIncludesAgentFiles(t *testing.T) {
	input := "windsurf:/home/wes/Windsurf/User\x00" +
		"@agentfile:windsurf:/home/wes/Windsurf/User/workspaceStorage/a/state.vscdb\x00" +
		"@agentfile:windsurf:/home/wes/Windsurf/User/workspaceStorage/a/state.vscdb\x00" +
		"@agentfile:windsurf:/home/wes/Windsurf/User/workspaceStorage/a/workspace.json\x00" +
		"@file:/home/wes/.codex/session_index.jsonl\x00"

	dirs, files, extraFiles := parseResolvedTargets(input)

	assert.Equal(t, []string{"/home/wes/Windsurf/User"}, dirs[parser.AgentWindsurf])
	assert.Equal(t, []string{
		"/home/wes/Windsurf/User/workspaceStorage/a/state.vscdb",
		"/home/wes/Windsurf/User/workspaceStorage/a/workspace.json",
	}, files[parser.AgentWindsurf])
	assert.Equal(t,
		[]string{"/home/wes/.codex/session_index.jsonl"}, extraFiles)
}
