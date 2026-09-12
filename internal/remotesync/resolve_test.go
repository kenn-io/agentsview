package remotesync_test

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/ssh"
)

func resolveTargetsForTest(t *testing.T, cfg config.Config) remotesync.TargetSet {
	t.Helper()
	targets, err := remotesync.ResolveTargets(cfg)
	require.NoError(t, err)
	return targets
}

func TestResolveTargetsExcludesNonLocalStructuredSessionSources(t *testing.T) {
	const localMachine = "0123456789abcdef0123456789abcdef"
	const foreignMachine = "remote-host"
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dataDir := filepath.Join(home, "data")
	localRoot := filepath.Join(home, "local-copilot")
	localStructuredRoot := filepath.Join(home, "local-structured-copilot")
	foreignRoot := filepath.Join(localRoot, "foreign-copilot")
	for _, dir := range []string{dataDir, localRoot, localStructuredRoot, foreignRoot} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	localSession := filepath.Join(localRoot, "local.jsonl")
	foreignSession := filepath.Join(foreignRoot, "foreign.jsonl")
	require.NoError(t, os.WriteFile(localSession, []byte("local\n"), 0o600))
	require.NoError(t, os.WriteFile(foreignSession, []byte("foreign\n"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	for _, def := range parser.Registry {
		if def.DefaultRootEnvVar != "" {
			t.Setenv(def.DefaultRootEnvVar, "")
		}
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar, "")
		}
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		fmt.Appendf(nil, `
copilot_dirs = [%q]

[[session_sources]]
agent = "copilot"
dir = %q

[[session_sources]]
agent = "copilot"
dir = %q
machine = %q
`, localRoot, localStructuredRoot, foreignRoot, foreignMachine),
		0o600,
	))

	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), []byte(localMachine), 0o600))
	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	require.Equal(t, localMachine, cfg.InstallationID)
	targets := resolveTargetsForTest(t, cfg)

	assert.ElementsMatch(t, []string{localRoot, localStructuredRoot},
		targets.Dirs[parser.AgentCopilot])
	assert.NotContains(t, targets.Dirs[parser.AgentCopilot], foreignRoot,
		"a source attributed to another machine must not be re-exported as local")
	assert.Contains(t, targets.ForbiddenRoots, foreignRoot)
	manifest, err := remotesync.BuildManifest(targets)
	require.NoError(t, err)
	var manifestPaths []string
	for _, file := range manifest.Files {
		manifestPaths = append(manifestPaths, file.Path)
	}
	assert.Contains(t, manifestPaths, localSession)
	assert.NotContains(t, manifestPaths, foreignSession,
		"an allowed ancestor must not re-export its nested foreign source")
}

func TestResolveTargetsFiltersAndIncludesSpecialFiles(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude")
	missingDir := filepath.Join(root, "missing")
	codexDir := filepath.Join(root, ".codex", "sessions")
	devinDir := filepath.Join(root, "devin")
	warpDir := filepath.Join(root, "warp")
	aiderRoot := filepath.Join(root, "code")
	aiderHistory := filepath.Join(aiderRoot, "repo", parser.AiderHistoryFileName())
	windsurfUserRoot := filepath.Join(root, "Windsurf", "User")
	windsurfWorkspaceRoot := filepath.Join(windsurfUserRoot, "workspaceStorage")
	windsurfWorkspaceDir := filepath.Join(windsurfWorkspaceRoot, "workspace-a")
	windsurfStateDB := filepath.Join(windsurfWorkspaceDir, parser.WindsurfStateDBName)
	windsurfStateWAL := windsurfStateDB + "-wal"
	windsurfStateSHM := windsurfStateDB + "-shm"
	windsurfWorkspaceJSON := filepath.Join(windsurfWorkspaceDir, "workspace.json")
	windsurfSecret := filepath.Join(windsurfWorkspaceDir, "extension-secret.json")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(devinDir, 0o755))
	require.NoError(t, os.MkdirAll(warpDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(aiderHistory), 0o755))
	require.NoError(t, os.MkdirAll(windsurfWorkspaceDir, 0o755))
	require.NoError(t, os.WriteFile(aiderHistory, []byte("# aider\n"), 0o644))
	require.NoError(t, os.WriteFile(windsurfStateDB, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(windsurfStateWAL, []byte("wal"), 0o644))
	require.NoError(t, os.WriteFile(windsurfStateSHM, []byte("shm"), 0o644))
	require.NoError(t, os.WriteFile(windsurfWorkspaceJSON, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(windsurfSecret, []byte("secret"), 0o644))
	codexIndex := filepath.Join(root, ".codex", parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(codexIndex, []byte("{}\n"), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir, missingDir},
			parser.AgentCodex:  {codexDir},
			parser.AgentDevin:  {devinDir},
			parser.AgentWarp:   {warpDir},
			parser.AgentAider:  {aiderRoot},
			parser.AgentZed:    {filepath.Join(root, "zed")},
			parser.AgentWindsurf: {
				windsurfUserRoot,
			},
		},
	})

	assert.Equal(t, []string{claudeDir}, targets.Dirs[parser.AgentClaude])
	assert.Equal(t, []string{codexDir}, targets.Dirs[parser.AgentCodex])
	assert.NotContains(t, targets.Dirs, parser.AgentDevin)
	assert.NotContains(t, targets.Dirs, parser.AgentWarp)
	assert.Equal(t, []string{aiderHistory}, targets.Dirs[parser.AgentAider])
	assert.NotContains(t, targets.Dirs, parser.AgentZed)
	assert.Equal(t, []string{windsurfUserRoot}, targets.Dirs[parser.AgentWindsurf])
	assert.NotContains(t, targets.Dirs[parser.AgentWindsurf], windsurfWorkspaceRoot)
	assert.ElementsMatch(t, []string{
		windsurfStateDB,
		windsurfStateWAL,
		windsurfWorkspaceJSON,
	}, targets.Files[parser.AgentWindsurf])
	assert.NotContains(t, targets.Files[parser.AgentWindsurf], windsurfStateSHM)
	assert.NotContains(t, targets.Files[parser.AgentWindsurf], windsurfSecret)
	assert.Contains(t, targets.ProviderExtraFiles[parser.AgentCodex], codexIndex)
}

func TestResolveTargetsExcludesRemoteSyncExcludedAgentState(t *testing.T) {
	root := t.TempDir()
	chatDB := filepath.Join(root, "chat.db")
	for _, path := range []string{
		chatDB,
		chatDB + "-wal",
		chatDB + "-shm",
		chatDB + "-journal",
	} {
		require.NoError(t, os.WriteFile(path, []byte("sqlite"), 0o644))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "credentials.json"), []byte("secret"), 0o600,
	))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTrae: {root},
		},
	})

	assert.NotContains(t, targets.Dirs, parser.AgentTrae)
	assert.NotContains(t, targets.Files, parser.AgentTrae)
	assert.Equal(t, []string{filepath.Clean(root)}, targets.ForbiddenRoots,
		"excluded-provider roots must remain as transfer boundaries")
}

func TestResolveTargetsExcludesTraeProfile(t *testing.T) {
	root := t.TempDir()
	traeRoot := filepath.Join(root, "TRAE", "User")
	claudeRoot := filepath.Join(root, "claude")
	require.NoError(t, os.MkdirAll(traeRoot, 0o755))
	require.NoError(t, os.MkdirAll(claudeRoot, 0o755))

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentTrae:   {traeRoot},
		parser.AgentClaude: {claudeRoot},
	}})
	assert.NotContains(t, targets.Dirs, parser.AgentTrae)
	assert.Equal(t, []string{claudeRoot}, targets.Dirs[parser.AgentClaude])
}

