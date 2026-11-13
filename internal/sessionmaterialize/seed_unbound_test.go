package sessionmaterialize

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// The archive is measured with its reader already open, so opening the reader
// for an assertion cannot itself change the sidecar observation.
func seedArchiveState(t *testing.T, seed string) ([]byte, map[string][32]byte, []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	entries, err := os.ReadDir(seed)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	vault := filepath.Join(seed, rawarchive.Directory)
	files := make(map[string][32]byte)
	require.NoError(t, filepath.WalkDir(vault, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(vault, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[rel+"/"] = [32]byte{}
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = sha256.Sum256(content)
		return nil
	}))
	return data, files, names
}

func TestMaterializeUnboundSeededNativeSources(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	for _, tc := range []struct {
		agent     parser.AgentType
		scenarios []string
	}{
		{parser.AgentClaude, []string{"plain", "same ID different roots", "missing captured path", "unowned machine", "recorded machine alias", "recorded root alias", "empty machine", "installation machine"}},
		{parser.AgentCodex, []string{"plain", "revert page"}},
	} {
		agent := tc.agent
		for _, scenario := range tc.scenarios {
			t.Run(string(agent)+"/"+scenario, func(t *testing.T) {
				ctx := t.Context()
				root := t.TempDir()
				t.Setenv("HOME", filepath.Join(root, "home"))
				t.Setenv("USERPROFILE", filepath.Join(root, "home"))
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "home", "claude"))
				t.Setenv("CODEX_HOME", filepath.Join(root, "home", "codex"))
				t.Setenv("CODEX_SESSIONS_DIR", "")
				t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
				data := filepath.Join(root, "data")
				require.NoError(t, os.MkdirAll(data, 0o700))
				dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
				database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = database.Close() })
				require.NoError(t, database.ConfigureArtifactLocalMachine(ctx, device))
				first, selected := filepath.Join(root, "first"), filepath.Join(root, "selected")
				var original, selectedPath, firstPath, nativeID, ancestor string
				if agent == parser.AgentClaude {
					original = testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "selected", id).String()
					other := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "other", id).String()
					selectedPath = filepath.Join(selected, "project", id+".jsonl")
					firstPath = filepath.Join(first, "project", id+".jsonl")
					dbtest.WriteTestFile(t, firstPath, []byte(other))
					nativeID = id
				} else {
					name := "rollout-2026-01-01T00-00-00-" + id + ".jsonl"
					original = testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(id, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "selected", "2026-01-01T00:00:01Z"))
					selectedPath = filepath.Join(selected, "sessions", "2026", "01", "01", name)
					nativeID = "codex:" + id
					if scenario == "revert page" {
						const rollout = "22222222-2222-4222-8222-222222222222"
						ancestor = original
						dbtest.WriteTestFile(t, selectedPath, []byte(ancestor))
						name = "rollout-2026-01-02T00-00-00-" + id + "_" + rollout + ".jsonl"
						selectedPath = filepath.Join(selected, "sessions", "2026", "01", "02", name)
						nativeID += "_" + rollout
						original = testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(id, "/work", "codex_cli_rs", "2026-01-02T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": id, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "revert page", "2026-01-02T00:00:01Z"))
					}
				}
				dbtest.WriteTestFile(t, selectedPath, []byte(original))
				acceptedPath := selectedPath
				discovery := selected
				var rootAlias string
				if scenario == "recorded root alias" {
					rootAlias = filepath.Join(root, "alias")
					if err := os.Symlink(selected, rootAlias); err != nil {
						t.Skipf("directory symlinks unavailable: %v", err)
					}
					rel, err := filepath.Rel(selected, selectedPath)
					require.NoError(t, err)
					selectedPath = filepath.Join(rootAlias, rel)
					discovery = rootAlias
				}
				if agent == parser.AgentCodex {
					discovery = filepath.Join(discovery, "sessions")
				}
				machine := "local"
				if scenario == "empty machine" {
					machine = ""
				} else if scenario == "installation machine" {
					machine = device
				}
				if scenario == "unowned machine" || scenario == "recorded machine alias" {
					machine = "old-host"
				}
				factory, ok := parser.ProviderFactoryByType(agent)
				require.True(t, ok)
				if scenario == "recorded machine alias" {
					const priorID = "33333333-3333-4333-8333-333333333333"
					priorRoot := filepath.Join(root, "prior")
					body := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "prior", priorID).String()
					dbtest.WriteTestFile(t, filepath.Join(priorRoot, "project", priorID+".jsonl"), []byte(body))
					prior := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {priorRoot}}, ProviderFactories: []parser.ProviderFactory{factory}, Machine: machine, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
					priorStats := prior.SyncAll(ctx, nil)
					prior.Close()
					require.Zero(t, priorStats.Failed)
					require.Positive(t, priorStats.Synced)
					require.NoError(t, database.AdoptMachineIdentity(ctx, device, []string{machine}))
				}
				engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {discovery}}, ProviderFactories: []parser.ProviderFactory{factory}, Machine: machine, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
				stats := engine.SyncAll(ctx, nil)
				engine.Close()
				require.Zero(t, stats.Failed)
				require.Positive(t, stats.Synced)
				before, err := database.GetSessionFull(ctx, nativeID)
				require.NoError(t, err)
				require.NotNil(t, before)
				require.Equal(t, machine, before.Machine)
				require.NotNil(t, before.FilePath)
				require.Equal(t, selectedPath, *before.FilePath)
				require.NoError(t, database.Close())
				roots := []rawarchive.RootSpec{{Provider: string(agent), Path: selected}}
				if scenario == "same ID different roots" {
					roots = append(roots, rawarchive.RootSpec{Provider: string(agent), Path: first})
				} else if scenario == "missing captured path" {
					roots = []rawarchive.RootSpec{{Provider: string(agent), Path: first}}
				}
				seed := captureAndSeedRetainedFixture(t, root, data, roots)
				reader, err := db.OpenReadOnly(ctx, filepath.Join(seed, "sessions.db"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Close() })
				session, err := reader.GetSessionFull(ctx, nativeID)
				require.NoError(t, err)
				require.NotNil(t, session)
				require.Equal(t, before.Machine, session.Machine)
				require.Equal(t, before.FilePath, session.FilePath)
				var count int
				require.NoError(t, reader.Reader().QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sessions`).Scan(&count))
				require.Zero(t, count)
				sources, err := reader.ListRawArchiveSources(ctx, "", 1000)
				require.NoError(t, err)
				var selectedFound, otherFound bool
				for _, source := range sources {
					selectedFound = selectedFound || source.OriginalPath == acceptedPath
					otherFound = otherFound || source.OriginalPath == firstPath
				}
				if scenario == "missing captured path" {
					require.False(t, selectedFound)
					require.True(t, otherFound)
				} else {
					require.True(t, selectedFound, "selected source must be independently accepted")
				}
				if scenario == "same ID different roots" {
					require.True(t, otherFound, "other same-ID source must be independently accepted")
				}
				beforeDB, beforeVault, beforeNames := seedArchiveState(t, seed)
				require.NoError(t, os.RemoveAll(selected))
				require.NoError(t, os.RemoveAll(first))
				destination := t.TempDir()
				t.Setenv("CLAUDE_CONFIG_DIR", destination)
				t.Setenv("CODEX_HOME", destination)
				cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{agent: {discovery}}}
				result, materializeErr := Materialize(ctx, cfg, reader, session, t.TempDir())
				require.NoError(t, reader.Reader().QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sessions`).Scan(&count))
				assert.Zero(t, count)
				afterDB, afterVault, afterNames := seedArchiveState(t, seed)
				assert.Equal(t, beforeDB, afterDB)
				assert.Equal(t, beforeVault, afterVault)
				assert.Equal(t, beforeNames, afterNames)
				if scenario == "missing captured path" || scenario == "unowned machine" {
					require.ErrorIs(t, materializeErr, errSourceUnavailable)
					entries, err := os.ReadDir(destination)
					require.NoError(t, err)
					assert.Empty(t, entries)
					return
				}
				require.NoError(t, materializeErr)
				assert.Contains(t, result.Source, "retained:")
				want := []string{original}
				if ancestor != "" {
					want = []string{ancestor, original}
				}
				require.Len(t, result.Paths, len(want))
				for i, expected := range want {
					got, err := os.ReadFile(result.Paths[i])
					require.NoError(t, err)
					assert.Equal(t, expected, string(got))
				}
			})
		}
	}
}
