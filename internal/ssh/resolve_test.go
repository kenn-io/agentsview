package ssh

import (
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
				parser.ProviderMigrationProviderAuthoritative &&
			!def.RemoteSyncExcluded
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
	home := physTempDir(t)
	devinRoot := filepath.Join(home, ".local", "share", "devin")
	require.NoError(t, os.MkdirAll(devinRoot, 0o755))

	out := runResolveScriptForTest(t, "HOME="+home, "DEVIN_DIR="+devinRoot)

	dirs, _, _ := parseResolvedDirs(string(out))
	assert.NotContains(t, dirs, parser.AgentDevin)
}

func TestResolveScriptExcludesTraeProfile(t *testing.T) {
	home := physTempDir(t)
	traeRoot := filepath.Join(home, "AppData", "Roaming", "TRAE", "User")
	claudeRoot := filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(traeRoot, 0o755))
	require.NoError(t, os.MkdirAll(claudeRoot, 0o755))

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+traeRoot)
	dirs, _, _ := parseResolvedDirs(string(out))
	assert.NotContains(t, dirs, parser.AgentTrae)
}

func TestResolveScriptHonorsClaudeConfigDirRoot(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	root := filepath.Join(home, "claude personal")
	projectsDir := filepath.Join(root, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755), "mkdir projects")

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"CLAUDE_CONFIG_DIR="+root,
	)

	dirs, _, _ := parseResolvedDirs(string(out))
	assert.Contains(t, dirs[parser.AgentClaude], root+"/projects")
	assert.NotContains(t, dirs[parser.AgentClaude], home+"/.claude/projects")
}

func TestResolveScriptTreatsEnvValuesAsData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolve script runs on POSIX remote hosts; local Windows filepaths and MSYS shell parsing are not representative")
	}
	home := physTempDir(t)
	projectsDir := filepath.Join(home, "config root", "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755), "mkdir projects")

	script := buildResolveScript()
	require.NotContains(t, script, "eval")
	out := runResolveScriptForTest(t,
		"HOME="+home,
		"CLAUDE_PROJECTS_DIR="+projectsDir,
	)

	dirs, _, _ := parseResolvedDirs(string(out))
	assert.Contains(t, dirs[parser.AgentClaude], projectsDir)
}

func TestResolveScriptExitsZero(t *testing.T) {
	// The resolve script must exit 0 even when no agent
	// dirs exist. Verify by running it against an empty
	// HOME so no default dirs are found.
	out := runResolveScriptForTest(t, "HOME=/nonexistent")
	dirs, files, extraFiles, forbiddenRoots, _ :=
		parseResolvedTargets(string(out))
	assert.Empty(t, dirs)
	assert.Empty(t, files)
	assert.Empty(t, extraFiles)
	assert.True(t, hasSuffix(forbiddenRoots, ".config/Trae/User"),
		"missing excluded roots remain protected if created after resolution")
}

// TestResolveScriptIncludesCodexIndex verifies the resolve script emits the
// Codex session_index.jsonl as an extra file when it exists, so renamed
// titles get transferred and imported during remote SSH sync. Runs the real
// script through sh against a temp HOME rather than mocking it.
func TestResolveScriptIncludesCodexIndex(t *testing.T) {
	home := physTempDir(t)
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755), "mkdir sessions")
	indexPath := filepath.Join(home, ".codex", "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte("{}\n"), 0o644), "write index")

	out := runResolveScriptForTest(t, "HOME="+home)

	// The script runs in a POSIX shell (MSYS on Windows), so it emits
	// forward-slash paths that differ from native filepath.Join output.
	// Match by POSIX suffix, which also guards against the parent
	// expansion collapsing the index path to /session_index.jsonl.
	dirs, extraFiles, _ := parseResolvedDirs(string(out))
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

// TestResolveScriptIncludesHermesNamedProfiles verifies the remote resolve
// script discovers Hermes named-profile session dirs and database files, not
// just the default profile.
func TestResolveScriptIncludesHermesNamedProfiles(t *testing.T) {
	home := physTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".hermes", "sessions"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".hermes", "state.db"), []byte("sqlite"), 0o644,
	))
	orchestratorRoot := filepath.Join(home, ".hermes", "profiles", "orchestrator")
	researchRoot := filepath.Join(home, ".hermes", "profiles", "research")
	require.NoError(t, os.MkdirAll(filepath.Join(orchestratorRoot, "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(researchRoot, "sessions"), 0o755))
	for _, path := range []string{
		filepath.Join(orchestratorRoot, "state.db"),
		filepath.Join(orchestratorRoot, "state.db-wal"),
		filepath.Join(orchestratorRoot, "state.db-shm"),
		filepath.Join(orchestratorRoot, "state.db-journal"),
		filepath.Join(researchRoot, "state.db"),
	} {
		require.NoError(t, os.WriteFile(path, []byte("sqlite"), 0o644))
	}

	out := runResolveScriptForTest(t, "HOME="+home)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.Truef(t, hasSuffix(dirs[parser.AgentHermes], ".hermes/sessions"),
		"default profile sessions dir should resolve, got %v", dirs[parser.AgentHermes])
	assert.Truef(t, hasSuffix(dirs[parser.AgentHermes], ".hermes/profiles/orchestrator/sessions"),
		"orchestrator profile sessions dir should resolve, got %v", dirs[parser.AgentHermes])
	assert.Truef(t, hasSuffix(dirs[parser.AgentHermes], ".hermes/profiles/research/sessions"),
		"research profile sessions dir should resolve, got %v", dirs[parser.AgentHermes])
	assert.True(t, hasSuffix(extraFiles, ".hermes/profiles/orchestrator/state.db"))
	assert.True(t, hasSuffix(extraFiles, ".hermes/profiles/orchestrator/state.db-wal"))
	assert.True(t, hasSuffix(extraFiles, ".hermes/profiles/orchestrator/state.db-shm"))
	assert.True(t, hasSuffix(extraFiles, ".hermes/profiles/orchestrator/state.db-journal"))
	assert.True(t, hasSuffix(extraFiles, ".hermes/profiles/research/state.db"))
	assert.True(t, hasSuffix(extraFiles, ".hermes/state.db"))
}

func TestResolveScriptExcludesRemoteSyncExcludedAgentState(t *testing.T) {
	home := physTempDir(t)
	root := filepath.Join(home, "AppData", "Roaming", "Trae", "User")
	require.NoError(t, os.MkdirAll(root, 0o755))
	for _, name := range []string{
		"chat.db",
		"chat.db-wal",
		"chat.db-shm",
		"chat.db-journal",
	} {
		require.NoError(t,
			os.WriteFile(filepath.Join(root, name), []byte("sqlite"), 0o644))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "credentials.json"), []byte("secret"), 0o600,
	))

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+root)
	dirs, files, _, _, _ := parseResolvedTargets(string(out))

	assert.NotContains(t, dirs, parser.AgentTrae)
	assert.NotContains(t, files, parser.AgentTrae)
}

