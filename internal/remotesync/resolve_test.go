package remotesync_test

import (
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
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	require.NoError(t, os.MkdirAll(devinDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(aiderHistory), 0o755))
	require.NoError(t, os.MkdirAll(windsurfWorkspaceDir, 0o755))
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
			parser.AgentClaude: {claudeDir},
			parser.AgentCodex:  {codexDir},
			parser.AgentDevin:  {devinDir},
			parser.AgentAider:  {aiderRoot},
			parser.AgentWindsurf: {
				windsurfUserRoot,
			},
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