// TestResolveTargetsOmitsAllowedTargetsInsideForbiddenRoots pins the fix
// for overlapping directory overrides: an allowed agent's root nested
// inside an excluded agent's root must be omitted from the advertised
// TargetSet — not advertised and then rejected — so an honest client
// echoing the advertised set syncs the remaining targets instead of
// failing the whole request with 403.
func TestResolveTargetsOmitsAllowedTargetsInsideForbiddenRoots(t *testing.T) {
	base := t.TempDir()
	traeRoot := filepath.Join(base, "trae")
	nestedClaude := filepath.Join(traeRoot, "claude")
	outsideClaude := filepath.Join(base, "claude")
	require.NoError(t, os.MkdirAll(nestedClaude, 0o755))
	require.NoError(t, os.MkdirAll(outsideClaude, 0o755))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTrae:   {traeRoot},
			parser.AgentClaude: {nestedClaude, outsideClaude},
		},
	})

	assert.Equal(t, []string{outsideClaude}, targets.Dirs[parser.AgentClaude],
		"nested root must be dropped from the advertised set, siblings kept")
	assert.Equal(t, []string{traeRoot}, targets.ForbiddenRoots)

	selected, ok := remotesync.SelectAllowedTargets(targets, targets)
	require.True(t, ok,
		"a client echoing the advertised set must not be rejected")
	assert.Equal(t, []string{outsideClaude}, selected.Dirs[parser.AgentClaude])
}

// TestResolveTargetsDropsFileScopedAgentWhenSessionFilesForbidden guards
// the file-scoped pairing invariant: when a forbidden root swallows a
// file-scoped agent's curated session files but not its advertised root,
// both halves must be dropped — otherwise the agent would degrade to a
// raw directory target and expose settings and caches its file scoping
// exists to keep unreachable.
func TestResolveTargetsDropsFileScopedAgentWhenSessionFilesForbidden(
	t *testing.T,
) {
	base := t.TempDir()
	rooRoot := filepath.Join(base, "globalStorage", "rooveterinaryinc.roo-cline")
	tasksDir := filepath.Join(rooRoot, "tasks")
	taskDir := filepath.Join(tasksDir, "task-1")
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "history_item.json"),
		[]byte(`{"id":"task-1","ts":1,"task":"t"}`), 0o644,
	))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTrae:    {tasksDir},
			parser.AgentRooCode: {rooRoot},
		},
	})

	assert.Equal(t, []string{tasksDir}, targets.ForbiddenRoots)
	assert.NotContains(t, targets.Dirs, parser.AgentRooCode,
		"file-scoped root must not survive as a raw directory target")
	assert.NotContains(t, targets.Files, parser.AgentRooCode)
}

// TestResolveTargetsPoolsideNarrowsToTrajectories ensures the HTTP
// remote-sync resolver narrows Poolside's application-data root to
// only the trajectories/ subdirectory, preventing unrelated config,
// caches, or credentials from being archived.
func TestResolveTargetsPoolsideNarrowsToTrajectories(t *testing.T) {
	root := t.TempDir()
	trajectoriesDir := filepath.Join(root, "trajectories")
	settingsFile := filepath.Join(root, "config.json")
	require.NoError(t, os.MkdirAll(trajectoriesDir, 0o755))
	require.NoError(t, os.WriteFile(settingsFile, []byte(`{"api_key":"sk-secret"}`), 0o644))

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentPoolside: {root},
	}})

	require.Len(t, targets.Dirs[parser.AgentPoolside], 1,
		"Poolside must resolve to exactly one directory (trajectories/)")
	assert.Equal(t, trajectoriesDir, targets.Dirs[parser.AgentPoolside][0],
		"resolved target must be the trajectories/ subdirectory, not the parent root")
	assert.NotContains(t, targets.Dirs[parser.AgentPoolside], root,
		"the application-data root itself must not be an archived target")
}

// TestResolveTargetsPoolsideSkipsMissingTrajectories ensures the HTTP
// resolver emits nothing when the trajectories/ subdirectory does not
// exist.
func TestResolveTargetsPoolsideSkipsMissingTrajectories(t *testing.T) {
	root := t.TempDir()

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentPoolside: {root},
	}})

	assert.NotContains(t, targets.Dirs, parser.AgentPoolside,
		"a Poolside root without trajectories/ must not produce a target")
}

// TestResolveTargetsPoolsideTrajectoriesRoot verifies the HTTP resolver
// handles a configured root that IS already the trajectories/ directory,
// using it as-is without producing trajectories/trajectories/.
func TestResolveTargetsPoolsideTrajectoriesRoot(t *testing.T) {
	trajectoriesDir := filepath.Join(t.TempDir(), "trajectories")
	require.NoError(t, os.MkdirAll(trajectoriesDir, 0o755))

	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentPoolside: {trajectoriesDir},
	}})

	require.Len(t, targets.Dirs[parser.AgentPoolside], 1)
	assert.Equal(t, trajectoriesDir, targets.Dirs[parser.AgentPoolside][0],
		"a trajectories/ root must be used as-is, not appended to")
}

func TestResolveTargetsExpandsHermesProfilesWithDatabaseFiles(t *testing.T) {
	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	withSessions := filepath.Join(profilesRoot, "research")
	databaseOnly := filepath.Join(profilesRoot, "database-only")
	require.NoError(t, os.MkdirAll(filepath.Join(withSessions, "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(databaseOnly, 0o755))
	for _, path := range []string{
		filepath.Join(withSessions, "state.db"),
		filepath.Join(withSessions, "state.db-wal"),
		filepath.Join(withSessions, "state.db-shm"),
		filepath.Join(withSessions, "state.db-journal"),
		filepath.Join(databaseOnly, "state.db"),
	} {
		require.NoError(t, os.WriteFile(path, []byte("sqlite"), 0o644))
	}

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profilesRoot},
		},
	})

	assert.ElementsMatch(t, []string{
		filepath.Join(withSessions, "sessions"),
		filepath.Join(databaseOnly, "state.db"),
	}, targets.Dirs[parser.AgentHermes])
	assert.ElementsMatch(t, []string{
		filepath.Join(withSessions, "state.db"),
		filepath.Join(withSessions, "state.db-wal"),
		filepath.Join(withSessions, "state.db-shm"),
		filepath.Join(withSessions, "state.db-journal"),
		filepath.Join(databaseOnly, "state.db-wal"),
		filepath.Join(databaseOnly, "state.db-shm"),
		filepath.Join(databaseOnly, "state.db-journal"),
	}, targets.ProviderExtraFiles[parser.AgentHermes])
}

func TestResolveTargetsIncludesFlatCustomHermesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom", "hermes-archive")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "child.jsonl"), []byte("{}\n"), 0o644,
	))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {root},
		},
	})

	assert.Equal(t, []string{root}, targets.Dirs[parser.AgentHermes])
	assert.Empty(t, targets.ExtraFiles)
}

func TestResolveTargetsSkipsSessionlessHermesProfileCredentials(t *testing.T) {
	profileRoot := filepath.Join(t.TempDir(), ".hermes", "profiles", "sessions")
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

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profileRoot},
		},
	})

	assert.NotContains(t, targets.Dirs, parser.AgentHermes)
	assert.Empty(t, targets.ExtraFiles)
}

func TestResolveTargetsSkipsAiderHomeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir does not use HOME on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	aiderHistory := filepath.Join(home, "repo", parser.AiderHistoryFileName())
	require.NoError(t, os.MkdirAll(filepath.Dir(aiderHistory), 0o755))
	require.NoError(t, os.WriteFile(aiderHistory, []byte("# aider\n"), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider: {home + string(filepath.Separator)},
		},
	})

	assert.NotContains(t, targets.Dirs, parser.AgentAider)
}

