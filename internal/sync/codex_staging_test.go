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
// status- and content-driven signals.
func TestCodexStreamingParseParityWithLegacy(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	assertCodexStagedParity(t, uuid, codexParityTranscript(uuid))

	// The base fixture pins the absolute classification: call_b fails by
	// status and call_c by content heuristics, so both paths must see
	// exactly those two failures.
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(codexParityTranscript(uuid)), 0o644,
	))
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	sess, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, sess.ToolFailureSignalCount)
	require.Equal(t, 2, sess.ConsecutiveFailureMax)
	require.Equal(t, 2, sess.FinalFailureStreak)
	require.Equal(t, 1, sess.SecretLeakCount)
}

// codexParityTranscript is the deterministic dual-path fixture: a session
// meta, one token-counted turn with a secret-bearing output, a
// status-errored output, and a status-less content-failure output.
// writeCodexParityRoot writes the dual-path fixture into a fresh codex
// session root and returns the root path.
func writeCodexParityRoot(t *testing.T, uuid string) string {
	t.Helper()
	return writeCodexTranscriptRoot(t, uuid, codexParityTranscript(uuid))
}

func writeCodexTranscriptRoot(t *testing.T, uuid, transcript string) string {
	t.Helper()
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(transcript), 0o644,
	))
	return root
}

// syncCodexParityEngine builds an engine over the fixture root with the
// given staged threshold and runs one cold sync.
func syncCodexParityEngine(
	t *testing.T, database *db.DB, root string, stagedMin int64,
) {
	t.Helper()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine:                  "local",
		StagedCodexParseMinBytes: stagedMin,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
}

func codexParityTranscript(uuid string) string {
	return testjsonl.JoinJSONL(
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
		// A status-less output whose failure lives only in the content
		// heuristics: the streaming reducer must classify it identically.
		testjsonl.CodexMsgJSON("user", "third", "2024-01-01T10:00:10Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_c", nil, "2024-01-01T10:00:11Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_c",
			"bash: python3: command not found",
			"2024-01-01T10:00:12Z",
		),
	)
}

// TestCodexEngineStagedSyncParity syncs the same transcript through two
// real engines — one on the collecting path, one on the staged streaming
// path (threshold lowered to a byte) — and asserts the stored projections
// match exactly. This is the default-CI coverage for the staged wiring
// that the macro gates cannot provide.
func TestCodexEngineStagedSyncParity(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	legacyDB := openTestDB(t)
	stagedDB := openTestDB(t)
	syncCodexParityEngine(
		t, legacyDB, writeCodexParityRoot(t, uuid), 0,
	)
	syncCodexParityEngine(
		t, stagedDB, writeCodexParityRoot(t, uuid), 1,
	)
	sessionID := "codex:" + uuid

	msgsL, err := legacyDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	msgsS, err := stagedDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, msgsL, msgsS,
		"staged engine sync must match the collecting projection")

	sessL, err := legacyDB.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	sessS, err := stagedDB.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessL.ToolFailureSignalCount,
		sessS.ToolFailureSignalCount)
	require.Equal(t, sessL.ConsecutiveFailureMax,
		sessS.ConsecutiveFailureMax)
	require.Equal(t, sessL.FinalFailureStreak, sessS.FinalFailureStreak)
	require.Equal(t, sessL.SecretLeakCount, sessS.SecretLeakCount)

	findingsL, err := legacyDB.SessionSecretFindings(
		t.Context(), sessionID,
	)
	require.NoError(t, err)
	findingsS, err := stagedDB.SessionSecretFindings(
		t.Context(), sessionID,
	)
	require.NoError(t, err)
	sortFindings(findingsL)
	sortFindings(findingsS)
	require.Equal(t, len(findingsL), len(findingsS))
	for i := range findingsL {
		require.Equal(t, findingsL[i].RuleName, findingsS[i].RuleName)
		require.Equal(t, findingsL[i].MessageOrdinal,
			findingsS[i].MessageOrdinal)
		require.Equal(t, findingsL[i].EventIndex, findingsS[i].EventIndex)
	}
}