func TestResolveScriptCarriesTraeForbiddenRoot(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	root := filepath.Join(home, "AppData", "Roaming", "Trae", "User")
	require.NoError(t, os.MkdirAll(root, 0o755))

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+root)
	_, _, _, forbiddenRoots, _ := parseResolvedTargets(string(out))

	assert.Contains(t, forbiddenRoots, root,
		"the SSH resolver must preserve excluded roots as transfer boundaries")
}

func TestResolveScriptCarriesMissingExcludedRoot(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	root := filepath.Join(home, "AppData", "Roaming", "Trae", "User")

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+root)
	_, _, _, forbiddenRoots, _ := parseResolvedTargets(string(out))

	assert.Contains(t, forbiddenRoots, root,
		"the exclusion boundary must survive creation after remote resolution")
}

func TestResolveScriptHermesOverrideReplacesNamedProfiles(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := filepath.ToSlash(physTempDir(t))
	profileSessions := path.Join(
		home, ".hermes", "profiles", "research", "sessions",
	)
	customRoot := path.Join(home, "custom-hermes")
	customSessions := path.Join(customRoot, "sessions")
	require.NoError(t, os.MkdirAll(profileSessions, 0o755))
	require.NoError(t, os.MkdirAll(customSessions, 0o755))
	require.NoError(t, os.WriteFile(
		path.Join(customRoot, "state.db"), []byte("sqlite"), 0o644,
	))

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"HERMES_SESSIONS_DIR="+customSessions,
	)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.Equal(t, []string{customSessions}, dirs[parser.AgentHermes])
	assert.Equal(t, []string{path.Join(customRoot, "state.db")}, extraFiles)
}

func TestResolveScriptHermesProfilesContainerOverrideEnumeratesProfiles(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := filepath.ToSlash(physTempDir(t))
	profilesRoot := path.Join(home, ".hermes", "profiles")
	researchRoot := path.Join(profilesRoot, "research")
	researchSessions := path.Join(researchRoot, "sessions")
	researchStateDB := path.Join(researchRoot, "state.db")
	databaseOnlyRoot := path.Join(profilesRoot, "database-only")
	databaseOnlyStateDB := path.Join(databaseOnlyRoot, "state.db")
	require.NoError(t, os.MkdirAll(researchSessions, 0o755))
	require.NoError(t, os.MkdirAll(databaseOnlyRoot, 0o755))
	require.NoError(t, os.WriteFile(researchStateDB, []byte("sqlite"), 0o644))
	require.NoError(t, os.WriteFile(databaseOnlyStateDB, []byte("sqlite"), 0o644))

	outsideRoot := filepath.ToSlash(path.Join(physTempDir(t), "outside-profile"))
	require.NoError(t, os.MkdirAll(path.Join(outsideRoot, "sessions"), 0o755))
	require.NoError(t, os.Symlink(outsideRoot, path.Join(profilesRoot, "linked")))

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"HERMES_SESSIONS_DIR="+profilesRoot,
	)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.ElementsMatch(t, []string{researchSessions, databaseOnlyStateDB},
		dirs[parser.AgentHermes])
	assert.Equal(t, []string{researchStateDB}, extraFiles)
}

