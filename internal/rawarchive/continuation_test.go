package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeContinuationArchiveReparse(t *testing.T) {
	for _, mode := range []string{"selected-first", "selected-second", "all", "foreign", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			paths := []string{filepath.Join(root, "project", "parent-a", "subagents", "agent-reviewer.jsonl"), filepath.Join(root, "project", "parent-b", "subagents", "workflows", "dispatch", "agent-reviewer.jsonl")}
			dbtest.WriteTestFile(t, paths[0], []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:40:00Z", "A0").AddClaudeAssistant("2026-08-05T03:41:00Z", "A1").String()))
			dbtest.WriteTestFile(t, paths[1], []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:42:00Z", "B0").AddClaudeAssistant("2026-08-05T03:43:00Z", "B1").String()))
			device, id := owner, "agent-reviewer"
			selected := paths[0]
			if mode == "selected-second" {
				selected = paths[1]
			}
			if mode == "foreign" {
				device = foreign
				id = foreign + "~" + id
			} else {
				engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: owner, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
				require.NoError(t, engine.SyncPathsContext(ctx, []string{selected}))
				engine.Close()
				before, err := database.GetSessionFull(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, before)
				require.Equal(t, 4, before.MessageCount)
				// Either member can be the seed's saved file path, depending on which
				// transcript was last refreshed on that machine.
				before.FilePath = new(selected)
				require.NoError(t, database.UpsertSession(ctx, *before))
			}
			if mode == "missing" {
				require.NoError(t, os.Remove(paths[1]))
			}
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			capture := newImportCapture(t, device, "source", RootSpec{Provider: "claude", Path: root})
			spec := loadTestCapture(t, &capture)
			report, err := archive.Import(ctx, spec)
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			sources, err := database.ListRawArchiveSources(ctx, "", 10)
			require.NoError(t, err)
			require.Len(t, sources, 1, "verified continuations are one logical source")
			// Reimport must elect the same source even after its session has been bound.
			report, err = archive.Import(ctx, spec)
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			require.NoError(t, os.RemoveAll(root))
			opts := ReparseOptions{ScratchBytes: 1 << 20}
			if mode == "all" || mode == "foreign" {
				opts.All = true
			} else {
				opts.ManifestIDs = []string{sources[0].ManifestID}
			}
			for range 2 {
				_, err = archive.Reparse(ctx, opts)
				if mode == "missing" {
					require.ErrorContains(t, err, "missing recorded Claude continuation")
				} else {
					require.NoError(t, err)
				}
				messages, err := database.GetAllMessages(ctx, id)
				require.NoError(t, err)
				contents := make([]string, len(messages))
				for i, m := range messages {
					contents[i] = m.Content
				}
				assert.Equal(t, []string{"A0", "A1", "B0", "B1"}, contents)
				session, err := database.GetSessionFull(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, session)
				assert.Equal(t, device, session.Machine)
				if mode != "foreign" {
					require.NotNil(t, session.FilePath)
					assert.Equal(t, selected, *session.FilePath)
				}
			}
		})
	}
}