func TestSelectAllowedTargetsReturnsResolvedValues(t *testing.T) {
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {"/srv/claude", "/srv/claude-extra"},
			parser.AgentWindsurf: {"/srv/Windsurf/User"},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {
				"/srv/Windsurf/User/workspaceStorage/a/state.vscdb",
				"/srv/Windsurf/User/workspaceStorage/a/workspace.json",
			},
		},
		ExtraFiles: []string{"/srv/.codex/session_index.jsonl"},
	}
	requested := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {"/srv/claude-extra"},
			parser.AgentWindsurf: {"/srv/Windsurf/User"},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {
				"/srv/Windsurf/User/workspaceStorage/a/state.vscdb",
			},
		},
		ExtraFiles: []string{"/srv/.codex/session_index.jsonl"},
	}

	selected, ok := remotesync.SelectAllowedTargets(allowed, requested)

	require.True(t, ok)
	assert.Equal(t, []string{"/srv/claude-extra"}, selected.Dirs[parser.AgentClaude])
	assert.Equal(t, []string{"/srv/Windsurf/User"}, selected.Dirs[parser.AgentWindsurf])
	assert.Equal(t, []string{
		"/srv/Windsurf/User/workspaceStorage/a/state.vscdb",
	}, selected.Files[parser.AgentWindsurf])
	assert.Equal(t, []string{"/srv/.codex/session_index.jsonl"}, selected.ExtraFiles)
}

func TestSelectAllowedTargetsRetainsForbiddenRootsAndRejectsForbiddenDelta(t *testing.T) {
	root := t.TempDir()
	allowedRoot := filepath.Join(root, "sessions")
	forbiddenRoot := filepath.Join(allowedRoot, ".forbidden-provider")
	secret := filepath.Join(forbiddenRoot, "chat.db")
	keep := filepath.Join(allowedRoot, "session.jsonl")
	require.NoError(t, os.MkdirAll(forbiddenRoot, 0o755))
	require.NoError(t, os.WriteFile(keep, []byte("session"), 0o644))
	require.NoError(t, os.WriteFile(secret, []byte("authentication state"), 0o600))

	allowed := remotesync.TargetSet{
		Dirs:           map[parser.AgentType][]string{parser.AgentClaude: {allowedRoot}},
		ForbiddenRoots: []string{forbiddenRoot},
	}
	selected, ok := remotesync.SelectAllowedTargets(allowed, remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {allowedRoot}},
	})

	require.True(t, ok)
	assert.Equal(t, []string{forbiddenRoot}, selected.ForbiddenRoots,
		"archive and manifest writers need the server-resolved boundary")
	_, ok = remotesync.SelectAllowedFiles(allowed, []string{secret})
	assert.False(t, ok,
		"the delta request must reject a forbidden file even under an allowed root")
}

func TestSelectAllowedTargetsRejectsFileScopedDirOnlyRequest(t *testing.T) {
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {"/srv/Windsurf/User"},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {
				"/srv/Windsurf/User/workspaceStorage/a/state.vscdb",
			},
		},
	}
	requested := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {"/srv/Windsurf/User"},
		},
	}

	_, ok := remotesync.SelectAllowedTargets(allowed, requested)

	assert.False(t, ok)
	assert.False(t, remotesync.TargetSetAllowed(allowed, requested))
}

func TestSelectAllowedTargetsRejectsUnresolvedValues(t *testing.T) {
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/claude"},
		},
	}
	requested := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/etc"},
		},
	}

	_, ok := remotesync.SelectAllowedTargets(allowed, requested)

	assert.False(t, ok)
	assert.False(t, remotesync.TargetSetAllowed(allowed, requested))
}