func TestResolveScriptHermesTrailingSlashOverrideIncludesStateDB(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := filepath.ToSlash(physTempDir(t))
	customRoot := path.Join(home, "custom-hermes")
	customSessions := path.Join(customRoot, "sessions")
	require.NoError(t, os.MkdirAll(filepath.FromSlash(customSessions), 0o755))
	stateDB := path.Join(customRoot, "state.db")
	require.NoError(t, os.WriteFile(filepath.FromSlash(stateDB), []byte("sqlite"), 0o644))

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"HERMES_SESSIONS_DIR="+customSessions+"/",
	)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.Equal(t, []string{customSessions}, dirs[parser.AgentHermes])
	assert.Equal(t, []string{stateDB}, extraFiles)
}

func TestResolveScriptIncludesFlatCustomHermesRoot(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	customRoot := filepath.Join(home, "custom", "hermes-archive")
	require.NoError(t, os.MkdirAll(customRoot, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(customRoot, "child.jsonl"), []byte("{}\n"), 0o644,
	))

	out := runResolveScriptForTest(t,
		"HOME="+home,
		"HERMES_SESSIONS_DIR="+customRoot,
	)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.Equal(t, []string{customRoot}, dirs[parser.AgentHermes])
	assert.Empty(t, extraFiles)
}

func TestResolveScriptIncludesHermesDatabaseOnlyProfile(t *testing.T) {
	home := filepath.ToSlash(physTempDir(t))
	profileRoot := path.Join(home, ".hermes", "profiles", "database-only")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	stateDB := path.Join(profileRoot, "state.db")
	require.NoError(t, os.WriteFile(stateDB, []byte("sqlite"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home)
	dirs, _, _ := parseResolvedDirs(string(out))

	assert.Truef(t, hasSuffix(
		dirs[parser.AgentHermes], ".hermes/profiles/database-only/state.db",
	), "database-only profile should resolve, got %v", dirs[parser.AgentHermes])
}

func TestResolveScriptSkipsSessionlessHermesProfileCredentials(t *testing.T) {
	home := physTempDir(t)
	profileRoot := filepath.Join(home, ".hermes", "profiles", "sessions")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileRoot, ".env"), []byte("TOKEN=secret\n"), 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileRoot, "auth.json"), []byte(`{"token":"secret"}`), 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileRoot, "debug.jsonl"), []byte("not a session\n"), 0o600,
	))

	out := runResolveScriptForTest(t, "HOME="+home)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.NotContains(t, dirs[parser.AgentHermes], profileRoot)
	assert.NotContains(t, extraFiles, filepath.Join(profileRoot, ".env"))
	assert.NotContains(t, extraFiles, filepath.Join(profileRoot, "auth.json"))
}

func TestResolveScriptSkipsSymlinkedHermesProfile(t *testing.T) {
	home := physTempDir(t)
	profilesRoot := filepath.Join(home, ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(profilesRoot, 0o755))
	outsideRoot := filepath.Join(physTempDir(t), "outside-profile")
	outsideSessions := filepath.Join(outsideRoot, "sessions")
	require.NoError(t, os.MkdirAll(outsideSessions, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outsideSessions, "credential.jsonl"), []byte("secret\n"), 0o600,
	))
	profileLink := filepath.Join(profilesRoot, "linked")
	require.NoError(t, os.Symlink(outsideRoot, profileLink))

	out := runResolveScriptForTest(t, "HOME="+home)
	dirs, extraFiles, _ := parseResolvedDirs(string(out))

	assert.NotContains(t, dirs[parser.AgentHermes], filepath.Join(profileLink, "sessions"))
	assert.Empty(t, extraFiles)
}

// TestResolveScriptSkipsMissingHermesProfiles verifies that when no profiles/
// dir exists, the glob expands to nothing emittable (av_emit_target's [ -d ]
// guard drops the non-existent path) — so only the default profile resolves.
func TestResolveScriptSkipsMissingHermesProfiles(t *testing.T) {
	home := physTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".hermes", "sessions"), 0o755))

	out := runResolveScriptForTest(t, "HOME="+home)
	dirs, _, _ := parseResolvedDirs(string(out))

	assert.Truef(t, hasSuffix(dirs[parser.AgentHermes], ".hermes/sessions"),
		"default profile sessions dir should resolve, got %v", dirs[parser.AgentHermes])
	// The unexpanded glob must never leak through as a literal path.
	assert.Falsef(t, hasSuffix(dirs[parser.AgentHermes], "profiles/*/sessions"),
		"unexpanded glob must not be emitted, got %v", dirs[parser.AgentHermes])
}

// TestResolveScriptSkipsMissingCodexIndex verifies that a missing index
// produces no extra-file entry, so the transfer's tar command never names a
// nonexistent path (which would be a fatal, non-benign error).
func TestResolveScriptSkipsMissingCodexIndex(t *testing.T) {
	home := physTempDir(t)
	require.NoError(t,
		os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755),
		"mkdir sessions")

	out := runResolveScriptForTest(t, "HOME="+home)

	_, extraFiles, _ := parseResolvedDirs(string(out))
	assert.Empty(t, extraFiles,
		"no extra files when session_index.jsonl is absent")
}