// TestCodexEngineStagedSanitizesToolResultContent protects the central
// persistence contract at the staged boundary. The expected string is a
// literal oracle: NUL, ESC, and C1 controls are removed while printable bytes
// remain, in both the event row and its denormalized tool-call summary.
func TestCodexEngineStagedSanitizesToolResultContent(t *testing.T) {
	const (
		uuid       = "019eb791-cf7d-75c1-8439-9ed74c122b11"
		rawOutput  = "before\x00after\x1b[31mred\u0085done"
		wantOutput = "beforeafter[31mreddone"
	)
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "run it", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_a", nil, "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", rawOutput, "2024-01-01T10:00:03Z",
		),
		// Exact raw duplicates collapse before sanitization, while an event
		// differing only by stripped controls remains distinct. This pins the
		// collecting path's existing identity contract at the staged boundary.
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", rawOutput, "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", wantOutput, "2024-01-01T10:00:05Z",
		),
	)

	legacyDB := openTestDB(t)
	stagedDB := openTestDB(t)
	syncCodexParityEngine(
		t, legacyDB, writeCodexTranscriptRoot(t, uuid, transcript), 0,
	)
	syncCodexParityEngine(
		t, stagedDB, writeCodexTranscriptRoot(t, uuid, transcript), 1,
	)

	assertStored := func(t *testing.T, database *db.DB) []db.Message {
		t.Helper()
		msgs, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
		require.NoError(t, err)
		var calls []db.ToolCall
		for _, msg := range msgs {
			calls = append(calls, msg.ToolCalls...)
		}
		require.Len(t, calls, 1)
		assert.Equal(t, wantOutput, calls[0].ResultContent)
		assert.Equal(t, len(wantOutput), calls[0].ResultContentLength)
		require.Len(t, calls[0].ResultEvents, 2)
		for _, event := range calls[0].ResultEvents {
			assert.Equal(t, wantOutput, event.Content)
			assert.Equal(t, len(wantOutput), event.ContentLength)
		}
		return msgs
	}

	legacyMsgs := assertStored(t, legacyDB)
	stagedMsgs := assertStored(t, stagedDB)
	assert.Equal(t, legacyMsgs, stagedMsgs)
}

// TestCodexEngineResyncBulkStagedParity pins the bulk-write blocker: a
// full rebuild (ResyncAll) with the staged streaming path active must
// publish real tool outputs, never the staged placeholders, and must
// match the plain cold sync projection.
func TestCodexEngineResyncBulkStagedParity(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	sessionID := "codex:" + uuid

	legacyDB := openTestDB(t)
	syncCodexParityEngine(
		t, legacyDB, writeCodexParityRoot(t, uuid), 0,
	)

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {writeCodexParityRoot(t, uuid)},
		},
		Machine:                  "local",
		StagedCodexParseMinBytes: 1,
	})
	t.Cleanup(engine.Close)
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "resync aborted: %v", stats.Warnings)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)

	msgsL, err := legacyDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	msgsS, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, msgsL, msgsS,
		"bulk resync must publish real tool outputs, not placeholders")
	for _, m := range msgsS {
		for _, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				require.NotContains(t, ev.Content, "staged:",
					"bulk resync wrote a staging placeholder")
			}
		}
	}
}

