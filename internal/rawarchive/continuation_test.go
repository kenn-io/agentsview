package rawarchive

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeContinuationArchiveReparse(t *testing.T) {
	for _, mode := range []string{"selected-first", "selected-second", "all", "seed", "foreign", "missing"} {
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
			companions := mode == "seed" || mode == "foreign"
			const output = "archived compiler output\nall checks passed\n"
			if companions {
				resultPath := filepath.Join(root, "project", "parent-b", "tool-results", "result.txt")
				dbtest.WriteTestFile(t, resultPath, []byte(output))
				dbtest.WriteTestFile(t, paths[0], []byte(testjsonl.NewSessionBuilder().
					AddClaudeUser("2026-08-05T03:40:00Z", "earlier question").
					AddClaudeAssistant("2026-08-05T03:41:00Z", "earlier answer").String()))
				continuationContent := testjsonl.NewSessionBuilder().
					AddClaudeUserWithUUID("2026-08-05T03:42:00Z", "start", "a", "").
					AddClaudeAssistantWithUUID("2026-08-05T03:42:01Z", "reply", "b", "a").
					AddClaudeUserWithUUID("2026-08-05T03:42:02Z", "one", "c", "b").
					AddClaudeAssistantWithUUID("2026-08-05T03:42:03Z", "one reply", "d", "c").
					AddClaudeUserWithUUID("2026-08-05T03:42:04Z", "two", "e", "d").
					AddClaudeAssistantWithUUID("2026-08-05T03:42:05Z", "two reply", "f", "e").
					AddClaudeUserWithUUID("2026-08-05T03:42:06Z", "three", "g", "f").
					AddClaudeAssistantWithUUID("2026-08-05T03:42:07Z", "three reply", "h", "g").
					AddClaudeUserWithUUID("2026-08-05T03:42:08Z", "four", "k", "h").
					AddClaudeAssistantWithUUID("2026-08-05T03:42:09Z", "four reply", "l", "k").String()
				notice := "<persisted-output>\nFull output saved to: " + resultPath + "\n</persisted-output>"
				continuationContent += strings.Join([]string{
					`{"type":"assistant","timestamp":"2026-08-05T03:42:10Z","uuid":"m","parentUuid":"l","message":{"content":[{"type":"tool_use","id":"toolu_archive","name":"Bash","input":{"command":"make"}}]}}`,
					`{"type":"user","timestamp":"2026-08-05T03:42:11Z","uuid":"n","parentUuid":"m","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_archive","content":` + strconv.Quote(notice) + `}]} ,"toolUseResult":{"persistedOutputPath":` + strconv.Quote(resultPath) + `,"persistedOutputSize":` + strconv.Itoa(len(output)) + `}}`,
				}, "\n") + "\n"
				continuationContent += testjsonl.NewSessionBuilder().
					AddClaudeUserWithUUID("2026-08-05T03:43:00Z", "fork question", "i", "b").
					AddClaudeAssistantWithUUID("2026-08-05T03:43:01Z", "fork answer", "j", "i").String()
				dbtest.WriteTestFile(t, paths[1], []byte(continuationContent))
			}
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
				if mode == "seed" {
					fork, err := database.GetSessionFull(ctx, "agent-reviewer-i")
					require.NoError(t, err)
					require.NotNil(t, fork)
					require.NotNil(t, fork.FilePath)
					require.Equal(t, paths[1], *fork.FilePath)
				}
				before, err := database.GetSessionFull(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, before)
				if !companions {
					require.Equal(t, 4, before.MessageCount)
				}
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
			if mode == "all" || mode == "seed" || mode == "foreign" {
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
				if companions {
					require.GreaterOrEqual(t, len(contents), 2)
					assert.Equal(t, []string{"earlier question", "earlier answer"}, contents[:2])
					var results []string
					for _, message := range messages {
						for _, call := range message.ToolCalls {
							results = append(results, call.ResultContent)
						}
					}
					assert.Equal(t, []string{output}, results)
					fork, err := database.GetSessionFull(ctx, id+"-i")
					require.NoError(t, err)
					require.NotNil(t, fork)
					require.NotNil(t, fork.ParentSessionID)
					assert.Equal(t, id, *fork.ParentSessionID)
					if mode == "seed" {
						require.NotNil(t, fork.FilePath)
						assert.Equal(t, paths[1], *fork.FilePath)
					}
					messages, err := database.GetAllMessages(ctx, id+"-i")
					require.NoError(t, err)
					require.Len(t, messages, 2)
					assert.Equal(t, "fork question", messages[0].Content)
					assert.Equal(t, "fork answer", messages[1].Content)
				} else {
					assert.Equal(t, []string{"A0", "A1", "B0", "B1"}, contents)
				}
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
