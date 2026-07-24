package remotesync_test

import (
	"archive/tar"
	"bytes"
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

	targets := remotesync.ResolveTargets(config.Config{
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
	assert.Contains(t, targets.ExtraFiles, codexIndex)
}

func TestResolveTargetsExcludesTraeProfile(t *testing.T) {
	root := t.TempDir()
	traeRoot := filepath.Join(root, "TRAE", "User")
	claudeRoot := filepath.Join(root, "claude")
	require.NoError(t, os.MkdirAll(traeRoot, 0o755))
	require.NoError(t, os.MkdirAll(claudeRoot, 0o755))

	targets := remotesync.ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentTrae:   {traeRoot},
		parser.AgentClaude: {claudeRoot},
	}})
	assert.NotContains(t, targets.Dirs, parser.AgentTrae)
	assert.Equal(t, []string{claudeRoot}, targets.Dirs[parser.AgentClaude])
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

	targets := remotesync.ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
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

	targets := remotesync.ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
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

	targets := remotesync.ResolveTargets(config.Config{AgentDirs: map[parser.AgentType][]string{
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

	targets := remotesync.ResolveTargets(config.Config{
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
	}, targets.ExtraFiles)
}

func TestResolveTargetsIncludesFlatCustomHermesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom", "hermes-archive")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "child.jsonl"), []byte("{}\n"), 0o644,
	))

	targets := remotesync.ResolveTargets(config.Config{
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

	targets := remotesync.ResolveTargets(config.Config{
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

	targets := remotesync.ResolveTargets(config.Config{
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
	home := t.TempDir()
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
	poolsideRoot := filepath.Join(home, ".local", "share", "poolside")
	poolsideTrajectories := filepath.Join(poolsideRoot, "trajectories")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(devinDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(aiderHistory), 0o755))
	require.NoError(t, os.MkdirAll(windsurfWorkspaceDir, 0o755))
	require.NoError(t, os.MkdirAll(poolsideTrajectories, 0o755))
	require.NoError(t, os.WriteFile(aiderHistory, []byte("# aider\n"), 0o644))
	require.NoError(t, os.WriteFile(windsurfStateDB, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(windsurfWorkspaceJSON, []byte("{}\n"), 0o644))
	codexIndex := filepath.Join(home, ".codex", parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(codexIndex, []byte("{}\n"), 0o644))

	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(ssh.BuildResolveScriptForTest())
	cmd.Env = []string{"HOME=" + home, "AIDER_DIR=" + aiderRoot, "DEVIN_DIR=" + devinDir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "ssh resolver output: %s", out)
	sshDirs, sshFiles, sshExtra := ssh.ParseResolvedTargetsWithFilesForTest(string(out))

	goTargets := remotesync.ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {claudeDir},
			parser.AgentCodex:    {codexDir},
			parser.AgentDevin:    {devinDir},
			parser.AgentAider:    {aiderRoot},
			parser.AgentWindsurf: {windsurfUserRoot},
			parser.AgentPoolside: {poolsideRoot},
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
	assert.ElementsMatch(t, sshExtra, goTargets.ExtraFiles)
	// Poolside: both resolvers must narrow to the trajectories/
	// subdirectory, not the application-data root.
	assert.ElementsMatch(t, []string{poolsideTrajectories}, sshDirs[parser.AgentPoolside])
	assert.ElementsMatch(t, sshDirs[parser.AgentPoolside], goTargets.Dirs[parser.AgentPoolside])
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

	targets := remotesync.ResolveTargets(config.Config{
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
	staleClientTargets := remotesync.ResolveTargets(cfg)
	require.Contains(t, staleClientTargets.Files[parser.AgentRooCode],
		task1Messages)

	// The whole task vanishes on the remote before the next request.
	require.NoError(t, os.RemoveAll(task1))
	freshServerTargets := remotesync.ResolveTargets(cfg)
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

	targets := remotesync.ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooRoot},
		},
	})

	// With no discovered sessions there is nothing to export — the
	// root must not fall back to a recursive directory target.
	assert.NotContains(t, targets.Dirs, parser.AgentRooCode)
	assert.NotContains(t, targets.Files, parser.AgentRooCode)
}
