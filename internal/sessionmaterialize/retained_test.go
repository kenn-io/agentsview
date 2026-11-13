package sessionmaterialize

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func seedRetainedFixture(t *testing.T, root, data string, roots []rawarchive.RootSpec) (string, *db.DB, *rawarchive.Archive) {
	t.Helper()
	ctx := t.Context()
	capture := filepath.Join(root, "capture")
	_, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Roots: roots, Settings: rawarchive.RecoverySettings{LocalMachineName: "fixture"}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	return seed, database, archive
}

func TestMaterializeRetainedCodexLineage(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	codexHome := filepath.Join(root, "codex")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(codexHome, "sessions", pageName), []byte(page))
	dbtest.WriteTestFile(t, filepath.Join(codexHome, "archived_sessions", parentName), []byte(ancestor))
	seed, database, archive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "codex", Path: codexHome}})
	defer database.Close()
	session, err := database.GetSessionFull(ctx, "codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, session)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SESSIONS_DIR", "")
	cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {codexHome}}}
	cwd := t.TempDir()
	configured, err := Materialize(ctx, cfg, database, session, cwd)
	require.NoError(t, err, "configured original must remain usable while the vault is busy")
	assert.NotContains(t, configured.Source, "retained:")
	require.NoError(t, archive.Close())
	// Removing a configured root, while the original remains on disk, may use
	// only the explicitly bound retained source.
	unconfigured := cfg
	unconfigured.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {t.TempDir()}}
	t.Setenv("CODEX_HOME", t.TempDir())
	result, err := Materialize(ctx, unconfigured, database, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "retained:")
	// A configured lexical path escaping its root through a link remains fatal.
	t.Run("configured symlink escape", func(t *testing.T) {
		escapeRoot := t.TempDir()
		link := filepath.Join(escapeRoot, "sessions")
		if err := os.Symlink(filepath.Join(codexHome, "sessions"), link); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		escaped := *session
		escapePath := filepath.Join(link, pageName)
		escaped.FilePath = &escapePath
		escapeCfg := cfg
		escapeCfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {escapeRoot}}
		_, err := Materialize(t.Context(), escapeCfg, database, &escaped, cwd)
		require.ErrorContains(t, err, "outside configured root")
	})
	require.NoError(t, os.Remove(filepath.Join(codexHome, "sessions", pageName)))
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(page)))
	blob := filepath.Join(seed, rawarchive.Directory, "artifacts", "blobs", hash[:2], hash)
	original, err := os.ReadFile(blob)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	corrupt := append([]byte(nil), original...)
	corrupt[0] ^= 1
	require.NoError(t, os.WriteFile(blob, corrupt, 0o600))
	_, err = Materialize(ctx, cfg, database, session, cwd)
	require.ErrorIs(t, err, artifact.ErrArtifactCorrupt)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, os.WriteFile(blob, original, 0o600))
	result, err = Materialize(ctx, cfg, database, session, cwd)
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	assert.Contains(t, result.Source, "retained:")
	for i, want := range []string{ancestor, page} {
		got, err := os.ReadFile(result.Paths[i])
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
}

func TestMaterializeClaudeConfiguredBeforeRetained(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	projects := filepath.Join(root, "claude", "projects")
	name := filepath.Join(projects, "project", id+".jsonl")
	original := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "configured original", id).String()
	dbtest.WriteTestFile(t, name, []byte(original))
	seed, database, archive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
	defer database.Close()
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session)
	cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {projects}}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
	result, err := Materialize(ctx, cfg, database, session, t.TempDir())
	require.NoError(t, err, "configured original must remain usable while the vault is busy")
	assert.Equal(t, name, result.Source)
	require.NoError(t, archive.Close())
	require.NoError(t, os.Remove(name))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	result, err = Materialize(ctx, cfg, database, session, t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, result.Source, "retained:")
}
