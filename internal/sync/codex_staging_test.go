package sync

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDrainResultsReleasesStagedScratchAndGCGuard(t *testing.T) {
	dir := t.TempDir()
	sink, err := newCodexStagingSink(dir, nil)
	require.NoError(t, err)

	// SetGCPercent(-1) disables GC rather than querying it on this
	// toolchain, so drive the restore check with an explicit baseline and
	// read each transition from SetGCPercent's return value.
	debug.SetGCPercent(200)
	t.Cleanup(func() { debug.SetGCPercent(100) })
	guard := beginStagedColdSync()
	t.Cleanup(guard)
	require.Equal(t, stagedColdSyncGCPercent,
		debug.SetGCPercent(stagedColdSyncGCPercent),
		"the staged parse must lower the GC target")

	results := make(chan syncJob, 1)
	results <- syncJob{processResult: processResult{
		staged:          sink,
		stagedGCRelease: guard,
	}}
	drainResults(results, 1)

	require.Equal(t, 200, debug.SetGCPercent(200),
		"draining must restore the process GC target")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"draining must remove the staged scratch file")
}

func TestCodexStagingBlockedContentNeverEntersScratch(t *testing.T) {
	dir := t.TempDir()
	sink, err := newCodexStagingSink(dir, map[string]bool{"Bash": true})
	require.NoError(t, err)
	defer func() { require.NoError(t, sink.Close()) }()

	sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
		ToolUseID: "call_secret",
		ToolName:  "exec_command",
		Category:  "Bash",
	}}})
	const secret = "AKIA7QHWN2DKR4FYPLJM blocked payload"
	sink.AppendToolResultEvent("call_secret", nil, parser.ParsedToolResultEvent{
		ToolUseID: "call_secret",
		Source:    "function_call_output",
		Content:   secret,
	})
	require.NoError(t, sink.Err())

	var content string
	var length, blanked int
	require.NoError(t, sink.scratch.QueryRow(
		`SELECT content, content_length, blanked FROM stage_events LIMIT 1`,
	).Scan(&content, &length, &blanked))
	require.Empty(t, content)
	require.Equal(t, len(secret), length)
	require.Equal(t, 1, blanked)

	raw, err := os.ReadFile(sink.path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), secret,
		"blocked raw content must not be recoverable from scratch storage")
}

// TestCodexStagingDropsOrphanResultEvents pins the staged sink's
// bounded-memory contract for tool-result events whose call never
// registered a message-model position. parser.ParseResult carries no
// ToolCallUpdates field, so every full-parse consumer discards such
// events; the staged sink must not retain their content on the way to
// being discarded, since that content is an uncloned reference into the
// source line's backing buffer and can be arbitrarily large.
func TestCodexStagingDropsOrphanResultEvents(t *testing.T) {
	dir := t.TempDir()
	sink, err := newCodexStagingSink(dir, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, sink.Close()) }()

	large := strings.Repeat("orphan output ", 1000)
	sink.AppendToolResultEvent("call_never_registered", nil,
		parser.ParsedToolResultEvent{
			ToolUseID: "call_never_registered",
			Source:    "function_call_output",
			Content:   large,
		},
	)
	require.NoError(t, sink.Err())

	assert.Empty(t, sink.ToolCallUpdates(),
		"an orphan event must not be retained in the collecting "+
			"sink's toolCallUpdates, which no full-parse consumer reads")

	var rowCount int
	require.NoError(t, sink.scratch.QueryRow(
		`SELECT COUNT(*) FROM stage_events`,
	).Scan(&rowCount))
	assert.Zero(t, rowCount, "an orphan event must not be staged either")
}

func TestStagedCodexParseOutcomeCopiesFingerprintHash(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b04"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp", "user", "2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hi", "2024-01-01T10:00:01Z"),
	)), 0o644))

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

	staged, err := newCodexStagingSink("", map[string]bool{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = staged.Close() })

	outcome, err := stagedCodexParseOutcome(
		cfg, source,
		parser.SourceFingerprint{Hash: "sha256:abc"},
		staged,
	)
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "sha256:abc",
		outcome.Results[0].Result.Session.File.Hash,
		"the staged path must persist the fingerprint hash the collecting path would")
}