// assertCodexStagedParity runs one transcript through the collecting and
// staging parse paths and asserts the parser projections and stored DB
// projections agree field by field. It is shared by the deterministic
// parity test and the perturbation table so every variant gets the full
// dual-path comparison.
func assertCodexStagedParity(t *testing.T, uuid, content string) {
	t.Helper()
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
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
	legacySess, legacyMsgs, legacyCursor, legacyHash, legacyAnchor, legacyRetry, err :=
		parser.ParseCodexSessionStreaming(cfg, source, legacySink)
	require.NoError(t, err)
	require.NotNil(t, legacySess)

	// Staging parse.
	stagedSink, err := newCodexStagingSink("", map[string]bool{})
	require.NoError(t, err)
	defer func() { require.NoError(t, stagedSink.Close()) }()
	stagedSess, stagedMsgs, stagedCursor, stagedHash, stagedAnchor, stagedRetry, err :=
		parser.ParseCodexSessionStreaming(cfg, source, stagedSink)
	require.NoError(t, err)
	require.NotNil(t, stagedSess)

	// Parser-level parity: metadata, cursor, hash state, anchor digest.
	assert.Equal(t, legacyCursor, stagedCursor)
	assert.Equal(t, legacyHash, stagedHash)
	assert.Equal(t, legacyAnchor, stagedAnchor)
	assert.Equal(t, legacyRetry, stagedRetry)
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
	// The publish transaction resolves each summary once and runs this
	// closure before commit, so the content-failure-aware signals and
	// findings persist atomically with the rows they describe.
	require.NoError(t, dbStaged.ReplaceSessionContentStaged(
		context.Background(), rowS.ID, stagedDBMsgs, stagedSink,
		map[string]bool{},
		func(verdicts map[string]bool) (
			db.SessionSignalUpdate, []db.SecretFinding, error,
		) {
			update, findings :=
				computeSignalsAndSecretsWithContentFailures(
					rowS, stagedDBMsgs, verdicts,
				)
			combined := append(
				append([]db.SecretFinding(nil), findings...),
				stagedSink.Findings(rowS.ID, positions)...,
			)
			update.SecretLeakCount = definiteFindingCount(combined)
			return update, combined, nil
		},
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

	// Signals parity: call_b fails by event status (kept in the staged
	// model) and call_c by content heuristics (folded in through the
	// streaming reducer), so both classification paths must agree.
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

// TestCodexStagedBlockedCategorySignalParity pins the blocked-category
// parity: a blocked Bash call whose status-less output contains a failure
// marker must not be classified as a content failure. The legacy path
// blanks the summary before computing signals, so the staged summary
// resolver must evaluate its verdict against empty content too.
func TestCodexStagedBlockedCategorySignalParity(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b09"
	blocked := map[string]bool{"Bash": true}
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "run it", "2024-01-01T10:00:01Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_a", nil, "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", "command not found", "2024-01-01T10:00:03Z",
		),
	)

	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), parser.FindSourceRequest{
			FullSessionID: "codex:" + uuid,
		},
	)
	require.NoError(t, err)
	require.True(t, found)

	// Legacy collecting parse: the blocked map blanks Bash content, so the
	// status-less "command not found" output is not a content failure.
	legacySink := parser.NewCodexCollectingSink(0)
	legacySess, legacyMsgs, _, _, _, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, legacySink)
	require.NoError(t, err)
	require.NotNil(t, legacySess)
	legacyDBMsgs := toDBMessages(pendingWrite{
		sess: *legacySess, msgs: legacyMsgs,
	}, blocked)
	legacyUpdate, _ := computeSignalsAndSecrets(
		db.Session{ID: legacySess.ID}, legacyDBMsgs,
	)

	// Staging parse with the same blocked map; resolving summaries records
	// the per-call content-failure verdicts the publish transaction folds.
	stagedSink, err := newCodexStagingSink("", blocked)
	require.NoError(t, err)
	defer func() { require.NoError(t, stagedSink.Close()) }()
	stagedSess, stagedMsgs, _, _, _, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, stagedSink)
	require.NoError(t, err)
	require.NotNil(t, stagedSess)
	stagedDBMsgs := toDBMessages(pendingWrite{
		sess: *stagedSess, msgs: stagedMsgs,
	}, blocked)
	for _, m := range stagedDBMsgs {
		for _, tc := range m.ToolCalls {
			if tc.ToolUseID == "" {
				continue
			}
			_, _, err := stagedSink.ResolveSummary(
				context.Background(), tc.ToolUseID,
			)
			require.NoError(t, err)
		}
	}
	stagedUpdate, _ := computeSignalsAndSecretsWithContentFailures(
		db.Session{ID: stagedSess.ID}, stagedDBMsgs,
		stagedSink.ContentFailures(),
	)

	assert.Equal(t, legacyUpdate.ToolFailureSignalCount,
		stagedUpdate.ToolFailureSignalCount,
		"blocked-category failure signals must match the legacy path")
	assert.Zero(t, legacyUpdate.ToolFailureSignalCount,
		"a blocked output with a failure marker must never be a content failure")
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
