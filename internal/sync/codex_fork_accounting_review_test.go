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
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Historical events are deliberately independent of the current message's
// usage. Codex's native rollout parser emits message usage, not these events.
func TestCodexForkAccountingReviewResolvedHistoricalEvents(t *testing.T) {
	for _, provenance := range []bool{true, false} {
		for _, rebuild := range []bool{false, true} {
			t.Run("provenance="+strconv.FormatBool(provenance)+"/rebuild="+strconv.FormatBool(rebuild), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, 1, parser.AgentCodex)
				id := "codex:" + forkReviewChild
				body := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "child answer", forkReviewTime))
				require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
				require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
				if provenance {
					require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
					state, err := d.GetCodexForkState(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, state)
					require.False(t, state.PreserveHistory)
				} else {
					// A pre-provenance archive with the correct child identity.
					require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Machine: "local", Project: "project", FilePath: &cp, ParentSessionID: new("codex:" + forkReviewParent)}))
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Role: "assistant", Content: "child answer", Timestamp: forkReviewTime}}))
					require.NoError(t, d.SetSessionDataVersion(t.Context(), id, 17))
					state, err := d.GetCodexForkState(t.Context(), id)
					require.NoError(t, err)
					require.Nil(t, state)
				}
				require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{{SessionID: id, Source: "turn", Model: "gpt-5", InputTokens: 100, OutputTokens: 5, OccurredAt: "2026-09-01T09:00:00Z", DedupKey: "historical-only"}}))
				if rebuild {
					stats := e.ResyncAll(t.Context(), nil)
					require.True(t, stats.ArchiveRebuilt)
					require.Zero(t, stats.Failed)
				} else {
					require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
				}
				events, err := d.GetUsageEvents(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, events, 1, "a resolved source does not contain this independently archived usage")
				assert.Equal(t, "historical-only", events[0].DedupKey)
			})
		}
	}
}

func TestCodexForkAccountingReviewHistoricalAndMessageTotals(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			id := "codex:" + forkReviewChild
			body := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "new answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 7, 0))
			require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			// No MessageOrdinal and an earlier timestamp distinguish this from
			// the seven-token current message. It is additive accounting.
			require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{{SessionID: id, Source: "turn", Model: "gpt-5", InputTokens: 100, OutputTokens: 5, OccurredAt: "2026-09-01T09:00:00Z", DedupKey: "historical-only"}}))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
			events, err := d.GetUsageEvents(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, events, 1)
			messages, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Equal(t, 7, messages[0].OutputTokens)
			u, err := d.GetSessionUsage(t.Context(), id, true)
			require.NoError(t, err)
			require.NotNil(t, u)
			var breakdownOutput int
			for _, entry := range u.Breakdown {
				breakdownOutput += entry.OutputTokens
			}
			assert.Equal(t, 12, breakdownOutput, "the independent message and historical event both reach usage readers")
			assert.Equal(t, 12, u.TotalOutputTokens)
			assert.Equal(t, 100, u.PeakContextTokens)
		})
	}
}

func TestCodexForkAccountingReviewOnlyHistoricalUsageKeepsActivity(t *testing.T) {
	for _, withMessage := range []bool{false, true} {
		t.Run("withMessage="+strconv.FormatBool(withMessage), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, 1, parser.AgentCodex)
			id := "codex:" + forkReviewChild
			const later = "2026-09-01T11:00:00Z"
			body := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime))
			if withMessage {
				body += testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "answer", forkReviewTime))
			}
			require.NoError(t, os.WriteFile(cp, []byte(body+testjsonl.JoinJSONL(`{"timestamp":"2026-09-01T11:00:00Z","type":"event_msg","payload":{"type":"task_complete"}}`)), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			before, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Equal(t, new(later), before.EndedAt)
			require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{{SessionID: id, Source: "turn", Model: "gpt-5", OutputTokens: 5, OccurredAt: later, DedupKey: "historical-only"}}))
			require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
			events, err := d.GetUsageEvents(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, events, 1)
			state, err := d.GetCodexForkState(t.Context(), id)
			require.NoError(t, err)
			require.True(t, state.ParentKnown)
			after, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, after)
			require.NotNil(t, after.EndedAt)
			assert.Equal(t, later, *after.EndedAt)
		})
	}
}

func TestCodexForkAccountingReviewFractionalActivity(t *testing.T) {
	for _, tc := range []struct {
		name, oldStart, oldEnd, newStart, newEnd, wantStart, wantEnd string
	}{
		{"earliest", "2026-09-01T10:00:00.1Z", "2026-09-01T10:00:00.2Z", "2026-09-01T10:00:00.11Z", "2026-09-01T10:00:00.3Z", "2026-09-01T10:00:00.1Z", "2026-09-01T10:00:00.3Z"},
		{"latest", forkReviewTime, "2026-09-01T10:00:00.11Z", forkReviewTime, "2026-09-01T10:00:00.1Z", forkReviewTime, "2026-09-01T10:00:00.11Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, d, cp, _, _ := forkReviewEngine(t, 1, parser.AgentCodex)
			id := "codex:" + forkReviewChild
			oldBody := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", tc.oldStart), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tc.oldStart), testjsonl.CodexMsgJSON("assistant", "saved answer", tc.oldEnd))
			require.NoError(t, os.WriteFile(cp, []byte(oldBody), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			newBody := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", tc.newStart), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tc.newStart), testjsonl.CodexMsgJSON("assistant", "current answer", tc.newEnd))
			require.NoError(t, os.WriteFile(cp, []byte(newBody), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
			assert.Equal(t, []string{"saved answer", "current answer"}, messageContents(t, d, id))
			s, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, s)
			require.NotNil(t, s.StartedAt)
			require.NotNil(t, s.EndedAt)
			assert.Equal(t, tc.wantStart, *s.StartedAt)
			assert.Equal(t, tc.wantEnd, *s.EndedAt)
		})
	}
}

// This uses the production normalized artifact import boundary to establish
// that Codex events are accepted archive rows, without claiming that today's
// native Codex rollout parser originates them.
func TestCodexForkAccountingReviewImportedEventIngress(t *testing.T) {
	root := t.TempDir()
	cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+forkReviewChild+".jsonl")
	pp := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+forkReviewParent+".jsonl")
	d := openTestDB(t)
	e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "fixture", IDPrefix: "fixture~", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: 1})
	t.Cleanup(e.Close)
	id := "fixture~codex:" + forkReviewChild
	imported := db.ArtifactImportedSession{Origin: "fixture", GID: id, ManifestHash: strings.Repeat("0", 64), ImportedSessionID: id}
	result, err := d.ApplyArtifactImportedSession(t.Context(), imported, db.SessionBatchWrite{
		Session:         db.Session{ID: id, Agent: "codex", Machine: "fixture", Project: "project"},
		UsageEvents:     []db.UsageEvent{{SessionID: id, Source: "turn", Model: "gpt-5", InputTokens: 100, OutputTokens: 5, OccurredAt: forkReviewTime, DedupKey: "historical-only"}},
		ReplaceMessages: true, DataVersion: 17,
	})
	require.NoError(t, err)
	require.True(t, result.Written)
	before, err := d.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.NoError(t, os.WriteFile(cp, []byte(testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", forkReviewTime), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime))), 0o600))
	require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
	stats := e.SyncAllForceParse(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Zero(t, stats.Deferred)
	after, err := d.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	assert.Len(t, after, 1, "resolved refresh loses a successfully imported historical event")
}
