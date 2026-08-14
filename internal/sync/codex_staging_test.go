package sync

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestCodexStreamingParseParityWithLegacy drives one Codex transcript
// through both the collecting parse (legacy full write) and the staging
// parse (scratch-backed write), then compares the stored message, tool
// call, and result-event projections field by field, plus findings and
// status-driven signals.
func TestCodexStreamingParseParityWithLegacy(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexMsgJSON("user", "run the suite", "2024-01-01T10:00:02Z"),
		testjsonl.CodexMsgJSON(
			"assistant", "running", "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_a", nil, "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a",
			"build passed AKIA7QHWN2DKR4FYPLJM",
			"2024-01-01T10:00:05Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:06Z", 120, 40, 80,
		),
		testjsonl.CodexMsgJSON("user", "again", "2024-01-01T10:00:07Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_b", nil, "2024-01-01T10:00:08Z",
		),
		// A status-carrying output keeps the failure classification
		// status-driven: the staged in-memory model omits event content
		// (content heuristics arrive with the streaming signal reducer),
		// so parity here pins the status-based path.
		`{"timestamp":"2024-01-01T10:00:09Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_b","status":"errored","output":[{"type":"input_text","text":"command not found"}]}}`,
	)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg := parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), parser.FindSourceRequest{
			FullSessionID: "codex:" + uuid,
		},
	)
	require.NoError(t, err)
	require.True(t, found)

	// Legacy collecting parse.
	legacySink := parser.NewCodexCollectingSink(0)
	legacySess, legacyMsgs, legacyCursor, legacyHash, legacyAnchor, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, legacySink)
	require.NoError(t, err)
	require.NotNil(t, legacySess)

	// Staging parse.
	stagedSink, err := newCodexStagingSink(map[string]bool{})
	require.NoError(t, err)
	defer func() { require.NoError(t, stagedSink.Close()) }()
	stagedSess, stagedMsgs, stagedCursor, stagedHash, stagedAnchor, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, stagedSink)
	require.NoError(t, err)
	require.NotNil(t, stagedSess)

	// Parser-level parity: metadata, cursor, hash state, anchor digest.
	assert.Equal(t, legacyCursor, stagedCursor)
	assert.Equal(t, legacyHash, stagedHash)
	assert.Equal(t, legacyAnchor, stagedAnchor)
	assert.Equal(t, legacySess.ID, stagedSess.ID)
	assert.Equal(t, legacySess.MessageCount, stagedSess.MessageCount)
	require.Len(t, stagedMsgs, len(legacyMsgs))
	for i := range legacyMsgs {
		assert.Equal(t, legacyMsgs[i].Role, stagedMsgs[i].Role)
		assert.Equal(t, legacyMsgs[i].Content, stagedMsgs[i].Content)
		assert.Equal(t, legacyMsgs[i].HasToolUse, stagedMsgs[i].HasToolUse)
		assert.Equal(t, legacyMsgs[i].ContextTokens,
			stagedMsgs[i].ContextTokens)
		require.Len(t, stagedMsgs[i].ToolCalls,
			len(legacyMsgs[i].ToolCalls))
		for j := range legacyMsgs[i].ToolCalls {
			lc, sc := legacyMsgs[i].ToolCalls[j],
				stagedMsgs[i].ToolCalls[j]
			assert.Equal(t, lc.ToolUseID, sc.ToolUseID)
			assert.Equal(t, lc.Category, sc.Category)
			require.Len(t, sc.ResultEvents, len(lc.ResultEvents))
			for k := range lc.ResultEvents {
				le, se := lc.ResultEvents[k], sc.ResultEvents[k]
				assert.Equal(t, le.AgentID, se.AgentID)
				assert.Equal(t, le.Status, se.Status)
				assert.Equal(t, le.Source, se.Source)
				assert.NotEqual(t, le.Content, se.Content,
					"staged events carry placeholders, never content")
			}
		}
	}

	// Database projections.
	dbLegacy := openTestDB(t)
	dbStaged := openTestDB(t)
	writeSessionRow := func(database *db.DB, sess *parser.ParsedSession) db.Session {
		var started *string
		if !sess.StartedAt.IsZero() {
			s := sess.StartedAt.Format(time.RFC3339Nano)
			started = &s
		}
		row := db.Session{
			ID:               sess.ID,
			Project:          sess.Project,
			Machine:          sess.Machine,
			Agent:            string(sess.Agent),
			FirstMessage:     &sess.FirstMessage,
			StartedAt:        started,
			MessageCount:     sess.MessageCount,
			UserMessageCount: sess.UserMessageCount,
			IsAutomated:      false,
		}
		require.NoError(t, database.UpsertSession(row))
		return row
	}
	rowL := writeSessionRow(dbLegacy, legacySess)
	rowS := writeSessionRow(dbStaged, stagedSess)

	legacyDBMsgs := toDBMessages(pendingWrite{
		sess: *legacySess, msgs: legacyMsgs,
	}, nil)
	stagedDBMsgs := toDBMessages(pendingWrite{
		sess: *stagedSess, msgs: stagedMsgs,
	}, nil)

	updateL, findingsL := computeSignalsAndSecrets(rowL, legacyDBMsgs)
	require.NoError(t, dbLegacy.ReplaceSessionContent(
		rowL.ID, legacyDBMsgs, updateL, findingsL,
	))

	updateS, findingsFromMsgs := computeSignalsAndSecrets(
		rowS, stagedDBMsgs,
	)
	positions := make(map[string]db.StagedToolCallPosition)
	for _, m := range stagedDBMsgs {
		for callIdx, tc := range m.ToolCalls {
			if tc.ToolUseID == "" {
				continue
			}
			positions[tc.ToolUseID] = db.StagedToolCallPosition{
				ToolUseID: tc.ToolUseID,
				Ordinal:   m.Ordinal,
				CallIndex: callIdx,
			}
		}
	}
	combinedFindings := append(
		append([]db.SecretFinding(nil), findingsFromMsgs...),
		stagedSink.Findings(rowS.ID, positions)...,
	)
	updateS.SecretLeakCount = definiteFindingCount(combinedFindings)
	require.NoError(t, dbStaged.ReplaceSessionContentStaged(
		rowS.ID, stagedDBMsgs, updateS, combinedFindings,
		stagedSink, map[string]bool{},
	))

	msgsL, err := dbLegacy.GetAllMessages(context.Background(), rowL.ID)
	require.NoError(t, err)
	msgsS, err := dbStaged.GetAllMessages(context.Background(), rowS.ID)
	require.NoError(t, err)
	require.Len(t, msgsS, len(msgsL))
	for i := range msgsL {
		require.Equal(t, msgsL[i].Ordinal, msgsS[i].Ordinal)
		assert.Equal(t, msgsL[i].Role, msgsS[i].Role)
		assert.Equal(t, msgsL[i].Content, msgsS[i].Content)
		assert.Equal(t, msgsL[i].HasToolUse, msgsS[i].HasToolUse)
		assert.Equal(t, msgsL[i].ContextTokens, msgsS[i].ContextTokens)
		assert.Equal(t, msgsL[i].OutputTokens, msgsS[i].OutputTokens)
		require.Len(t, msgsS[i].ToolCalls, len(msgsL[i].ToolCalls))
		for j := range msgsL[i].ToolCalls {
			lc, sc := msgsL[i].ToolCalls[j], msgsS[i].ToolCalls[j]
			assert.Equal(t, lc.ToolUseID, sc.ToolUseID)
			assert.Equal(t, lc.Category, sc.Category)
			assert.Equal(t, lc.InputJSON, sc.InputJSON)
			assert.Equal(t, lc.ResultContent, sc.ResultContent,
				"summaries must match byte for byte")
			assert.Equal(t, lc.ResultContentLength, sc.ResultContentLength)
			require.Len(t, sc.ResultEvents, len(lc.ResultEvents))
			for k := range lc.ResultEvents {
				le, se := lc.ResultEvents[k], sc.ResultEvents[k]
				assert.Equal(t, le.ToolUseID, se.ToolUseID)
				assert.Equal(t, le.AgentID, se.AgentID)
				assert.Equal(t, le.Source, se.Source)
				assert.Equal(t, le.Status, se.Status)
				assert.Equal(t, le.Content, se.Content)
				assert.Equal(t, le.ContentLength, se.ContentLength)
				assert.Equal(t, le.EventIndex, se.EventIndex)
			}
		}
	}

	// Findings parity.
	findingsStored, err := dbStaged.SessionSecretFindings(
		context.Background(), rowS.ID,
	)
	require.NoError(t, err)
	findingsLegacy, err := dbLegacy.SessionSecretFindings(
		context.Background(), rowL.ID,
	)
	require.NoError(t, err)
	sortFindings(findingsLegacy)
	sortFindings(findingsStored)
	require.Equal(t, len(findingsLegacy), len(findingsStored))
	for i := range findingsLegacy {
		assert.Equal(t, findingsLegacy[i].RuleName,
			findingsStored[i].RuleName)
		assert.Equal(t, findingsLegacy[i].LocationKind,
			findingsStored[i].LocationKind)
		assert.Equal(t, findingsLegacy[i].MessageOrdinal,
			findingsStored[i].MessageOrdinal)
		assert.Equal(t, findingsLegacy[i].CallIndex,
			findingsStored[i].CallIndex)
		assert.Equal(t, findingsLegacy[i].EventIndex,
			findingsStored[i].EventIndex)
		assert.Equal(t, findingsLegacy[i].MatchStart,
			findingsStored[i].MatchStart)
	}

	// Status-driven signals parity (the fixture's failures come from
	// event statuses, which the staged model keeps).
	sessL, err := dbLegacy.GetSessionFull(context.Background(), rowL.ID)
	require.NoError(t, err)
	sessS, err := dbStaged.GetSessionFull(context.Background(), rowS.ID)
	require.NoError(t, err)
	assert.Equal(t, sessL.ToolFailureSignalCount,
		sessS.ToolFailureSignalCount)
	assert.Equal(t, sessL.ConsecutiveFailureMax,
		sessS.ConsecutiveFailureMax)
	assert.Equal(t, sessL.FinalFailureStreak, sessS.FinalFailureStreak)
	assert.Equal(t, sessL.SecretLeakCount, sessS.SecretLeakCount)
}

func timePtr(s string) *string { return &s }

var _ = timePtr // retained for future helpers


func sortFindings(findings []db.SecretFinding) {
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.MessageOrdinal != b.MessageOrdinal {
			return a.MessageOrdinal < b.MessageOrdinal
		}
		if a.LocationKind != b.LocationKind {
			return a.LocationKind < b.LocationKind
		}
		if a.MatchStart != b.MatchStart {
			return a.MatchStart < b.MatchStart
		}
		if a.MatchIndex != b.MatchIndex {
			return a.MatchIndex < b.MatchIndex
		}
		return a.RuleName < b.RuleName
	})
}
