package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkImportedOrdinalUsageSurvivesResolvedShortening(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			root := t.TempDir()
			cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+forkReviewChild+".jsonl")
			pp := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+forkReviewParent+".jsonl")
			meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", forkReviewTime))
			turn := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime))
			full := meta + turn + testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("user", "unlinked old prompt", forkReviewTime),
				testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime),
				testjsonl.CodexFunctionCallOutputJSON("call_test", "archived result body", forkReviewTime),
			)
			require.NoError(t, os.WriteFile(cp, []byte(full), 0o600))
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}, Machine: "fixture"})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0], Machine: "fixture", ForceParse: true})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed.Results[0].Result, ingest.ContentOptions{})
			require.NoError(t, err)
			rows, err := ingest.FinalizeRows(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)
			require.Len(t, rows.Messages, 2)
			require.Len(t, rows.Messages[1].ToolCalls, 1)
			id := "fixture~codex:" + forkReviewChild
			rows.Session.ID, rows.Session.Machine = id, "fixture"
			rows.Session.ParentSessionID = new("fixture~codex:" + forkReviewParent)
			for i := range rows.Messages {
				rows.Messages[i].SessionID = id
				for j := range rows.Messages[i].ToolCalls {
					rows.Messages[i].ToolCalls[j].SessionID = id
				}
			}
			d := openTestDB(t)
			imported := db.ArtifactImportedSession{Origin: "fixture", GID: id, ManifestHash: strings.Repeat("b", 64), ImportedSessionID: id}
			result, err := d.ApplyArtifactImportedSession(t.Context(), imported, db.SessionBatchWrite{
				Session: rows.Session, Messages: rows.Messages,
				UsageEvents:     []db.UsageEvent{{SessionID: id, MessageOrdinal: new(1), Source: "turn", Model: "gpt-5", InputTokens: 100, OutputTokens: 5, OccurredAt: forkReviewTime, DedupKey: "imported-ordinal"}},
				ReplaceMessages: true, DataVersion: 17, SkipSignalUpdates: true,
			})
			require.NoError(t, err)
			require.True(t, result.Written)
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "fixture", IDPrefix: "fixture~", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
			t.Cleanup(e.Close)
			for range 2 {
				stats := e.SyncAllForceParse(t.Context(), nil)
				require.Zero(t, stats.Failed)
				require.Zero(t, stats.Deferred)
				state, err := d.GetCodexForkState(t.Context(), id)
				require.NoError(t, err)
				require.NotNil(t, state)
				require.True(t, state.ParentKnown)
				require.False(t, state.PreserveHistory)
				events, err := d.GetUsageEvents(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, events, 1)
				assert.Equal(t, new(1), events[0].MessageOrdinal)
			}
			require.NoError(t, os.WriteFile(cp, []byte(meta+turn+testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "replacement answer", forkReviewTime))), 0o600))
			for range 3 {
				stats := e.SyncAllForceParse(t.Context(), nil)
				require.Zero(t, stats.Failed)
				require.Zero(t, stats.Deferred)
				messages, err := d.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				events, err := d.GetUsageEvents(t.Context(), id)
				require.NoError(t, err)
				assert.Len(t, messages, 2, "retain the ordinal-linked message plus the current replacement")
				assert.Len(t, events, 1, "retain the accepted imported usage event")
				for _, message := range messages {
					assert.NotEqual(t, "unlinked old prompt", message.Content, "an unrelated omitted message remains authoritative")
				}
				if len(messages) == 2 {
					require.Len(t, messages[0].ToolCalls, 1)
					assert.Equal(t, "archived result body", messages[0].ToolCalls[0].ResultContent)
					require.Len(t, messages[0].ToolCalls[0].ResultEvents, 1)
					assert.Equal(t, "archived result body", messages[0].ToolCalls[0].ResultEvents[0].Content)
					assert.Equal(t, "replacement answer", messages[1].Content)
				}
				if len(events) == 1 {
					assert.Equal(t, new(0), events[0].MessageOrdinal)
					assert.Equal(t, "imported-ordinal", events[0].DedupKey)
					assert.Equal(t, 5, events[0].OutputTokens)
					usage, err := d.GetSessionUsage(t.Context(), id, true)
					require.NoError(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, 5, usage.TotalOutputTokens)
				}
			}
		})
	}
}