// TestResolveScriptSkipsAiderHomeDefault verifies the resolve script does
// NOT infer a bare-$HOME Aider root. The remote resolver tars every emitted
// target, so Aider must stay opt-in even when a history file exists at home
// root. Runs the real script through sh against a temp HOME.
func TestResolveScriptSkipsAiderHomeDefault(t *testing.T) {
	home := physTempDir(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".aider.chat.history.md"),
		[]byte("# aider chat started at 2024-01-01 00:00:00\n"),
		0o644,
	), "write history")

	out := runResolveScriptForTest(t, "HOME="+home)

	dirs, _, _ := parseResolvedDirs(string(out))
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
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
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

	dirs, _, _ := parseResolvedDirs(string(out))
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
	home := physTempDir(t)
	codeRoot := filepath.Join(home, "code")
	injected := "/home/victim/" + parser.AiderHistoryFileName()
	maliciousDir := filepath.Join(codeRoot, "repo\naider:", "home", "victim")
	require.NoError(t, os.MkdirAll(maliciousDir, 0o755), "mkdir malicious dir")
	maliciousHistory := filepath.Join(maliciousDir, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(maliciousHistory, []byte("# aider\n"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home, "AIDER_DIR="+codeRoot)

	dirs, _, _ := parseResolvedDirs(string(out))
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
	home := physTempDir(t)

	for _, override := range []string{home, home + "/"} {
		out := runResolveScriptForTest(t, "HOME="+home, "AIDER_DIR="+override)

		dirs, _, _ := parseResolvedDirs(string(out))
		assert.Empty(t, dirs[parser.AgentAider],
			"AIDER_DIR=%q (== $HOME) must not resolve to a whole-home tar, got %v",
			override, dirs[parser.AgentAider])
	}
}

func TestResolveScriptWindsurfTargetsOnlySessionFiles(t *testing.T) {
	home := physTempDir(t)
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

// skipScriptPathEqualityOnWindows skips script-execution tests that
// assert exact emitted path spellings or build POSIX symlink fixtures.
// The resolve script targets POSIX remote hosts; on Windows CI it runs
// under MSYS sh, whose pwd -P prints /c/... POSIX spellings for Windows
// paths, so physical-path emission (av_phys_dir) rewrites fixtures into
// a dialect no real deployment produces.
func skipScriptPathEqualityOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("resolve script targets POSIX remote hosts; MSYS pwd rewrites Windows paths into /c/... spellings")
	}
}

func runResolveScriptForTest(t *testing.T, env ...string) []byte {
	t.Helper()
	var out []byte
	var err error
	for range 3 {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(buildResolveScript())
		cmd.Env = env
		out, err = cmd.CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte("*** fatal error")) {
			break
		}
		// MSYS bash on Windows runners occasionally dies during process
		// initialization, before the script runs, with errors such as
		// "fatal error - add_item (...) failed, errno 1". Retry the
		// transient launch failure.
		time.Sleep(time.Second)
	}
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

	dirs, extraFiles, _ := parseResolvedDirs(input)

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

	dirs, extraFiles, _ := parseResolvedDirs(input)

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

	dirs, files, extraFiles, _, _ := parseResolvedTargets(input)

	assert.Equal(t, []string{"/home/wes/Windsurf/User"}, dirs[parser.AgentWindsurf])
	assert.Equal(t, []string{
		"/home/wes/Windsurf/User/workspaceStorage/a/state.vscdb",
		"/home/wes/Windsurf/User/workspaceStorage/a/workspace.json",
	}, files[parser.AgentWindsurf])
	assert.Equal(t,
		[]string{"/home/wes/.codex/session_index.jsonl"}, extraFiles)
}

