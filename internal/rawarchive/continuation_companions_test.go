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

func TestClaudeContinuationArchiveRetainsForkAndToolResult(t *testing.T) {
	for _, mode := range []string{"seed", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			foreign := mode == "foreign"
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			device, prefix := owner, ""
			if foreign {
				device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				prefix = device + "~"
			}
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			first := filepath.Join(root, "project", "parent-a", "subagents", "agent-reviewer.jsonl")
			second := filepath.Join(root, "project", "parent-b", "subagents", "agent-reviewer.jsonl")
			resultPath := filepath.Join(root, "project", "parent-b", "tool-results", "result.txt")
			const output = "archived compiler output\nall checks passed\n"
			dbtest.WriteTestFile(t, resultPath, []byte(output))
			dbtest.WriteTestFile(t, first, []byte(testjsonl.NewSessionBuilder().
				AddClaudeUser("2026-08-05T03:40:00Z", "earlier question").
				AddClaudeAssistant("2026-08-05T03:41:00Z", "earlier answer").String()))
			secondContent := testjsonl.NewSessionBuilder().
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
			secondContent += strings.Join([]string{
				`{"type":"assistant","timestamp":"2026-08-05T03:42:10Z","uuid":"m","parentUuid":"l","message":{"content":[{"type":"tool_use","id":"toolu_archive","name":"Bash","input":{"command":"make"}}]}}`,
				`{"type":"user","timestamp":"2026-08-05T03:42:11Z","uuid":"n","parentUuid":"m","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_archive","content":` + strconv.Quote(notice) + `}]} ,"toolUseResult":{"persistedOutputPath":` + strconv.Quote(resultPath) + `,"persistedOutputSize":` + strconv.Itoa(len(output)) + `}}`,
			}, "\n") + "\n"
			secondContent += testjsonl.NewSessionBuilder().
				AddClaudeUserWithUUID("2026-08-05T03:43:00Z", "fork question", "i", "b").
				AddClaudeAssistantWithUUID("2026-08-05T03:43:01Z", "fork answer", "j", "i").String()
			dbtest.WriteTestFile(t, second, []byte(secondContent))
			if !foreign {
				engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: owner, Ephemeral: true, DisableFilesystemProjectDiscovery: true})
				require.NoError(t, engine.SyncPathsContext(ctx, []string{first}))
				engine.Close()
				fork, err := database.GetSessionFull(ctx, "agent-reviewer-i")
				require.NoError(t, err)
				require.NotNil(t, fork)
				require.NotNil(t, fork.FilePath)
				require.Equal(t, second, *fork.FilePath)
			}
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			capture := newImportCapture(t, device, "source", RootSpec{Provider: "claude", Path: root})
			report, err := archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			assert.Equal(t, 1, report.Sources)
			// All subsequent content must come from the vault, including the
			// tool result stored beside the second continuation's parent.
			require.NoError(t, os.RemoveAll(root))
			for range 2 {
				_, err := archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
				require.NoError(t, err)
				fork, err := database.GetSessionFull(ctx, prefix+"agent-reviewer-i")
				require.NoError(t, err)
				require.NotNil(t, fork)
				require.NotNil(t, fork.ParentSessionID)
				assert.Equal(t, prefix+"agent-reviewer", *fork.ParentSessionID)
				if !foreign {
					require.NotNil(t, fork.FilePath)
					assert.Equal(t, second, *fork.FilePath)
				}
				messages, err := database.GetAllMessages(ctx, prefix+"agent-reviewer-i")
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, "fork question", messages[0].Content)
				assert.Equal(t, "fork answer", messages[1].Content)
				messages, err = database.GetAllMessages(ctx, prefix+"agent-reviewer")
				require.NoError(t, err)
				require.GreaterOrEqual(t, len(messages), 2)
				assert.Equal(t, "earlier question", messages[0].Content)
				assert.Equal(t, "earlier answer", messages[1].Content)
				var results []string
				for _, message := range messages {
					for _, call := range message.ToolCalls {
						results = append(results, call.ResultContent)
					}
				}
				assert.Equal(t, []string{output}, results)
			}
		})
	}
}

func TestClaudeOverlappingArchiveSourcesDoNotPublishPartialHistory(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	for _, parent := range []string{"parent-a", "parent-b"} {
		path := filepath.Join(root, "project", parent, "subagents", "agent-reviewer.jsonl")
		dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-08-05T03:40:00Z", parent+" question").
			AddClaudeAssistant("2026-08-05T03:41:00Z", parent+" answer").String()))
	}
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capture := newImportCapture(t, owner, "source", RootSpec{Provider: "claude", Path: root})
	report, err := archive.Import(ctx, loadTestCapture(t, &capture))
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	assert.Equal(t, 2, report.Sources, "overlapping runs are not verified continuations")
	require.NoError(t, os.RemoveAll(root))
	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.ErrorContains(t, err, "another selected source")
	assert.Zero(t, report.Parsed)
	session, err := database.GetSessionFull(ctx, "agent-reviewer")
	require.NoError(t, err)
	assert.Nil(t, session, "neither competing transcript should be published")
}