func TestSyncSingleSessionStagedPublishesRealContent(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
	root := writeCodexParityRoot(t, uuid)
	stagingDir := t.TempDir()
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine:                  "local",
		StagedCodexParseMinBytes: 1,
		CodexStagingDir:          stagingDir,
	})
	t.Cleanup(engine.Close)
	sessionID := "codex:" + uuid

	require.NoError(t, engine.SyncSingleSession(sessionID))
	msgs, err := database.GetAllMessages(
		context.Background(), sessionID,
	)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	found := false
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ToolUseID != "call_a" || len(tc.ResultEvents) == 0 {
				continue
			}
			found = true
			require.NotContains(t, tc.ResultEvents[0].Content, "staged:",
				"the single-session path must publish real output, "+
					"not staged placeholders")
			require.Contains(t, tc.ResultEvents[0].Content,
				"build passed")
		}
	}
	require.True(t, found, "call_a output must be stored")

	entries, err := os.ReadDir(stagingDir)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"the single-session path must release its scratch file")
}

func TestParseDiffLargeCodexDoesNotStagePlaceholders(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c12"
	root := writeCodexParityRoot(t, uuid)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine:                  "local",
		StagedCodexParseMinBytes: 1,
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	diff := NewDiffEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(diff.Close)
	report, err := diff.ParseDiff(t.Context(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})
	require.NoError(t, err)
	require.Zero(t, report.Totals.Changed,
		"parse-diff must compare real content, not staged placeholders")
}

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

// TestCodexEngineStagedSubagentEventIDPrefix pins the staged publish path's
// event-level id-prefix contract for a remote (IDPrefix-configured) sync.
// The collecting path prefixes a subagent result event's SubagentSessionID
// in memory through applyRemoteRewrites before the write; the staged path
// inserts straight from scratch storage and must apply the same prefix at
// publish time, or a large remote Codex import would persist an unprefixed
// native id that cannot resolve to the session it actually belongs to.
func TestCodexEngineStagedSubagentEventIDPrefix(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b13"
	transcript := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "wait for the subagent", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"wait", "call_wait", map[string]any{
				"ids": []string{"agent-1"},
			}, "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_wait", map[string]any{
				"status": map[string]any{
					"agent-1": map[string]any{
						"completed": "subagent finished",
					},
				},
			}, "2024-01-01T10:00:03Z",
		),
	)

	syncPrefixed := func(t *testing.T, stagedMin int64) *db.DB {
		t.Helper()
		database := openTestDB(t)
		engine := NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodex: {writeCodexTranscriptRoot(t, uuid, transcript)},
			},
			Machine:                  "remote",
			IDPrefix:                 "remote-host~",
			StagedCodexParseMinBytes: stagedMin,
		})
		t.Cleanup(engine.Close)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		require.Equal(t, 1, stats.Synced)
		return database
	}

	legacyDB := syncPrefixed(t, 0)
	stagedDB := syncPrefixed(t, 1)
	sessionID := "remote-host~codex:" + uuid

	assertPrefixed := func(t *testing.T, database *db.DB) db.ToolResultEvent {
		t.Helper()
		msgs, err := database.GetAllMessages(t.Context(), sessionID)
		require.NoError(t, err)
		var events []db.ToolResultEvent
		for _, msg := range msgs {
			for _, call := range msg.ToolCalls {
				events = append(events, call.ResultEvents...)
			}
		}
		require.Len(t, events, 1)
		return events[0]
	}

	legacyEvent := assertPrefixed(t, legacyDB)
	stagedEvent := assertPrefixed(t, stagedDB)
	const wantSubagentID = "remote-host~codex:agent-1"
	assert.Equal(t, wantSubagentID, legacyEvent.SubagentSessionID)
	assert.Equal(t, wantSubagentID, stagedEvent.SubagentSessionID,
		"staged publish must prefix the event's subagent session id "+
			"the same way the collecting path's applyRemoteRewrites does")
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

	positions := stagedToolCallPositions(stagedDBMsgs)
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