func TestResolveScriptRooCodeTargetsOnlySessionFiles(t *testing.T) {
	home := physTempDir(t)
	rooRoot := filepath.Join(home, ".config", "Code", "User",
		"globalStorage", "rooveterinaryinc.roo-cline")
	task1 := filepath.Join(rooRoot, "tasks", "task-1")
	task2 := filepath.Join(rooRoot, "tasks", "task-2")
	metaDir := filepath.Join(rooRoot, "tasks", "_meta")
	settingsDir := filepath.Join(rooRoot, "settings")
	checkpoints := filepath.Join(task1, "checkpoints")
	require.NoError(t, os.MkdirAll(task1, 0o755))
	require.NoError(t, os.MkdirAll(task2, 0o755))
	require.NoError(t, os.MkdirAll(metaDir, 0o755))
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.MkdirAll(checkpoints, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(task1, "history_item.json"), []byte(`{"id":"task-1"}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(task1, "ui_messages.json"), []byte(`[]`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(task2, "history_item.json"), []byte(`{"id":"task-2"}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(settingsDir, "mcp_settings.json"),
		[]byte(`{"mcpServers":{"s":{"env":{"API_KEY":"sk-secret"}}}}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(checkpoints, "checkpoint.bin"), []byte("checkpoint"), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home)

	records := resolveOutputRecords(string(out))
	rootSuffix := filepath.ToSlash(filepath.Join(".config", "Code", "User",
		"globalStorage", "rooveterinaryinc.roo-cline"))
	agentFilePrefix := resolveAgentFilePrefix + ":" + string(parser.AgentRooCode)
	assert.True(t, hasRecordWithPathSuffix(records,
		string(parser.AgentRooCode), rootSuffix),
		"root must be emitted once as the agent target")
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/task-1/history_item.json"))
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/task-1/ui_messages.json"))
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/task-2/history_item.json"))
	// task-2 has no ui_messages.json; av_emit_agent_file skips it.
	assert.False(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/task-2/ui_messages.json"))
	for _, record := range records {
		assert.NotContains(t, record, "mcp_settings.json",
			"settings must never be emitted")
		assert.NotContains(t, record, "checkpoint",
			"checkpoint data must never be emitted")
		assert.NotContains(t, record, "_meta",
			"underscore-prefixed task dirs must be skipped")
	}
}

func TestResolveScriptRooCodeSkipsRootWithoutSessions(t *testing.T) {
	home := physTempDir(t)
	rooRoot := filepath.Join(home, ".config", "Code", "User",
		"globalStorage", "rooveterinaryinc.roo-cline")
	settingsDir := filepath.Join(rooRoot, "settings")
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(settingsDir, "mcp_settings.json"),
		[]byte(`{"mcpServers":{}}`), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home)

	for _, record := range resolveOutputRecords(string(out)) {
		assert.NotContains(t, record, "roo-cline",
			"a session-less RooCode root must emit nothing")
	}
}

func TestResolveScriptKiloLegacyRejectsSymlinkedTaskDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	home := physTempDir(t)
	klRoot := filepath.Join(home, ".config", "Code", "User",
		"globalStorage", "kilocode.kilo-code")
	tasksDir := filepath.Join(klRoot, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))

	// Real task directory with metadata.
	realTask := filepath.Join(tasksDir, "real-task")
	require.NoError(t, os.MkdirAll(realTask, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(realTask, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(realTask, "ui_messages.json"),
		[]byte(`[]`), 0o644,
	))

	// Symlinked task directory pointing outside root.
	outsideDir := filepath.Join(physTempDir(t), "escaped-task")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outsideDir, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	symlinkTask := filepath.Join(tasksDir, "symlink-task")
	require.NoError(t, os.Symlink(outsideDir, symlinkTask))

	out := runResolveScriptForTest(t, "HOME="+home)

	records := resolveOutputRecords(string(out))
	agentFilePrefix := resolveAgentFilePrefix + ":" + string(parser.AgentKiloLegacy)
	// Real task files should be emitted.
	assert.True(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/real-task/task_metadata.json"),
		"real task metadata should be emitted")
	// Symlinked task files must not be emitted.
	assert.False(t, hasRecordWithPathSuffix(records, agentFilePrefix,
		"tasks/symlink-task/task_metadata.json"),
		"symlinked task files must be rejected")
}

// TestResolveScriptPoolsideTargetsOnlyTrajectories ensures the SSH
// remote-sync resolver narrows Poolside's application-data root to
// only the trajectories/ subdirectory, mirroring
// remotesync.resolvePoolsideTarget.
func TestResolveScriptPoolsideTargetsOnlyTrajectories(t *testing.T) {
	home := physTempDir(t)
	poolsideRoot := filepath.Join(home, ".local", "state", "poolside")
	trajectoriesDir := filepath.Join(poolsideRoot, "trajectories")
	settingsDir := filepath.Join(poolsideRoot, "settings")
	require.NoError(t, os.MkdirAll(trajectoriesDir, 0o755))
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(trajectoriesDir, "trajectory-standalone_test.ndjson"),
		[]byte(`{"type":"session.start"}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(settingsDir, "config.json"),
		[]byte(`{"api_key":"sk-secret"}`), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home)

	records := resolveOutputRecords(string(out))
	trajectoriesSuffix := filepath.ToSlash(filepath.Join(
		".local", "state", "poolside", "trajectories"))
	assert.True(t, hasRecordWithPathSuffix(records,
		string(parser.AgentPoolside), trajectoriesSuffix),
		"only the trajectories/ subdirectory must be emitted as the target")
	for _, record := range records {
		assert.NotContains(t, record, "settings",
			"unrelated settings directory must not be emitted")
		assert.NotContains(t, record, "config.json",
			"unrelated config files must not be emitted")
	}
}

// TestResolveScriptPoolsideSkipsRootWithoutTrajectories ensures the
// SSH resolver emits nothing when the trajectories/ subdirectory does
// not exist.
func TestResolveScriptPoolsideSkipsRootWithoutTrajectories(t *testing.T) {
	home := physTempDir(t)
	poolsideRoot := filepath.Join(home, ".local", "state", "poolside")
	require.NoError(t, os.MkdirAll(poolsideRoot, 0o755))

	out := runResolveScriptForTest(t, "HOME="+home)

	for _, record := range resolveOutputRecords(string(out)) {
		assert.NotContains(t, record, "poolside",
			"a Poolside root without trajectories/ must emit nothing")
	}
}

// TestResolveScriptPoolsideTrajectoriesRoot verifies the SSH resolver
// handles a configured root that IS already the trajectories/ directory,
// using it as-is without producing trajectories/trajectories/.
func TestResolveScriptPoolsideTrajectoriesRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolve script runs on POSIX remote hosts; local Windows filepaths and MSYS shell parsing are not representative")
	}
	home := physTempDir(t)
	// Set POOLSIDE_DIR directly to a trajectories/ directory.
	trajectoriesDir := filepath.Join(home, "poolside", "trajectories")
	require.NoError(t, os.MkdirAll(trajectoriesDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(trajectoriesDir, "trajectory-standalone_test.ndjson"),
		[]byte(`{"type":"session.start"}`), 0o644))

	out := runResolveScriptForTest(t, "HOME="+home, "POOLSIDE_DIR="+trajectoriesDir)

	dirs, _, _ := parseResolvedDirs(string(out))
	assert.Equal(t, []string{trajectoriesDir}, dirs[parser.AgentPoolside],
		"the environment override must produce one transfer target")

	records := resolveOutputRecords(string(out))
	agentSuffix := filepath.ToSlash(filepath.Join("poolside", "trajectories"))
	assert.True(t, hasRecordWithPathSuffix(records,
		string(parser.AgentPoolside), agentSuffix),
		"a trajectories/ root must be used as-is")
	// Must NOT produce poolside/poolside/trajectories.
	for _, record := range records {
		assert.NotContains(t, record, "poolside/poolside",
			"must not double-nest trajectories")
	}
}