func TestResolveTargetsMatchesSSHResolverForRepresentativeHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSH resolver parity test compares Unix shell path dialects")
	}
	// The resolve script emits physical paths, so the parity fixture must
	// live at a physical spelling (macOS t.TempDir() sits under the /var
	// symlink).
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	claudeDir := filepath.Join(home, ".claude", "projects")
	codexDir := filepath.Join(home, ".codex", "sessions")
	devinDir := filepath.Join(home, ".local", "share", "devin")
	aiderRoot := filepath.Join(home, "code")
	aiderHistory := filepath.Join(aiderRoot, "repo", parser.AiderHistoryFileName())
	windsurfUserRoot := filepath.Join(home, "AppData", "Roaming", "Windsurf", "User")
	windsurfWorkspaceRoot := filepath.Join(windsurfUserRoot, "workspaceStorage")
	windsurfWorkspaceDir := filepath.Join(windsurfWorkspaceRoot, "workspace-a")
	windsurfStateDB := filepath.Join(windsurfWorkspaceDir, parser.WindsurfStateDBName)
	windsurfWorkspaceJSON := filepath.Join(windsurfWorkspaceDir, "workspace.json")
	poolsideRoot := filepath.Join(home, ".local", "state", "poolside")
	poolsideTrajectories := filepath.Join(poolsideRoot, "trajectories")
	clineRoot := filepath.Join(home, ".cline")
	clineSessionDir := filepath.Join(clineRoot, "data", "sessions", "sess-test")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(devinDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(aiderHistory), 0o755))
	require.NoError(t, os.MkdirAll(windsurfWorkspaceDir, 0o755))
	require.NoError(t, os.MkdirAll(poolsideTrajectories, 0o755))
	require.NoError(t, os.MkdirAll(clineSessionDir, 0o755))
	require.NoError(t, os.WriteFile(aiderHistory, []byte("# aider\n"), 0o644))
	require.NoError(t, os.WriteFile(windsurfStateDB, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(windsurfWorkspaceJSON, []byte("{}\n"), 0o644))
	clineMeta := filepath.Join(clineSessionDir, "sess-test.json")
	clineMsg := filepath.Join(clineSessionDir, "sess-test.messages.json")
	clineTm := filepath.Join(clineSessionDir, "sess-test__teamtask__git-scout__t1.messages.json")
	require.NoError(t, os.WriteFile(clineMeta, []byte(`{"session_id":"sess-test"}`), 0o644))
	require.NoError(t, os.WriteFile(clineMsg, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(clineTm, []byte(`{"messages":[]}`), 0o644))
	clineSecretDir := filepath.Join(clineRoot, "data", "sessions", ".secret")
	require.NoError(t, os.MkdirAll(clineSecretDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clineSecretDir, ".secret.json"), []byte(`{"session_id":".secret"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clineSecretDir, ".secret.messages.json"), []byte(`{"messages":[]}`), 0o644))
	codexIndex := filepath.Join(home, ".codex", parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(codexIndex, []byte("{}\n"), 0o644))

	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(ssh.BuildResolveScriptForTest())
	cmd.Env = []string{"HOME=" + home, "AIDER_DIR=" + aiderRoot, "DEVIN_DIR=" + devinDir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ssh resolver output: %s", out)
	sshDirs, sshFiles, sshExtra, _ := ssh.ParseResolvedTargetsWithFilesForTest(string(out))

	goTargets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {claudeDir},
			parser.AgentCodex:    {codexDir},
			parser.AgentDevin:    {devinDir},
			parser.AgentAider:    {aiderRoot},
			parser.AgentWindsurf: {windsurfUserRoot},
			parser.AgentPoolside: {poolsideRoot},
			parser.AgentCline:    {clineRoot},
		},
	})
	assert.ElementsMatch(t, sshDirs[parser.AgentClaude], goTargets.Dirs[parser.AgentClaude])
	assert.ElementsMatch(t, sshDirs[parser.AgentCodex], goTargets.Dirs[parser.AgentCodex])
	assert.NotContains(t, sshDirs, parser.AgentDevin)
	assert.NotContains(t, goTargets.Dirs, parser.AgentDevin)
	assert.ElementsMatch(t, sshDirs[parser.AgentAider], goTargets.Dirs[parser.AgentAider])
	assert.ElementsMatch(t, []string{windsurfUserRoot}, sshDirs[parser.AgentWindsurf])
	assert.ElementsMatch(t, sshDirs[parser.AgentWindsurf], goTargets.Dirs[parser.AgentWindsurf])
	assert.ElementsMatch(t, []string{
		windsurfStateDB,
		windsurfWorkspaceJSON,
	}, sshFiles[parser.AgentWindsurf])
	assert.ElementsMatch(t, []string{
		windsurfStateDB,
		windsurfWorkspaceJSON,
	}, goTargets.Files[parser.AgentWindsurf])
	assert.ElementsMatch(t, sshFiles[parser.AgentWindsurf], goTargets.Files[parser.AgentWindsurf])
	assert.NotContains(t, sshDirs[parser.AgentWindsurf], windsurfWorkspaceRoot)
	assert.ElementsMatch(t, sshExtra, goTargets.AllExtraFiles())
	// Poolside: both resolvers must narrow to the trajectories/
	// subdirectory, not the application-data root.
	assert.ElementsMatch(t, []string{poolsideTrajectories}, sshDirs[parser.AgentPoolside])
	assert.ElementsMatch(t, sshDirs[parser.AgentPoolside], goTargets.Dirs[parser.AgentPoolside])
	// Cline: both resolvers must emit only session files.
	assert.ElementsMatch(t, []string{clineRoot}, sshDirs[parser.AgentCline])
	assert.ElementsMatch(t, sshDirs[parser.AgentCline], goTargets.Dirs[parser.AgentCline])
	assert.ElementsMatch(t, []string{clineMeta, clineMsg, clineTm}, sshFiles[parser.AgentCline])
	assert.ElementsMatch(t, []string{clineMeta, clineMsg, clineTm}, goTargets.Files[parser.AgentCline])
}

func TestSelectAllowedFiles(t *testing.T) {
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/home/u/.claude/projects"},
			parser.AgentAider:  {"/home/u/proj/.aider.chat.history.md"},
			parser.AgentCodex: {
				`C:\Users\u\.codex\sessions`,
				`\\server\share\.codex\sessions`,
			},
		},
		ExtraFiles: []string{"/home/u/.codex/session_index.jsonl"},
	}
	tests := []struct {
		name  string
		files []string
		ok    bool
	}{
		{"under allowed dir", []string{"/home/u/.claude/projects/p/s.jsonl"}, true},
		{"nested under allowed dir", []string{"/home/u/.claude/projects/a/b/c.jsonl"}, true},
		{"exact extra file", []string{"/home/u/.codex/session_index.jsonl"}, true},
		{"exact allowed dir root", []string{"/home/u/.claude/projects"}, true},
		{"exact aider file root", []string{"/home/u/proj/.aider.chat.history.md"}, true},
		{"windows drive path under allowed dir", []string{
			`C:\Users\u\.codex\sessions\2026\s.jsonl`,
		}, true},
		{"unc path under allowed unc root", []string{
			`\\server\share\.codex\sessions\2026\s.jsonl`,
		}, true},
		{"posix path colliding with drive root archive name", []string{
			"/__drive_C/Users/u/.codex/sessions/secret.jsonl",
		}, false},
		{"posix path colliding with unc root archive name", []string{
			"/__unc/server/share/.codex/sessions/secret.jsonl",
		}, false},
		{"outside allowed dirs", []string{"/etc/passwd"}, false},
		{"prefix sibling escape", []string{"/home/u/.claude/projects-evil/x"}, false},
		{"dot dot traversal", []string{"/home/u/.claude/projects/../../etc/passwd"}, false},
		{"relative path rejected", []string{"home/u/.claude/projects/p/s.jsonl"}, false},
		{"one bad entry rejects all", []string{
			"/home/u/.claude/projects/p/s.jsonl", "/etc/passwd",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected, ok := remotesync.SelectAllowedFiles(allowed, tt.files)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.files, selected)
			} else {
				assert.Nil(t, selected)
			}
		})
	}
}

func TestSelectAllowedFilesRejectsSymlinkAncestorEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on windows")
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.jsonl")
	require.NoError(t, os.WriteFile(victim, []byte("secret"), 0o644))

	root := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))
	nested := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	legit := filepath.Join(nested, "s.jsonl")
	require.NoError(t, os.WriteFile(legit, []byte("session"), 0o644))

	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
	}

	_, ok := remotesync.SelectAllowedFiles(allowed, []string{
		filepath.Join(root, "link", "victim.jsonl"),
	})
	assert.False(t, ok, "symlinked ancestor must not validate")

	// An in-root symlink pointing back inside the root is rejected
	// too: manifests never list paths behind symlinks, so delta
	// validation must not accept them either.
	require.NoError(t, os.Symlink(nested, filepath.Join(root, "alias")))
	_, ok = remotesync.SelectAllowedFiles(allowed, []string{
		filepath.Join(root, "alias", "s.jsonl"),
	})
	assert.False(t, ok, "in-root symlink component must not validate")

	// A symlinked component merely NAMED with a ".." prefix must not
	// be mistaken for a parent escape and skip the symlink walk.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "..alias")))
	_, ok = remotesync.SelectAllowedFiles(allowed, []string{
		filepath.Join(root, "..alias", "victim.jsonl"),
	})
	assert.False(t, ok, "dot-dot-prefixed symlink component must not validate")

	// A root that is itself a symlink streams nothing in manifests or
	// full archives, so delta requests under it are rejected.
	rootLink := filepath.Join(t.TempDir(), "root-link")
	require.NoError(t, os.Symlink(root, rootLink))
	_, ok = remotesync.SelectAllowedFiles(remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {rootLink}},
	}, []string{filepath.Join(rootLink, "project", "s.jsonl")})
	assert.False(t, ok, "symlinked root must not validate")

	selected, ok := remotesync.SelectAllowedFiles(allowed, []string{legit})
	require.True(t, ok)
	assert.Equal(t, []string{legit}, selected)
}

func TestSelectAllowedFilesRejectsFileScopedAgentDirs(t *testing.T) {
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {"/home/u/.claude/projects"},
			parser.AgentWindsurf: {"/home/u/Windsurf/User"},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {
				"/home/u/Windsurf/User/workspaceStorage/a/state.vscdb",
			},
		},
	}

	// A raw file under the Windsurf root must not validate as a delta:
	// the full archive streams only a sanitized subset for Windsurf.
	_, ok := remotesync.SelectAllowedFiles(allowed, []string{
		"/home/u/Windsurf/User/workspaceStorage/a/state.vscdb",
	})
	assert.False(t, ok, "raw file under file-scoped agent dir must be rejected")
	_, ok = remotesync.SelectAllowedFiles(allowed, []string{
		"/home/u/Windsurf/User/workspaceStorage/a/extension-secret.json",
	})
	assert.False(t, ok, "secret under file-scoped agent dir must be rejected")

	// Non-file-scoped agents still accept files under their dirs.
	selected, ok := remotesync.SelectAllowedFiles(allowed, []string{
		"/home/u/.claude/projects/p/s.jsonl",
	})
	require.True(t, ok)
	assert.Equal(t, []string{"/home/u/.claude/projects/p/s.jsonl"}, selected)
}

func TestRooCodeRemoteSyncExportsOnlySessionFiles(t *testing.T) {
	root := t.TempDir()
	rooRoot := filepath.Join(root, "globalStorage", "rooveterinaryinc.roo-cline")
	task1 := filepath.Join(rooRoot, "tasks", "task-1")
	task2 := filepath.Join(rooRoot, "tasks", "task-2")
	settingsDir := filepath.Join(rooRoot, "settings")
	checkpoints := filepath.Join(task1, "checkpoints")
	cacheDir := filepath.Join(rooRoot, "cache")
	require.NoError(t, os.MkdirAll(task1, 0o755))
	require.NoError(t, os.MkdirAll(task2, 0o755))
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.MkdirAll(checkpoints, 0o755))
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))

	task1History := filepath.Join(task1, "history_item.json")
	task1Messages := filepath.Join(task1, "ui_messages.json")
	task2History := filepath.Join(task2, "history_item.json")
	mcpSettings := filepath.Join(settingsDir, "mcp_settings.json")
	checkpointBlob := filepath.Join(checkpoints, "checkpoint.bin")
	cacheBlob := filepath.Join(cacheDir, "models.json")
	require.NoError(t, os.WriteFile(task1History,
		[]byte(`{"id":"task-1","ts":1,"task":"t"}`), 0o644))
	require.NoError(t, os.WriteFile(task1Messages, []byte(`[]`), 0o644))
	require.NoError(t, os.WriteFile(task2History,
		[]byte(`{"id":"task-2","ts":2,"task":"t"}`), 0o644))
	require.NoError(t, os.WriteFile(mcpSettings,
		[]byte(`{"mcpServers":{"s":{"env":{"API_KEY":"sk-secret"}}}}`), 0o644))
	require.NoError(t, os.WriteFile(checkpointBlob, []byte("checkpoint"), 0o644))
	require.NoError(t, os.WriteFile(cacheBlob, []byte("cache"), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooRoot},
		},
	})

	// The root stays in Dirs for target bookkeeping, but the export
	// is file-scoped to the discovered session files only.
	assert.Equal(t, []string{rooRoot}, targets.Dirs[parser.AgentRooCode])
	assert.ElementsMatch(t, []string{
		task1History,
		task1Messages,
		task2History,
	}, targets.Files[parser.AgentRooCode])

	// Full transfer: the archive must contain the session files and
	// nothing else from the RooCode tree.
	var buf bytes.Buffer
	require.NoError(t, remotesync.WriteArchive(&buf, targets))
	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	joined := strings.Join(names, "\n")
	assert.Contains(t, joined, "task-1/history_item.json")
	assert.Contains(t, joined, "task-1/ui_messages.json")
	assert.Contains(t, joined, "task-2/history_item.json")
	assert.NotContains(t, joined, "mcp_settings.json")
	assert.NotContains(t, joined, "checkpoint")
	assert.NotContains(t, joined, "cache")

	// Delta transfer: raw files under the RooCode root must not
	// validate as delta requests, and the root is not a delta root.
	_, ok := remotesync.SelectAllowedFiles(targets, []string{mcpSettings})
	assert.False(t, ok, "settings under the RooCode root must be rejected")
	_, ok = remotesync.SelectAllowedFiles(targets, []string{checkpointBlob})
	assert.False(t, ok, "checkpoint data under the RooCode root must be rejected")
	assert.NotContains(t, targets.DeltaAllowedRoots(), rooRoot)

	// The export is verbatim, so the curated files ride the
	// manifest/delta path: the manifest lists exactly them, they are
	// valid delta requests and delta roots, and no separate per-sync
	// full archive remains (the file-scoped split is empty).
	manifest, err := remotesync.BuildManifest(targets)
	require.NoError(t, err)
	manifestPaths := make([]string, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		manifestPaths = append(manifestPaths, entry.Path)
	}
	assert.ElementsMatch(t, []string{
		task1History,
		task1Messages,
		task2History,
	}, manifestPaths)

	dirScoped, fileScoped := targets.SplitFileScoped()
	assert.True(t, fileScoped.IsEmpty(),
		"a verbatim agent must not fall back to per-sync full archives")
	assert.Equal(t, targets.Files, dirScoped.Files)

	files, ok := remotesync.SelectAllowedFiles(targets, []string{task1Messages})
	require.True(t, ok, "a curated transcript must validate as a delta request")
	var delta bytes.Buffer
	require.NoError(t, remotesync.WriteArchiveFiles(
		&delta, targets, files))
	deltaNames := []string{}
	dr := tar.NewReader(&delta)
	for {
		hdr, err := dr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		deltaNames = append(deltaNames, hdr.Name)
	}
	require.Len(t, deltaNames, 1,
		"one changed transcript must transfer alone")
	assert.Contains(t, deltaNames[0], "task-1/ui_messages.json")
}

// A transcript deleted between the client's target fetch and its next
// request must not fail validation: targets are re-resolved per
// request, so the stale client set names a file the fresh resolution
// no longer contains. Session-shaped paths under a still-allowed root
// are authorized (the writers tolerate the missing file); everything
// else under the root stays rejected.
func TestRooCodeRemoteSyncToleratesVanishedSessionFile(t *testing.T) {
	root := t.TempDir()
	rooRoot := filepath.Join(root, "globalStorage", "rooveterinaryinc.roo-cline")
	task1 := filepath.Join(rooRoot, "tasks", "task-1")
	task2 := filepath.Join(rooRoot, "tasks", "task-2")
	require.NoError(t, os.MkdirAll(task1, 0o755))
	require.NoError(t, os.MkdirAll(task2, 0o755))
	task1History := filepath.Join(task1, "history_item.json")
	task1Messages := filepath.Join(task1, "ui_messages.json")
	task2History := filepath.Join(task2, "history_item.json")
	require.NoError(t, os.WriteFile(task1History,
		[]byte(`{"id":"task-1","ts":1,"task":"t"}`), 0o644))
	require.NoError(t, os.WriteFile(task1Messages, []byte(`[]`), 0o644))
	require.NoError(t, os.WriteFile(task2History,
		[]byte(`{"id":"task-2","ts":2,"task":"t"}`), 0o644))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooRoot},
		},
	}
	staleClientTargets := resolveTargetsForTest(t, cfg)
	require.Contains(t, staleClientTargets.Files[parser.AgentRooCode],
		task1Messages)

	// The whole task vanishes on the remote before the next request.
	require.NoError(t, os.RemoveAll(task1))
	freshServerTargets := resolveTargetsForTest(t, cfg)
	assert.NotContains(t, freshServerTargets.Files[parser.AgentRooCode],
		task1Messages)

	// Target validation must still accept the stale set, and the full
	// archive must stream the surviving files while skipping the
	// vanished ones.
	selected, ok := remotesync.SelectAllowedTargets(
		freshServerTargets, staleClientTargets,
	)
	require.True(t, ok,
		"a vanished session file must not fail the whole request")
	var buf bytes.Buffer
	require.NoError(t, remotesync.WriteArchive(&buf, selected))
	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	joined := strings.Join(names, "\n")
	assert.Contains(t, joined, "task-2/history_item.json")
	assert.NotContains(t, joined, "task-1")

	// The manifest tolerates the vanished entries the same way.
	manifest, err := remotesync.BuildManifest(selected)
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, task2History, manifest.Files[0].Path)

	// Delta requests for the vanished file validate and stream nothing.
	files, ok := remotesync.SelectAllowedFiles(
		freshServerTargets, []string{task1Messages},
	)
	require.True(t, ok,
		"a vanished session file must validate as a delta request")
	var delta bytes.Buffer
	require.NoError(t, remotesync.WriteArchiveFiles(
		&delta, freshServerTargets, files))
	dr := tar.NewReader(&delta)
	_, err = dr.Next()
	assert.Equal(t, io.EOF, err, "the vanished file streams nothing")

	// The shape authorization stays strict: nothing else under the
	// root validates, present or not.
	for _, path := range []string{
		filepath.Join(rooRoot, "settings", "mcp_settings.json"),
		filepath.Join(rooRoot, "tasks", "task-1", "checkpoint.bin"),
		filepath.Join(rooRoot, "tasks", "_marker", "history_item.json"),
		filepath.Join(rooRoot, "history_item.json"),
	} {
		_, ok := remotesync.SelectAllowedFiles(freshServerTargets, []string{path})
		assert.False(t, ok, "non-session path must stay rejected: %s", path)
		stale := staleClientTargets
		stale.Files = map[parser.AgentType][]string{
			parser.AgentRooCode: {path},
		}
		_, ok = remotesync.SelectAllowedTargets(freshServerTargets, stale)
		assert.False(t, ok,
			"non-session target must stay rejected: %s", path)
	}
}

func TestRooCodeRemoteSyncSkipsRootWithoutSessions(t *testing.T) {
	root := t.TempDir()
	rooRoot := filepath.Join(root, "globalStorage", "rooveterinaryinc.roo-cline")
	settingsDir := filepath.Join(rooRoot, "settings")
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(settingsDir, "mcp_settings.json"),
		[]byte(`{"mcpServers":{}}`), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooRoot},
		},
	})

	// With no discovered sessions there is nothing to export — the
	// root must not fall back to a recursive directory target.
	assert.NotContains(t, targets.Dirs, parser.AgentRooCode)
	assert.NotContains(t, targets.Files, parser.AgentRooCode)
}

func TestClineRemoteSyncExportsOnlySessionFiles(t *testing.T) {
	root := t.TempDir()
	clineRoot := filepath.Join(root, ".cline")
	sess1 := filepath.Join(clineRoot, "data", "sessions", "sess-1")
	sess2 := filepath.Join(clineRoot, "data", "sessions", "sess-2")
	settingsDir := filepath.Join(clineRoot, "settings")
	checkpoints := filepath.Join(clineRoot, "data", "checkpoints")
	require.NoError(t, os.MkdirAll(sess1, 0o755))
	require.NoError(t, os.MkdirAll(sess2, 0o755))
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	require.NoError(t, os.MkdirAll(checkpoints, 0o755))

	sess1Meta := filepath.Join(sess1, "sess-1.json")
	sess1Messages := filepath.Join(sess1, "sess-1.messages.json")
	sess2Meta := filepath.Join(sess2, "sess-2.json")
	mcpSettings := filepath.Join(settingsDir, "mcp_settings.json")
	checkpointBlob := filepath.Join(checkpoints, "checkpoint.bin")
	require.NoError(t, os.WriteFile(sess1Meta,
		[]byte(`{"session_id":"sess-1","started_at":"2026-09-10T10:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(sess1Messages, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(sess2Meta,
		[]byte(`{"session_id":"sess-2","started_at":"2026-09-10T11:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(mcpSettings,
		[]byte(`{"mcpServers":{"s":{"env":{"API_KEY":"sk-secret"}}}}`), 0o644))
	require.NoError(t, os.WriteFile(checkpointBlob, []byte("checkpoint"), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
	})

	assert.Equal(t, []string{clineRoot}, targets.Dirs[parser.AgentCline])
	assert.ElementsMatch(t, []string{
		sess1Meta,
		sess1Messages,
		sess2Meta,
	}, targets.Files[parser.AgentCline])

	var buf bytes.Buffer
	require.NoError(t, remotesync.WriteArchive(&buf, targets))
	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	joined := strings.Join(names, "\n")
	assert.Contains(t, joined, "data/sessions/sess-1/sess-1.json")
	assert.Contains(t, joined, "data/sessions/sess-1/sess-1.messages.json")
	assert.Contains(t, joined, "data/sessions/sess-2/sess-2.json")
	assert.NotContains(t, joined, "mcp_settings.json")
	assert.NotContains(t, joined, "checkpoint")

	_, ok := remotesync.SelectAllowedFiles(targets, []string{mcpSettings})
	assert.False(t, ok, "settings under the Cline root must be rejected")
	_, ok = remotesync.SelectAllowedFiles(targets, []string{checkpointBlob})
	assert.False(t, ok, "checkpoint data under the Cline root must be rejected")
	assert.NotContains(t, targets.DeltaAllowedRoots(), clineRoot)
}

func TestClineRemoteSyncArchiveRejectsBackslashSessionIDs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslashes in directory names are not supported on Windows")
	}
	root := t.TempDir()
	clineRoot := filepath.Join(root, ".cline")
	validSess := filepath.Join(clineRoot, "data", "sessions", "sess-valid")
	require.NoError(t, os.MkdirAll(validSess, 0o755))
	validMeta := filepath.Join(validSess, "sess-valid.json")
	validMsgs := filepath.Join(validSess, "sess-valid.messages.json")
	require.NoError(t, os.WriteFile(validMeta,
		[]byte(`{"session_id":"sess-valid","started_at":"2026-09-10T10:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(validMsgs, []byte(`{"messages":[]}`), 0o644))

	// Hostile session with a backslash in the session directory name
	hostileSess := filepath.Join(clineRoot, "data", "sessions", `sess\escape`)
	require.NoError(t, os.MkdirAll(hostileSess, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(hostileSess, `sess\escape.json`),
		[]byte(`{"session_id":"sess\\escape","started_at":"2026-09-10T10:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(hostileSess, `sess\escape.messages.json`),
		[]byte(`{"messages":[]}`), 0o644))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
	})

	assert.Contains(t, targets.Files[parser.AgentCline], validMeta)
	assert.Contains(t, targets.Files[parser.AgentCline], validMsgs)
	for _, file := range targets.Files[parser.AgentCline] {
		assert.NotContains(t, file, "escape",
			"session with backslash in ID must not be resolved as fresh target")
	}

	var buf bytes.Buffer
	require.NoError(t, remotesync.WriteArchive(&buf, targets))
	tr := tar.NewReader(&buf)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	joined := strings.Join(names, "\n")
	assert.Contains(t, joined, "data/sessions/sess-valid/sess-valid.json")
	assert.Contains(t, joined, "data/sessions/sess-valid/sess-valid.messages.json")
	assert.NotContains(t, joined, "escape",
		"session with backslash in ID must not be archived")
}

func TestClineRemoteSyncToleratesVanishedSessionFile(t *testing.T) {
	// 1. Configured-root layout (e.g. ~/.cline)
	root := t.TempDir()
	clineRoot := filepath.Join(root, ".cline")
	sess1 := filepath.Join(clineRoot, "data", "sessions", "sess-1")
	sess2 := filepath.Join(clineRoot, "data", "sessions", "sess-2")
	require.NoError(t, os.MkdirAll(sess1, 0o755))
	require.NoError(t, os.MkdirAll(sess2, 0o755))
	sess1Meta := filepath.Join(sess1, "sess-1.json")
	sess1Messages := filepath.Join(sess1, "sess-1.messages.json")
	sess1Teammate := filepath.Join(sess1, "scout__t1.messages.json")
	sess2Meta := filepath.Join(sess2, "sess-2.json")
	require.NoError(t, os.WriteFile(sess1Meta,
		[]byte(`{"session_id":"sess-1","started_at":"2026-09-10T10:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(sess1Messages, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(sess1Teammate, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(sess2Meta,
		[]byte(`{"session_id":"sess-2","started_at":"2026-09-10T11:00:00Z"}`), 0o644))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
	}
	staleClientTargets := resolveTargetsForTest(t, cfg)
	require.Contains(t, staleClientTargets.Files[parser.AgentCline], sess1Messages)
	require.Contains(t, staleClientTargets.Files[parser.AgentCline], sess1Teammate)

	require.NoError(t, os.RemoveAll(sess1))
	freshServerTargets := resolveTargetsForTest(t, cfg)
	assert.NotContains(t, freshServerTargets.Files[parser.AgentCline], sess1Messages)

	selected, ok := remotesync.SelectAllowedTargets(
		freshServerTargets, staleClientTargets,
	)
	require.True(t, ok, "a vanished session file under configured root must not fail request")
	var buf bytes.Buffer
	require.NoError(t, remotesync.WriteArchive(&buf, selected))
	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	joined := strings.Join(names, "\n")
	assert.Contains(t, joined, "sess-2/sess-2.json")
	assert.NotContains(t, joined, "sess-1")

	files, ok := remotesync.SelectAllowedFiles(
		freshServerTargets, []string{sess1Messages, sess1Teammate},
	)
	require.True(t, ok, "vanished session and teammate files must validate as delta request")
	var delta bytes.Buffer
	require.NoError(t, remotesync.WriteArchiveFiles(&delta, freshServerTargets, files))
	dr := tar.NewReader(&delta)
	_, err := dr.Next()
	assert.Equal(t, io.EOF, err, "vanished file streams nothing")

	// Non-session paths must stay rejected
	for _, path := range []string{
		filepath.Join(clineRoot, "settings", "mcp_settings.json"),
		filepath.Join(clineRoot, "data", "checkpoints", "checkpoint.bin"),
		filepath.Join(clineRoot, "data", "sessions", "_meta", "sess-1.json"),
		filepath.Join(clineRoot, "sessions", "sess-1", "sess-1.json"),
		filepath.Join(clineRoot, "sess-1.json"),
	} {
		_, ok := remotesync.SelectAllowedFiles(freshServerTargets, []string{path})
		assert.False(t, ok, "non-session path must stay rejected: %s", path)
	}

	// 2. Direct-sessions-root layout (e.g. ~/.cline/data/sessions)
	directRoot := filepath.Join(root, "direct", "sessions")
	directSess1 := filepath.Join(directRoot, "dsess-1")
	directSess2 := filepath.Join(directRoot, "dsess-2")
	require.NoError(t, os.MkdirAll(directSess1, 0o755))
	require.NoError(t, os.MkdirAll(directSess2, 0o755))
	dsess1Meta := filepath.Join(directSess1, "dsess-1.json")
	dsess1Teammate := filepath.Join(directSess1, "scout__t1.messages.json")
	dsess2Meta := filepath.Join(directSess2, "dsess-2.json")
	require.NoError(t, os.WriteFile(dsess1Meta,
		[]byte(`{"session_id":"dsess-1","started_at":"2026-09-10T10:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(dsess1Teammate, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(dsess2Meta,
		[]byte(`{"session_id":"dsess-2","started_at":"2026-09-10T11:00:00Z"}`), 0o644))

	directCfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {directRoot},
		},
	}
	staleDirectTargets := resolveTargetsForTest(t, directCfg)
	require.Contains(t, staleDirectTargets.Files[parser.AgentCline], dsess1Meta)
	require.Contains(t, staleDirectTargets.Files[parser.AgentCline], dsess1Teammate)

	require.NoError(t, os.RemoveAll(directSess1))
	freshDirectTargets := resolveTargetsForTest(t, directCfg)
	assert.NotContains(t, freshDirectTargets.Files[parser.AgentCline], dsess1Meta)
	assert.NotContains(t, freshDirectTargets.Files[parser.AgentCline], dsess1Teammate)

	_, ok = remotesync.SelectAllowedTargets(freshDirectTargets, staleDirectTargets)
	require.True(t, ok, "a vanished session file under direct sessions root must not fail request")

	_, ok = remotesync.SelectAllowedFiles(freshDirectTargets, []string{dsess1Teammate})
	require.True(t, ok, "vanished teammate under direct sessions root must validate")

	// Non-session paths under direct sessions root must stay rejected
	for _, path := range []string{
		filepath.Join(directRoot, "sessions", "dsess-1", "dsess-1.json"),
		filepath.Join(directRoot, "data", "sessions", "dsess-1", "dsess-1.json"),
		filepath.Join(directRoot, "dsess-1.json"),
	} {
		_, ok := remotesync.SelectAllowedFiles(freshDirectTargets, []string{path})
		assert.False(t, ok, "non-session path under direct root must stay rejected: %s", path)
	}
}

func TestClineRemoteSyncPreservesRootWhenEmpty(t *testing.T) {
	root := t.TempDir()
	clineRoot := filepath.Join(root, ".cline")
	sessionsDir := filepath.Join(clineRoot, "data", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
	}
	targets := resolveTargetsForTest(t, cfg)
	assert.Contains(t, targets.Dirs[parser.AgentCline], clineRoot)
	require.Contains(t, targets.Files, parser.AgentCline, "must retain files entry for Cline")
	assert.Empty(t, targets.Files[parser.AgentCline], "file slice must be empty when no sessions exist")

	// Verify a stale client request for a deleted session can still be authorized for eviction
	staleTargets := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
		Files: map[parser.AgentType][]string{
			parser.AgentCline: {filepath.Join(sessionsDir, "old-sess", "old-sess.json")},
		},
	}
	selected, ok := remotesync.SelectAllowedTargets(targets, staleTargets)
	require.True(t, ok, "stale session file under empty Cline root must remain authorized for eviction")
	assert.Empty(t, selected.Files[parser.AgentCline])
}

func TestCursorRemoteTargetsExcludeChatsRoot(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	projects := filepath.Join(home, ".cursor", "projects")
	chats := filepath.Join(home, ".cursor", "chats")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	require.NoError(t, os.MkdirAll(chats, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(chats, "workspace", "session"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(chats, "workspace", "session", "store.db"),
		[]byte("store"),
		0o644,
	))

	root, meta, err := parser.ResolveProviderRoot(parser.AgentCursor, projects)
	require.NoError(t, err)
	require.NotEmpty(t, meta)
	assert.True(t, samePathForTest(t, meta, chats))

	targets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCursor: {root},
		},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCursor: {root: {meta}},
		},
	})
	require.Contains(t, targets.Dirs, parser.AgentCursor)
	assert.Contains(t, targets.Dirs[parser.AgentCursor], root)
	for _, dir := range targets.Dirs[parser.AgentCursor] {
		assert.False(t, samePathForTest(t, dir, meta), "chats metadata must not be a transfer dir: %s", dir)
		assert.NotEqual(t, "chats", filepath.Base(dir))
	}
	for _, files := range targets.Files {
		for _, file := range files {
			assert.NotContains(t, file, string(filepath.Separator)+"chats"+string(filepath.Separator))
			assert.NotContains(t, filepath.Base(file), "store.db")
		}
	}
}

func samePathForTest(t *testing.T, a, b string) bool {
	t.Helper()
	aAbs, err := filepath.Abs(a)
	require.NoError(t, err)
	bAbs, err := filepath.Abs(b)
	require.NoError(t, err)
	return filepath.Clean(aAbs) == filepath.Clean(bAbs) ||
		strings.EqualFold(filepath.Clean(aAbs), filepath.Clean(bAbs))
}

func TestClineRemoteSyncRejectsSymlinkedAncestorsAndSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	outsideSessions := filepath.Join(outsideDir, "outside_sessions", "sess-outside")
	require.NoError(t, os.MkdirAll(outsideSessions, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outsideSessions, "sess-outside.json"),
		[]byte(`{"session_id":"sess-outside"}`),
		0o644,
	))

	// Case 1: data is a symlink pointing outside targetRoot.
	root1, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	clineRoot1 := filepath.Join(root1, ".cline")
	require.NoError(t, os.MkdirAll(clineRoot1, 0o755))
	outsideData := filepath.Join(outsideDir, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(outsideData, "sessions", "sess-outside"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(outsideData, "sessions", "sess-outside", "sess-outside.json"),
		[]byte(`{"session_id":"sess-outside"}`),
		0o644,
	))
	require.NoError(t, os.Symlink(outsideData, filepath.Join(clineRoot1, "data")))

	targets1 := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot1},
		},
	})
	assert.Empty(t, targets1.Dirs[parser.AgentCline])
	assert.Empty(t, targets1.Files[parser.AgentCline])

	// Case 2: data/sessions is a symlink pointing outside targetRoot.
	root2, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	clineRoot2 := filepath.Join(root2, ".cline")
	require.NoError(t, os.MkdirAll(filepath.Join(clineRoot2, "data"), 0o755))
	require.NoError(t, os.Symlink(filepath.Dir(outsideSessions), filepath.Join(clineRoot2, "data", "sessions")))

	targets2 := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot2},
		},
	})
	assert.Empty(t, targets2.Dirs[parser.AgentCline])
	assert.Empty(t, targets2.Files[parser.AgentCline])

	// Case 3: session dir within data/sessions is a symlink pointing outside targetRoot.
	root3, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	clineRoot3 := filepath.Join(root3, ".cline")
	sessionsDir3 := filepath.Join(clineRoot3, "data", "sessions")
	validSessionDir := filepath.Join(sessionsDir3, "sess-valid")
	require.NoError(t, os.MkdirAll(validSessionDir, 0o755))
	validMeta := filepath.Join(validSessionDir, "sess-valid.json")
	require.NoError(t, os.WriteFile(validMeta, []byte(`{"session_id":"sess-valid"}`), 0o644))
	require.NoError(t, os.Symlink(outsideSessions, filepath.Join(sessionsDir3, "sess-outside")))

	targets3 := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot3},
		},
	})
	assert.Contains(t, targets3.Dirs[parser.AgentCline], clineRoot3)
	assert.Contains(t, targets3.Files[parser.AgentCline], validMeta)
	for _, file := range targets3.Files[parser.AgentCline] {
		assert.NotContains(t, file, "sess-outside", "symlinked session escaping root must be rejected")
	}
}

func TestClineRootSymlinkParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	realCline := filepath.Join(outsideDir, "real-cline")
	realSessions := filepath.Join(realCline, "data", "sessions", "sess-outside")
	require.NoError(t, os.MkdirAll(realSessions, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(realSessions, "sess-outside.json"),
		[]byte(`{"session_id":"sess-outside"}`),
		0o644,
	))

	// Case 1: The configured Cline root itself is a symlink.
	home1, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	symlinkedRoot := filepath.Join(home1, ".cline")
	require.NoError(t, os.Symlink(realCline, symlinkedRoot))

	goTargets1 := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {symlinkedRoot},
		},
	})
	assert.Empty(t, goTargets1.Dirs[parser.AgentCline])
	assert.Empty(t, goTargets1.Files[parser.AgentCline])

	cmd1 := exec.Command("sh")
	cmd1.Stdin = strings.NewReader(ssh.BuildResolveScriptForTest())
	cmd1.Env = []string{"HOME=" + home1}
	out1, err := cmd1.CombinedOutput()
	require.NoError(t, err, "ssh output: %s", out1)
	sshDirs1, sshFiles1, _, _ := ssh.ParseResolvedTargetsWithFilesForTest(string(out1))
	assert.Empty(t, sshDirs1[parser.AgentCline])
	assert.Empty(t, sshFiles1[parser.AgentCline])
	assert.ElementsMatch(t, sshDirs1[parser.AgentCline], goTargets1.Dirs[parser.AgentCline])
	assert.ElementsMatch(t, sshFiles1[parser.AgentCline], goTargets1.Files[parser.AgentCline])

	// Case 2: The direct sessions root is a symlink.
	home2, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	symlinkedDirect := filepath.Join(home2, "direct-sessions")
	require.NoError(t, os.Symlink(filepath.Join(realCline, "data", "sessions"), symlinkedDirect))

	goTargets2 := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {symlinkedDirect},
		},
	})
	assert.Empty(t, goTargets2.Dirs[parser.AgentCline])
	assert.Empty(t, goTargets2.Files[parser.AgentCline])

	cmd2 := exec.Command("sh")
	cmd2.Stdin = strings.NewReader(ssh.BuildResolveScriptForTest())
	cmd2.Env = []string{"HOME=" + home2, "CLINE_DIR=" + symlinkedDirect}
	out2, err := cmd2.CombinedOutput()
	require.NoError(t, err, "ssh output: %s", out2)
	sshDirs2, sshFiles2, _, _ := ssh.ParseResolvedTargetsWithFilesForTest(string(out2))
	assert.Empty(t, sshDirs2[parser.AgentCline])
	assert.Empty(t, sshFiles2[parser.AgentCline])
	assert.ElementsMatch(t, sshDirs2[parser.AgentCline], goTargets2.Dirs[parser.AgentCline])
	assert.ElementsMatch(t, sshFiles2[parser.AgentCline], goTargets2.Files[parser.AgentCline])
}

func TestClineLeafSymlinkParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	secretFile := filepath.Join(outsideDir, "secret.json")
	require.NoError(t, os.WriteFile(secretFile, []byte(`{"secret":true}`), 0o644))

	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	clineRoot := filepath.Join(home, ".cline")
	sessionsDir := filepath.Join(clineRoot, "data", "sessions")

	// sess-valid: normal regular metadata and messages, plus teammate messages and symlink.
	sessValid := filepath.Join(sessionsDir, "sess-valid")
	require.NoError(t, os.MkdirAll(sessValid, 0o755))
	validMeta := filepath.Join(sessValid, "sess-valid.json")
	require.NoError(t, os.WriteFile(validMeta, []byte(`{"session_id":"sess-valid"}`), 0o644))
	validMsgs := filepath.Join(sessValid, "sess-valid.messages.json")
	require.NoError(t, os.WriteFile(validMsgs, []byte(`{"messages":[]}`), 0o644))
	validTm := filepath.Join(sessValid, "sess-valid__team__agent.messages.json")
	require.NoError(t, os.WriteFile(validTm, []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.Symlink(secretFile, filepath.Join(sessValid, "sess-valid__symlink.messages.json")))
	require.NoError(t, os.WriteFile(filepath.Join(sessValid, "sess-valid__bad__.messages.json"), []byte(`{"messages":[]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sessValid, "_sess-valid__skip.messages.json"), []byte(`{"messages":[]}`), 0o644))

	// sess-symlink-meta: metadata file is a symlink pointing outside root.
	sessMetaLink := filepath.Join(sessionsDir, "sess-symlink-meta")
	require.NoError(t, os.MkdirAll(sessMetaLink, 0o755))
	require.NoError(t, os.Symlink(secretFile, filepath.Join(sessMetaLink, "sess-symlink-meta.json")))
	require.NoError(t, os.WriteFile(filepath.Join(sessMetaLink, "sess-symlink-meta.messages.json"), []byte(`{"messages":[]}`), 0o644))

	// sess-symlink-msgs: valid metadata, but messages file is a symlink pointing outside root.
	sessMsgsLink := filepath.Join(sessionsDir, "sess-symlink-msgs")
	require.NoError(t, os.MkdirAll(sessMsgsLink, 0o755))
	validMeta2 := filepath.Join(sessMsgsLink, "sess-symlink-msgs.json")
	require.NoError(t, os.WriteFile(validMeta2, []byte(`{"session_id":"sess-symlink-msgs"}`), 0o644))
	require.NoError(t, os.Symlink(secretFile, filepath.Join(sessMsgsLink, "sess-symlink-msgs.messages.json")))

	goTargets := resolveTargetsForTest(t, config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {clineRoot},
		},
	})
	expectedFiles := []string{validMeta, validMsgs, validTm, validMeta2}
	assert.ElementsMatch(t, expectedFiles, goTargets.Files[parser.AgentCline])

	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(ssh.BuildResolveScriptForTest())
	cmd.Env = []string{"HOME=" + home}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ssh output: %s", out)
	sshDirs, sshFiles, _, _ := ssh.ParseResolvedTargetsWithFilesForTest(string(out))
	assert.ElementsMatch(t, sshDirs[parser.AgentCline], goTargets.Dirs[parser.AgentCline])
	assert.ElementsMatch(t, sshFiles[parser.AgentCline], goTargets.Files[parser.AgentCline])
}