// physTempDir returns t.TempDir() with symlinks resolved. The resolve
// script emits physical paths (see av_phys_dir), so fixtures must compare
// against physical spellings — on macOS t.TempDir() lives under /var,
// which is a symlink to /private/var.
func physTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func TestParseResolvedTargetsFailsClosedOnUnrepresentableForbiddenRoot(
	t *testing.T,
) {
	tests := []struct {
		name  string
		input string
	}{
		{
			"newline_inside_forbidden_value",
			"claude:/home/u/.claude\x00@forbidden:/home/u/evil\ndir\x00",
		},
		{
			"empty_forbidden_value",
			"claude:/home/u/.claude\x00@forbidden:\x00",
		},
		{
			"bare_forbidden_record",
			"claude:/home/u/.claude\x00@forbidden\x00",
		},
		{
			"non_utf8_forbidden_value",
			"claude:/home/u/.claude\x00@forbidden:/home/u/\xff\xfe\x00",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := parseResolvedTargets(tc.input)
			require.Error(t, err,
				"a forbidden record that cannot be represented must fail the sync, not drop the boundary")
			assert.Contains(t, err.Error(), "forbidden")
		})
	}
}

func TestParseResolvedTargetsPreservesNULDelimitedSpellingsExactly(
	t *testing.T,
) {
	input := "@forbidden:/home/u/trae dir \x00" +
		"claude:/home/u/.claude \x00"

	dirs, _, _, forbiddenRoots, err := parseResolvedTargets(input)
	require.NoError(t, err)
	assert.Equal(t, []string{"/home/u/trae dir "}, forbiddenRoots,
		"NUL-delimited forbidden roots must keep trailing whitespace — trimming changes the excluded path")
	assert.Equal(t, []string{"/home/u/.claude "}, dirs[parser.AgentClaude],
		"NUL-delimited target spellings must be preserved exactly")
}

func TestResolveScriptEmitsPhysicalPathsForSymlinkedRoots(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	physicalTrae := filepath.Join(home, "real-trae")
	physicalClaude := filepath.Join(home, "real-claude")
	require.NoError(t, os.MkdirAll(physicalTrae, 0o755))
	require.NoError(t, os.MkdirAll(
		filepath.Join(physicalClaude, "projects"), 0o755))
	traeAlias := filepath.Join(home, "trae-alias")
	require.NoError(t, os.Symlink(physicalTrae, traeAlias))
	// The default Claude root $HOME/.claude/projects is reached through a
	// symlinked ancestor.
	require.NoError(t, os.Symlink(
		physicalClaude, filepath.Join(home, ".claude")))

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+traeAlias)
	dirs, _, _, forbiddenRoots, err := parseResolvedTargets(string(out))
	require.NoError(t, err)

	assert.Contains(t, forbiddenRoots, physicalTrae,
		"forbidden roots must be emitted by physical spelling so alias overlap cannot bypass them")
	assert.NotContains(t, forbiddenRoots, traeAlias)
	assert.Contains(t, dirs[parser.AgentClaude],
		filepath.Join(physicalClaude, "projects"),
		"targets must be emitted by physical spelling to share a namespace with forbidden roots")
}

// TestAvPhysFileEdgeCases exercises the script's file-canonicalization
// helper directly (it is a pure path transform; existence checks live
// with the emitters): a root-level "/file" keeps parent "/", a bare
// filename resolves against the physical working directory, and a file
// spelled through a symlinked parent resolves to its physical location.
func TestAvPhysFileEdgeCases(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	base := physTempDir(t)
	physicalDir := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(physicalDir, 0o755))
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(physicalDir, alias))

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"root_level_file_keeps_root_parent", "/no-such-file", "/no-such-file"},
		{"bare_filename_resolves_against_cwd", "bare.md",
			filepath.Join(base, "bare.md")},
		{"symlinked_parent_resolves_physically",
			filepath.Join(alias, "history.md"),
			filepath.Join(physicalDir, "history.md")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := resolveScriptPhysHelpers +
				"av_phys_file \"$AV_TEST_INPUT\"\n"
			cmd := exec.Command("sh")
			cmd.Stdin = strings.NewReader(script)
			cmd.Dir = base
			cmd.Env = []string{"AV_TEST_INPUT=" + tc.input}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "helper failed: %s", out)
			assert.Equal(t, tc.want, string(out))
		})
	}
}

// TestResolveScriptAiderSymlinkOverlapStaysForbidden is the regression
// test for Aider results reached through a symlink into an excluded
// provider's tree: the emitted history-file path must carry the physical
// forbidden-root prefix so the transfer-side filter drops it.
func TestResolveScriptAiderSymlinkOverlapStaysForbidden(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	physicalTrae := filepath.Join(home, "real-trae")
	repoDir := filepath.Join(physicalTrae, "code", "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	history := filepath.Join(repoDir, parser.AiderHistoryFileName())
	require.NoError(t, os.WriteFile(history, []byte("# aider"), 0o644))
	aiderAlias := filepath.Join(home, "aider-alias")
	require.NoError(t, os.Symlink(
		filepath.Join(physicalTrae, "code"), aiderAlias))

	out := runResolveScriptForTest(t,
		"HOME="+home, "TRAE_DIR="+physicalTrae, "AIDER_DIR="+aiderAlias)
	dirs, _, _, forbiddenRoots, err := parseResolvedTargets(string(out))
	require.NoError(t, err)

	require.Contains(t, forbiddenRoots, physicalTrae)
	require.Len(t, dirs[parser.AgentAider], 1,
		"the walk should still find the history file through the alias")
	got := dirs[parser.AgentAider][0]
	assert.Equal(t, history, got,
		"aider results must be emitted by physical spelling")
	assert.True(t, pathWithinForbiddenRoots(forbiddenRoots, got),
		"physical spelling must fall inside the forbidden root so the transfer filter excludes it")
}

// TestAvPhysHelpersRefuseNewlinePaths pins the fail-closed handling of
// physical paths containing CR/LF: command substitution strips trailing
// newlines from pwd output, so emitting such a path would produce a
// clean-looking but wrong spelling that downstream exclusion would guard
// instead of the real subtree. The helpers must refuse instead.
func TestAvPhysHelpersRefuseNewlinePaths(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	base := physTempDir(t)
	newlineDir := filepath.Join(base, "bad\ndir")
	require.NoError(t, os.MkdirAll(newlineDir, 0o755))

	script := resolveScriptPhysHelpers +
		"if av_phys_dir \"$AV_TEST_INPUT\"; then echo ACCEPTED; else echo REFUSED; fi\n" +
		"if av_phys_file \"$AV_TEST_INPUT/file\"; then echo ACCEPTED; else echo REFUSED; fi\n" +
		"if av_phys_missing \"$AV_TEST_INPUT/missing\"; then echo ACCEPTED; else echo REFUSED; fi\n"
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"AV_TEST_INPUT=" + newlineDir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "helper harness failed: %s", out)
	assert.Equal(t, "REFUSED\nREFUSED\nREFUSED\n", string(out),
		"newline-carrying physical paths must be refused, not emitted misspelled")
}

// TestResolveScriptAbortsOnUnrepresentableForbiddenRoot pins the
// whole-script abort: when an excluded agent's root physically resolves
// to a newline-carrying path, the resolve must fail (non-zero exit) so
// the sync aborts, rather than emitting a mangled spelling that would
// guard the wrong subtree.
func TestResolveScriptAbortsOnUnrepresentableForbiddenRoot(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	traeRoot := filepath.Join(home, "trae\nroot")
	require.NoError(t, os.MkdirAll(traeRoot, 0o755))

	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(buildResolveScript())
	cmd.Env = []string{"HOME=" + home, "TRAE_DIR=" + traeRoot}
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"an unrepresentable forbidden root must abort the resolve, got output: %s", out)
}

// TestResolveScriptPoolsideSymlinkedTrajectoriesOverride pins the
// canonicalize-after-narrowing ordering: a POOLSIDE_DIR override that is
// a symlink NAMED trajectories must be recognized by its literal
// basename (used as-is), then emitted by physical spelling.
// Canonicalizing before dispatch would rename the override to its
// physical basename and make the resolver look for a nested
// trajectories/ that does not exist.
func TestResolveScriptPoolsideSymlinkedTrajectoriesOverride(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	physicalDir := filepath.Join(home, "poolside-data")
	require.NoError(t, os.MkdirAll(physicalDir, 0o755))
	alias := filepath.Join(home, "trajectories")
	require.NoError(t, os.Symlink(physicalDir, alias))

	out := runResolveScriptForTest(t, "HOME="+home, "POOLSIDE_DIR="+alias)
	dirs, _, _, _, err := parseResolvedTargets(string(out))
	require.NoError(t, err)

	assert.Equal(t, []string{physicalDir}, dirs[parser.AgentPoolside],
		"a symlink named trajectories must narrow by its literal basename and emit its physical path")
}

// TestResolveScriptWindsurfSymlinkedWorkspaceStorageOverride is the
// workspaceStorage analog: the override's literal basename decides
// whether workspaceStorage is appended, and only the emitted root is
// canonicalized.
func TestResolveScriptWindsurfSymlinkedWorkspaceStorageOverride(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	physicalDir := filepath.Join(home, "windsurf-data")
	wsDir := filepath.Join(physicalDir, "ws1")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(wsDir, parser.WindsurfStateDBName), []byte("db"), 0o644))
	alias := filepath.Join(home, "workspaceStorage")
	require.NoError(t, os.Symlink(physicalDir, alias))

	out := runResolveScriptForTest(t, "HOME="+home, "WINDSURF_DIR="+alias)
	dirs, files, _, _, err := parseResolvedTargets(string(out))
	require.NoError(t, err)

	assert.Equal(t, []string{physicalDir}, dirs[parser.AgentWindsurf],
		"a symlink named workspaceStorage must narrow by its literal basename and emit its physical path")
	assert.Contains(t, files[parser.AgentWindsurf],
		filepath.Join(wsDir, parser.WindsurfStateDBName),
		"session files must carry the physical spelling")
}

// TestAvPhysMissingResolvesLongestExistingAncestor exercises the
// missing-path canonicalization helper directly: the longest existing
// ancestor resolves physically and the missing tail is rejoined
// literally, so a forbidden root that does not exist yet still guards
// the physical subtree where its contents would appear.
func TestAvPhysMissingResolvesLongestExistingAncestor(t *testing.T) {
	skipScriptPathEqualityOnWindows(t)
	base := physTempDir(t)
	physicalDir := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(physicalDir, 0o755))
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(physicalDir, alias))

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"existing_dir_resolves_physically", alias, physicalDir},
		{"missing_leaf_under_existing_parent",
			filepath.Join(base, "missing"), filepath.Join(base, "missing")},
		{"missing_tail_under_symlinked_ancestor",
			filepath.Join(alias, "missing", "leaf"),
			filepath.Join(physicalDir, "missing", "leaf")},
		{"relative_spelling_anchors_to_cwd", "missing-rel/leaf",
			filepath.Join(base, "missing-rel", "leaf")},
		{"fully_missing_absolute_path_keeps_spelling",
			"/nonexistent-av-test/a/b", "/nonexistent-av-test/a/b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := resolveScriptPhysHelpers +
				"av_phys_missing \"$AV_TEST_INPUT\"\n"
			cmd := exec.Command("sh")
			cmd.Stdin = strings.NewReader(script)
			cmd.Dir = base
			cmd.Env = []string{"AV_TEST_INPUT=" + tc.input}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "helper failed: %s", out)
			assert.Equal(t, tc.want, string(out))
		})
	}
}

// TestResolveScriptMissingForbiddenRootResolvesSymlinkedAncestor pins
// av_emit_forbidden_root's missing-root handling: a TRAE_DIR that does
// not exist but is spelled through a symlinked ancestor must be emitted
// under the physical subtree where its contents would appear, so the
// boundary matches canonicalized targets if the root is created later.
func TestResolveScriptMissingForbiddenRootResolvesSymlinkedAncestor(
	t *testing.T,
) {
	skipScriptPathEqualityOnWindows(t)
	home := physTempDir(t)
	physicalDir := filepath.Join(home, "real-apps")
	require.NoError(t, os.MkdirAll(physicalDir, 0o755))
	alias := filepath.Join(home, "apps-alias")
	require.NoError(t, os.Symlink(physicalDir, alias))
	missingRoot := filepath.Join(alias, "Trae", "User")

	out := runResolveScriptForTest(t, "HOME="+home, "TRAE_DIR="+missingRoot)
	_, _, _, forbiddenRoots, err := parseResolvedTargets(string(out))
	require.NoError(t, err)

	assert.Contains(t, forbiddenRoots,
		filepath.Join(physicalDir, "Trae", "User"),
		"a missing forbidden root must resolve through its longest existing ancestor")
	assert.NotContains(t, forbiddenRoots, missingRoot)
}

// TestParseResolvedTargetsDropsNonUTF8TargetRecords: non-UTF-8 spellings
// are unrepresentable downstream (glob escaping iterates runes, the
// Python snapshot filter round-trips JSON), so target records carrying
// them are dropped rather than mangled. Forbidden records with non-UTF-8
// bytes fail the sync instead, covered by the fail-closed test above.
func TestParseResolvedTargetsDropsNonUTF8TargetRecords(t *testing.T) {
	input := "claude:/home/u/\xff\x00codex:/home/u/.codex/sessions\x00"

	dirs, _, _, _, err := parseResolvedTargets(input)
	require.NoError(t, err)

	assert.Empty(t, dirs[parser.AgentClaude],
		"a non-UTF-8 target spelling must be dropped, not passed downstream")
	assert.Equal(t, []string{"/home/u/.codex/sessions"},
		dirs[parser.AgentCodex],
		"valid records in the same output must survive")
}
