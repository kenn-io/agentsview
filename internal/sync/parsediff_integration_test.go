package sync_test

// Integration tests for the report-only parse-diff mode. They are
// written strictly against the exported contract in parsediff.go and
// parsediff_report.go: assertions go through DiffClass buckets, the
// Field* name constants, and ParseDiffTotals — never through rendered
// strings, whose exact wording belongs to the renderer.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// newParseDiffEngine builds a report-only diff engine over the same
// database and agent directories the setupTestEnv harness used for
// the initial SyncAll.
func newParseDiffEngine(env *testEnv) *sync.Engine {
	return sync.NewDiffEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude:         {env.claudeDir},
			parser.AgentCodex:          {env.codexDir},
			parser.AgentCursor:         {env.cursorDir},
			parser.AgentGemini:         {env.geminiDir},
			parser.AgentOpenCode:       {env.opencodeDir},
			parser.AgentForge:          {env.forgeDir},
			parser.AgentPiebald:        {env.piebaldDir},
			parser.AgentIflow:          {env.iflowDir},
			parser.AgentAmp:            {env.ampDir},
			parser.AgentPi:             {env.piDir},
			parser.AgentKiro:           {env.kiroDir},
			parser.AgentAntigravityCLI: {env.antigravityCLIDir},
		},
		Machine: "local",
	})
}

// runParseDiff runs ParseDiff with the given options and fails the
// test on error or a nil report.
func runParseDiff(
	t *testing.T, env *testEnv, opts sync.ParseDiffOptions,
) *sync.ParseDiffReport {
	t.Helper()
	report, err := newParseDiffEngine(env).ParseDiff(
		context.Background(), opts,
	)
	require.NoError(t, err, "ParseDiff")
	require.NotNil(t, report, "ParseDiff report")
	return report
}

// findSessionDiff returns the listed SessionDiff for the given
// session ID, or nil when the session is not listed.
func findSessionDiff(
	report *sync.ParseDiffReport, sessionID string,
) *sync.SessionDiff {
	for i := range report.Sessions {
		if report.Sessions[i].SessionID == sessionID {
			return &report.Sessions[i]
		}
	}
	return nil
}

// sessionDiffFieldNames collects the Field names attached to one
// session diff. Informational diffs are excluded unless
// includeInformational is set.
func sessionDiffFieldNames(
	sd *sync.SessionDiff, includeInformational bool,
) []string {
	var names []string
	for _, f := range sd.Fields {
		if f.Informational && !includeInformational {
			continue
		}
		names = append(names, f.Field)
	}
	return names
}

// mutateDB executes a single statement against the archive to
// simulate stored-row drift.
func mutateDB(
	t *testing.T, env *testEnv, query string, args ...any,
) {
	t.Helper()
	err := env.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
	require.NoError(t, err, "mutate db: %s", query)
}

// parseDiffClaudeContent builds a minimal two-message Claude session.
func parseDiffClaudeContent(prompt, reply string) string {
	return testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, prompt).
		AddClaudeAssistant(tsEarlyS5, reply).
		String()
}

// parseDiffClaudeContentRich builds a Claude session that exercises the
// thinking, tool_use/tool_result, and system-message paths, so the
// clean-archive acid test covers the message-flag and tool-call
// comparisons against real parsed data rather than empty fingerprints.
func parseDiffClaudeContentRich() string {
	return testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "run the build").
		AddRaw(testjsonl.ClaudeAssistantJSON(
			[]map[string]any{
				{"type": "thinking", "thinking": "I should run make build"},
				{"type": "text", "text": "Running the build now."},
				{
					"type":  "tool_use",
					"id":    "tu-1",
					"name":  "Bash",
					"input": map[string]any{"command": "make build"},
				},
			},
			tsEarlyS1,
		)).
		AddRaw(testjsonl.ClaudeToolResultUserJSON(
			"tu-1", "build succeeded", tsEarlyS5,
		)).
		AddClaudeMetaUser(tsEarlyS5, "system notice", true, false).
		String()
}

// parseDiffCodexContent builds a minimal Codex rollout session with
// the given session ID.
func parseDiffCodexContent(id string) string {
	return testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, id, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding coverage.").
		String()
}

// parseDiffGeminiContent builds a minimal two-message Gemini session.
func parseDiffGeminiContent(sessionID, hash string) string {
	return testjsonl.GeminiSessionJSON(
		sessionID, hash, tsEarly, tsEarlyS5,
		[]map[string]any{
			testjsonl.GeminiUserMsg("u1", tsEarly, "Explain this"),
			testjsonl.GeminiAssistantMsg(
				"a1", tsEarlyS5, "Here you go.", nil,
			),
		},
	)
}

// TestParseDiffCleanArchiveIsIdentical is the false-diff acid test:
// a freshly synced archive re-parsed by the same binary must come
// back identical on every session, with no field counts and no
// listed sessions.
func TestParseDiffCleanArchiveIsIdentical(t *testing.T) {
	env := setupTestEnv(t)

	// pd-alpha carries thinking, a tool_use/tool_result pair, and a
	// system message so the run exercises the message-flag and tool-call
	// comparisons, not just the summary fields.
	env.writeClaudeSession(t, "test-proj", "pd-alpha.jsonl",
		parseDiffClaudeContentRich())
	env.writeClaudeSession(t, "test-proj", "pd-beta.jsonl",
		parseDiffClaudeContent("beta prompt", "beta reply"))
	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-pd-codex.jsonl",
		parseDiffCodexContent("pd-codex"),
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 3, Synced: 3,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{})

	assert.Equal(t, db.CurrentDataVersion(), report.DataVersion,
		"report data version")
	assert.Equal(t, 3, report.FilesExamined, "files examined")
	assert.False(t, report.FilesLimited, "files limited")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 3, Identical: 3,
	}, report.Totals, "totals")
	assert.Empty(t, report.FieldCounts, "field counts")
	assert.Empty(t, report.Sessions,
		"identical sessions must not be listed")
	assert.False(t, report.HasFailures(), "HasFailures")
}

// TestParseDiffDetectsStoredDrift mutates stored rows directly after
// a sync and verifies each drifted session is classified DiffChanged
// with the expected field names while an untouched control session
// stays identical.
func TestParseDiffDetectsStoredDrift(t *testing.T) {
	env := setupTestEnv(t)

	ids := []string{
		"pd-count", "pd-first", "pd-model", "pd-role", "pd-time",
		"pd-body", "pd-swap", "pd-term", "pd-usage", "pd-control",
	}
	for _, id := range ids {
		env.writeClaudeSession(t, "test-proj", id+".jsonl",
			parseDiffClaudeContent(id+" prompt", id+" reply"))
	}
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 10, Synced: 10,
	})

	// Simulate drift between the stored rows and what the current
	// parser produces from the unchanged source files.
	mutateDB(t, env,
		"UPDATE sessions SET message_count = message_count + 5"+
			" WHERE id = ?", "pd-count")
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "pd-first")
	mutateDB(t, env,
		"UPDATE messages SET model = ? WHERE session_id = ?",
		"drifted-model", "pd-model")
	// Role-only and timestamp-only drift: neither moves the token or
	// content-length fingerprints, so the dedicated role/time
	// fingerprint is the only thing that can trigger the row-level
	// comparison. A regression here reports these sessions identical.
	mutateDB(t, env,
		"UPDATE messages SET role = 'assistant'"+
			" WHERE session_id = ? AND ordinal = 0", "pd-role")
	mutateDB(t, env,
		"UPDATE messages SET timestamp = ?"+
			" WHERE session_id = ? AND ordinal = 1",
		"2024-01-01T10:00:06Z", "pd-time")
	// Equal-length body rewrite: upper() changes the bytes without
	// moving content_length, so neither the length aggregates nor the
	// token fingerprint see it. Only the content hash can.
	mutateDB(t, env,
		"UPDATE messages SET content = upper(content)"+
			" WHERE session_id = ? AND ordinal = 0", "pd-body")
	// Aggregate collision: swapping content and content_length between
	// the two ordinals permutes the lengths, so sum/max/min are
	// unchanged while every per-ordinal value differs.
	swapMsgs := fetchMessages(t, env.db, "pd-swap")
	require.Len(t, swapMsgs, 2, "pd-swap fixture")
	require.NotEqual(t,
		swapMsgs[0].ContentLength, swapMsgs[1].ContentLength,
		"swap needs distinct lengths or the collision test is vacuous")
	mutateDB(t, env,
		"UPDATE messages SET content = ?, content_length = ?"+
			" WHERE session_id = ? AND ordinal = 0",
		swapMsgs[1].Content, swapMsgs[1].ContentLength, "pd-swap")
	mutateDB(t, env,
		"UPDATE messages SET content = ?, content_length = ?"+
			" WHERE session_id = ? AND ordinal = 1",
		swapMsgs[0].Content, swapMsgs[0].ContentLength, "pd-swap")
	// The parser classifies this fixture's termination; store a
	// different non-null value so the diff is real drift, not the
	// informational cleared-to-NULL case.
	mutateDB(t, env,
		"UPDATE sessions SET termination_status = ? WHERE id = ?",
		"truncated", "pd-term")
	// The Claude parser emits no usage events for this fixture, so
	// a synthetic stored event is pure drift.
	require.NoError(t, env.db.ReplaceSessionUsageEvents(
		"pd-usage", []db.UsageEvent{{
			SessionID: "pd-usage",
			Source:    "synthetic",
			Model:     "synthetic-model",
		}},
	), "insert synthetic usage event")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})

	assert.Equal(t, 10, report.FilesExamined, "files examined")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 10, Identical: 1, Changed: 9,
	}, report.Totals, "totals")

	cases := []struct {
		name      string
		sessionID string
		field     string
		// fieldCount is the expected FieldCounts entry; message_metadata
		// is shared by the role and timestamp drift sessions.
		fieldCount int
		// exact asserts the drifted field is the only
		// non-informational diff; otherwise presence suffices
		// (a synthetic usage event may also move totals).
		exact bool
	}{
		{"message count drift", "pd-count", sync.FieldMessageCount, 1, true},
		{"first message drift", "pd-first", sync.FieldFirstMessage, 1, true},
		{"model drift", "pd-model", sync.FieldModels, 1, true},
		{"role-only drift", "pd-role", sync.FieldMessageMetadata, 2, true},
		{"timestamp-only drift", "pd-time", sync.FieldMessageMetadata, 2, true},
		{"equal-length body drift", "pd-body", sync.FieldMessageContent, 2, true},
		{"aggregate-collision length swap", "pd-swap", sync.FieldMessageContent, 2, true},
		{"termination status drift", "pd-term", sync.FieldTerminationStatus, 1, true},
		{"usage event drift", "pd-usage", sync.FieldUsageEventCount, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := findSessionDiff(report, tc.sessionID)
			require.NotNil(t, sd, "session %q not listed", tc.sessionID)
			assert.Equal(t, sync.DiffChanged, sd.Class,
				"class for %q", tc.sessionID)
			got := sessionDiffFieldNames(sd, false)
			if tc.exact {
				assert.ElementsMatch(t, []string{tc.field}, got,
					"non-informational fields for %q", tc.sessionID)
			} else {
				assert.Contains(t, got, tc.field,
					"fields for %q", tc.sessionID)
			}
			assert.Equal(t, tc.fieldCount, report.FieldCounts[tc.field],
				"FieldCounts[%s]", tc.field)
		})
	}

	if sd := findSessionDiff(report, "pd-control"); sd != nil {
		assert.Equal(t, sync.DiffIdentical, sd.Class,
			"control session class")
		assert.Empty(t, sessionDiffFieldNames(sd, false),
			"control session non-informational fields")
	}
	assert.True(t, report.HasFailures(), "HasFailures with drift")
}

// TestParseDiffToleratesNullStoredTimestamp guards the NULL-timestamp
// regression. timestamp is the only nullable text column in messages, and
// a single imported row with a NULL timestamp once made the tier-1
// role/time fingerprint scan fail, aborting the whole run instead of
// producing a report. The run must complete and surface the now-empty
// stored timestamp as ordinary message_metadata drift.
func TestParseDiffToleratesNullStoredTimestamp(t *testing.T) {
	env := setupTestEnv(t)

	env.writeClaudeSession(t, "test-proj", "pd-nullts.jsonl",
		parseDiffClaudeContent("nullts prompt", "nullts reply"))
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Null the assistant message's stored timestamp. The source file is
	// unchanged, so the re-parse still yields a real timestamp there;
	// the stored NULL (coalesced to "") then differs from it as drift.
	mutateDB(t, env,
		"UPDATE messages SET timestamp = NULL"+
			" WHERE session_id = ? AND ordinal = 1", "pd-nullts")

	// runParseDiff requires no error and a non-nil report: before the
	// COALESCE guard the NULL row aborted the run inside the fingerprint.
	report := runParseDiff(t, env, sync.ParseDiffOptions{})

	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Changed: 1,
	}, report.Totals, "totals")

	sd := findSessionDiff(report, "pd-nullts")
	require.NotNil(t, sd, "session not listed")
	assert.Equal(t, sync.DiffChanged, sd.Class, "class")
	assert.ElementsMatch(t,
		[]string{sync.FieldMessageMetadata},
		sessionDiffFieldNames(sd, false),
		"non-informational fields")
	assert.True(t, report.HasFailures(), "HasFailures")

	// messageMetadataDiff compares role before timestamp, so the field
	// family alone does not prove timestamp was the trigger. Pin the
	// detail to the timestamp column to match this test's intent.
	var metaDetail string
	for _, f := range sd.Fields {
		if f.Field == sync.FieldMessageMetadata {
			metaDetail = f.Detail
		}
	}
	assert.Contains(t, metaDetail, "timestamp",
		"message_metadata drift should attribute to the timestamp column")
}

// TestParseDiffDetectsExtendedFieldDrift covers the comparator surface
// added beyond the summary fields end-to-end: tool_call drift, a
// per-message flag, a full-replace-agent session-metadata field, and the
// informational-for-incremental rule on a Claude session.
func TestParseDiffDetectsExtendedFieldDrift(t *testing.T) {
	env := setupTestEnv(t)

	// Two rich Claude sessions (thinking + tool_use/result + system),
	// one minimal Claude session, and a full-replace Gemini session.
	env.writeClaudeSession(t, "test-proj", "pd-ext-tool.jsonl",
		parseDiffClaudeContentRich())
	env.writeClaudeSession(t, "test-proj", "pd-ext-flag.jsonl",
		parseDiffClaudeContentRich())
	env.writeClaudeSession(t, "test-proj", "pd-ext-cwd.jsonl",
		parseDiffClaudeContent("cwd prompt", "cwd reply"))
	env.writeGeminiSession(t,
		filepath.Join("tmp", "exthash", "chats", "session-001.json"),
		parseDiffGeminiContent("pd-ext-branch", "exthash"))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 4, Synced: 4,
	})

	// Tool-call drift: rename a stored tool call. None of the message
	// token/role/content/flag fingerprints move, so this is caught only
	// via the tool-call fingerprint.
	mutateDB(t, env,
		"UPDATE tool_calls SET tool_name = ? WHERE session_id = ?",
		"DRIFTED", "pd-ext-tool")
	// Flag drift: flip has_thinking on the assistant message (the one
	// carrying the tool use). Only the flags fingerprint moves.
	mutateDB(t, env,
		"UPDATE messages SET has_thinking = NOT has_thinking"+
			" WHERE session_id = ? AND has_tool_use = 1", "pd-ext-flag")
	// Incremental-append session field on a Claude session: a real
	// difference, but classified informational, so the session stays
	// identical rather than changed.
	mutateDB(t, env,
		"UPDATE sessions SET cwd = ? WHERE id = ?",
		"/drifted/path", "pd-ext-cwd")
	// Full-replace-agent session metadata: Gemini does not take the
	// incremental path, so a git_branch difference is real drift.
	mutateDB(t, env,
		"UPDATE sessions SET git_branch = ? WHERE id = ?",
		"feature-x", "gemini:pd-ext-branch")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})

	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 4, Identical: 1, Changed: 3, InformationalOnly: 1,
	}, report.Totals, "totals")

	cases := []struct {
		name      string
		sessionID string
		field     string
	}{
		{"tool call drift", "pd-ext-tool", sync.FieldToolCalls},
		{"flag drift", "pd-ext-flag", sync.FieldMessageMetadata},
		{"git_branch drift", "gemini:pd-ext-branch", sync.FieldGitBranch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := findSessionDiff(report, tc.sessionID)
			require.NotNil(t, sd, "session %q not listed", tc.sessionID)
			assert.Equal(t, sync.DiffChanged, sd.Class,
				"class for %q", tc.sessionID)
			assert.ElementsMatch(t, []string{tc.field},
				sessionDiffFieldNames(sd, false),
				"non-informational fields for %q", tc.sessionID)
			assert.Equal(t, 1, report.FieldCounts[tc.field],
				"FieldCounts[%s]", tc.field)
		})
	}

	t.Run("incremental cwd drift is informational", func(t *testing.T) {
		sd := findSessionDiff(report, "pd-ext-cwd")
		require.NotNil(t, sd, "pd-ext-cwd not listed")
		assert.Equal(t, sync.DiffIdentical, sd.Class,
			"informational-only session stays identical")
		assert.Empty(t, sessionDiffFieldNames(sd, false),
			"no non-informational fields")
		assert.Contains(t, sessionDiffFieldNames(sd, true), sync.FieldCwd,
			"informational cwd diff must be attached")
		assert.Zero(t, report.FieldCounts[sync.FieldCwd],
			"informational diffs are excluded from FieldCounts")
	})

	assert.True(t, report.HasFailures(), "HasFailures with drift")
}

// TestParseDiffWritesNothing verifies the report-only promise: the
// stored drift is detected but not repaired, and nothing is persisted
// (no skip cache entries, no row rewrites).
func TestParseDiffWritesNothing(t *testing.T) {
	env := setupTestEnv(t)

	env.writeClaudeSession(t, "test-proj", "pd-keep.jsonl",
		parseDiffClaudeContent("keep prompt", "keep reply"))
	geminiPath := env.writeGeminiSession(
		t, filepath.Join("tmp", "pdhash", "chats", "session-001.json"),
		parseDiffGeminiContent("pd-err", "pdhash"),
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	// Corrupt the Gemini source so the diff run exercises the parse
	// error path, which a writing sync would record in skipped_files.
	dbtest.WriteTestFile(t, geminiPath, []byte("{corrupt"))

	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "pd-keep")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Changed: 1, ParseErrors: 1,
	}, report.Totals, "totals")

	// The drift must still be there: ParseDiff reports, never fixes.
	sess, err := env.db.GetSessionFull(
		context.Background(), "pd-keep",
	)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "session pd-keep not found")
	require.NotNil(t, sess.FirstMessage, "first_message is NULL")
	assert.Equal(t, "drifted first message", *sess.FirstMessage,
		"ParseDiff must not repair stored drift")

	// The corrupt session's archived rows are untouched.
	assertSessionMessageCount(t, env.db, "gemini:pd-err", 2)

	// Nothing was persisted: the parse error did not land in the
	// skip cache table.
	skipped, err := env.db.LoadSkippedFiles()
	require.NoError(t, err, "LoadSkippedFiles")
	assert.Empty(t, skipped, "skipped_files must stay empty")
}

// TestParseDiffBypassesSkipLayers proves ParseDiff re-parses every
// file even when the sync engine's size/mtime/skip-cache layers
// would skip it, by appending to a source file without re-syncing.
func TestParseDiffBypassesSkipLayers(t *testing.T) {
	env := setupTestEnv(t)

	path := env.writeClaudeSession(t, "test-proj", "pd-skip.jsonl",
		parseDiffClaudeContent("skip prompt", "skip reply"))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})
	// Second pass proves the skip layers are armed.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 0, Skipped: 1,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.FilesExamined,
		"skip layers must not hide files from ParseDiff")
	assert.Positive(t, report.Totals.Examined, "examined sessions")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1,
	}, report.Totals, "totals before append")

	// Append one more message without syncing. The incremental
	// append path would normally absorb this; a full re-parse must
	// surface it as a message count change instead.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open session file for append")
	_, err = f.WriteString(testjsonl.ClaudeAssistantJSON(
		[]map[string]any{{"type": "text", "text": "appended reply"}},
		"2024-01-01T10:00:10Z",
	) + "\n")
	require.NoError(t, err, "append message line")
	require.NoError(t, f.Close(), "close session file")

	report = runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, 1, report.FilesExamined, "files examined")
	assert.Equal(t, 1, report.Totals.Changed, "changed sessions")

	sd := findSessionDiff(report, "pd-skip")
	require.NotNil(t, sd, "session pd-skip not listed")
	assert.Equal(t, sync.DiffChanged, sd.Class, "class")
	assert.Contains(t, sessionDiffFieldNames(sd, false),
		sync.FieldMessageCount,
		"appended message must surface as a message_count diff")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldMessageCount],
		"FieldCounts[message_count]")
}

// TestParseDiffBuckets covers the non-compared classification
// buckets: skipped (source missing), new on disk, pending resync,
// and parse error.
func TestParseDiffBuckets(t *testing.T) {
	t.Run("source missing", func(t *testing.T) {
		env := setupTestEnv(t)
		path := env.writeClaudeSession(
			t, "test-proj", "pd-gone.jsonl",
			parseDiffClaudeContent("gone prompt", "gone reply"),
		)
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})
		require.NoError(t, os.Remove(path), "remove source file")

		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, 0, report.FilesExamined, "files examined")
		assert.Equal(t, sync.ParseDiffTotals{
			Skipped: 1,
		}, report.Totals, "totals")

		sd := findSessionDiff(report, "pd-gone")
		require.NotNil(t, sd, "session pd-gone not listed")
		assert.Equal(t, sync.DiffSkipped, sd.Class, "class")
		assert.NotEmpty(t, sd.Reason, "skip reason")
	})

	t.Run("new on disk", func(t *testing.T) {
		env := setupTestEnv(t)
		env.writeClaudeSession(t, "test-proj", "pd-base.jsonl",
			parseDiffClaudeContent("base prompt", "base reply"))
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})
		// Written after the sync: the archive is behind the disk.
		env.writeClaudeSession(t, "test-proj", "pd-new.jsonl",
			parseDiffClaudeContent("new prompt", "new reply"))

		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, 2, report.FilesExamined, "files examined")
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 1, Identical: 1, NewOnDisk: 1,
		}, report.Totals, "totals")

		sd := findSessionDiff(report, "pd-new")
		require.NotNil(t, sd, "session pd-new not listed")
		assert.Equal(t, sync.DiffNewOnDisk, sd.Class, "class")
	})

	t.Run("pending resync", func(t *testing.T) {
		env := setupTestEnv(t)
		env.writeClaudeSession(t, "test-proj", "pd-stale.jsonl",
			parseDiffClaudeContent("stale prompt", "stale reply"))
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})

		staleVersion := db.CurrentDataVersion() - 1
		require.NoError(t,
			env.db.SetSessionDataVersion("pd-stale", staleVersion),
			"downgrade data_version")
		// A real field diff that must NOT count as parser drift.
		mutateDB(t, env,
			"UPDATE sessions SET first_message = ? WHERE id = ?",
			"drifted first message", "pd-stale")

		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 1, PendingResync: 1,
		}, report.Totals, "totals")
		assert.Empty(t, report.FieldCounts,
			"pending_resync diffs must not be counted")

		sd := findSessionDiff(report, "pd-stale")
		require.NotNil(t, sd, "session pd-stale not listed")
		assert.Equal(t, sync.DiffPendingResync, sd.Class, "class")
		assert.Equal(t, staleVersion, sd.StoredDataVersion,
			"stored data version")
		// Field diffs are still attached for drill-down.
		assert.Contains(t, sessionDiffFieldNames(sd, true),
			sync.FieldFirstMessage,
			"pending_resync field diffs attached for drill-down")
		assert.False(t, report.HasFailures(),
			"pending_resync must not trip HasFailures")
	})

	t.Run("parse error", func(t *testing.T) {
		env := setupTestEnv(t)
		env.writeClaudeSession(t, "test-proj", "pd-ok.jsonl",
			parseDiffClaudeContent("ok prompt", "ok reply"))
		geminiPath := env.writeGeminiSession(
			t, filepath.Join(
				"tmp", "badhash", "chats", "session-001.json",
			),
			parseDiffGeminiContent("pd-bad", "badhash"),
		)
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 2, Synced: 2,
		})
		dbtest.WriteTestFile(t, geminiPath, []byte("{corrupt"))

		// The corrupt file must not abort the run.
		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 1, Identical: 1, ParseErrors: 1,
		}, report.Totals, "totals")

		sd := findSessionDiff(report, "gemini:pd-bad")
		require.NotNil(t, sd, "session gemini:pd-bad not listed")
		assert.Equal(t, sync.DiffParseError, sd.Class, "class")
		assert.True(t, report.HasFailures(),
			"parse errors must trip HasFailures")
	})
}

// TestParseDiffLimitNewestFirst verifies Limit samples files newest
// mtime first and reports the unexamined sessions as skipped.
func TestParseDiffLimitNewestFirst(t *testing.T) {
	env := setupTestEnv(t)

	base := time.Now()
	files := []struct {
		id    string
		mtime time.Time
	}{
		{"pd-oldest", base.Add(-2 * time.Hour)},
		{"pd-middle", base.Add(-1 * time.Hour)},
		{"pd-newest", base},
	}
	for _, f := range files {
		path := env.writeClaudeSession(
			t, "test-proj", f.id+".jsonl",
			parseDiffClaudeContent(f.id+" prompt", f.id+" reply"),
		)
		require.NoError(t, os.Chtimes(path, f.mtime, f.mtime),
			"chtimes %s", path)
	}
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 3, Synced: 3,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{Limit: 1})

	assert.Equal(t, 1, report.FilesExamined, "files examined")
	assert.True(t, report.FilesLimited, "files limited")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1, Skipped: 2,
	}, report.Totals, "totals")

	for _, id := range []string{"pd-oldest", "pd-middle"} {
		sd := findSessionDiff(report, id)
		require.NotNil(t, sd, "session %q not listed", id)
		assert.Equal(t, sync.DiffSkipped, sd.Class,
			"class for %q", id)
		assert.NotEmpty(t, sd.Reason, "skip reason for %q", id)
	}
	// The newest file was the one examined; it round-trips clean so
	// it is either unlisted or listed as identical.
	if sd := findSessionDiff(report, "pd-newest"); sd != nil {
		assert.Equal(t, sync.DiffIdentical, sd.Class,
			"newest session class")
	}
}

// TestParseDiffAgentScope verifies Agents restricts the run to the
// requested agents and that agents without an on-disk source to
// re-parse are rejected.
func TestParseDiffAgentScope(t *testing.T) {
	env := setupTestEnv(t)

	env.writeClaudeSession(t, "test-proj", "pd-claude.jsonl",
		parseDiffClaudeContent("claude prompt", "claude reply"))
	env.writeCodexSession(
		t, filepath.Join("2024", "01", "15"),
		"rollout-20240115-pd-codex.jsonl",
		parseDiffCodexContent("pd-codex"),
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	engine := newParseDiffEngine(env)
	report, err := engine.ParseDiff(
		context.Background(), sync.ParseDiffOptions{
			Agents: []parser.AgentType{parser.AgentCodex},
		},
	)
	require.NoError(t, err, "ParseDiff scoped to codex")
	require.NotNil(t, report, "ParseDiff report")

	assert.Equal(t, 1, report.FilesExamined,
		"claude files must not be counted in a codex-scoped run")
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1,
	}, report.Totals, "totals")
	for _, s := range report.Sessions {
		assert.NotEqual(t, "claude", s.Agent,
			"claude session listed in codex-scoped run: %+v", s)
		assert.NotEqual(t, "pd-claude", s.SessionID,
			"claude session listed in codex-scoped run: %+v", s)
	}
	assert.Equal(t, []string{"codex"}, report.Agents,
		"report.Agents must reflect the scoped run")

	// Database-backed agents have no on-disk source to re-parse.
	_, err = engine.ParseDiff(
		context.Background(), sync.ParseDiffOptions{
			Agents: []parser.AgentType{parser.AgentClaudeAI},
		},
	)
	require.Error(t, err,
		"ParseDiff must reject database-backed agents")
}

// TestParseDiffCoversKiroSQLite proves that Kiro's shared data.sqlite3
// store — which DiscoverFunc never emits and which normal sync reaches
// through a dedicated phase — is actually re-parsed by parse-diff. A
// regressed force-parse guard or missing synthesized discovery would
// surface here as the session being skipped/"not discovered" with
// Examined 0 rather than compared.
func TestParseDiffCoversKiroSQLite(t *testing.T) {
	env := setupTestEnv(t)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	ks.addSession(
		t, "/home/user/code/kiro-app", "sqlite-session",
		readKiroSQLiteFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentKiro},
	})
	// Examined:1/Identical:1 proves the data.sqlite3 session was
	// re-parsed and compared (not bucketed skipped/"not discovered").
	// Identical sessions are intentionally not listed, so a Skipped or
	// NewOnDisk count here would mean the synthesized discovery or the
	// force-parse guard regressed.
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1,
	}, report.Totals, "kiro sqlite session must be examined, not skipped")
	assert.Equal(t, 1, report.FilesExamined, "data.sqlite3 examined")
	assert.False(t, report.HasFailures(), "clean kiro sqlite run")
}

// TestParseDiffCoversMixedOpenCodeRoot proves a storage-mode OpenCode
// root that still carries DB-only legacy sessions in opencode.db gets
// BOTH sources re-parsed. Normal sync reads opencode.db regardless of
// source mode, so parse-diff must too; a mode-gated synthesized
// discovery would leave the legacy session "not discovered" and let
// --fail-on-change pass without vetting it.
func TestParseDiffCoversMixedOpenCodeRoot(t *testing.T) {
	env := setupTestEnv(t)

	// File-backed storage session: this makes ResolveOpenCodeSource
	// pick storage mode for the root.
	storage := createOpenCodeStorageFixture(t, env.opencodeDir)
	const storageID = "oc-mixed-storage"
	storage.addSession(
		t, "global", storageID,
		"/home/user/code/storage-app", "Mixed Storage",
		1704067200000, 1704067205000,
	)
	storage.addMessage(
		t, storageID, "msg-a1", "assistant", 1704067201000, nil,
	)
	storage.addTextPart(
		t, storageID, "msg-a1", "part-a1",
		"storage reply", 1704067201000,
	)

	// DB-only legacy session in the same root, plus a SQLite duplicate
	// of the storage session that the storage-ID filter must drop.
	sqlite := createOpenCodeDB(t, env.opencodeDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/legacy-app")
	const legacyID = "oc-mixed-legacy"
	timeCreated := int64(1704067200000)
	sqlite.addSession(t, legacyID, "proj-1", timeCreated, timeCreated+5000)
	sqlite.addMessage(t, "lg-msg-u1", legacyID, "user", timeCreated)
	sqlite.addMessage(t, "lg-msg-a1", legacyID, "assistant", timeCreated+1)
	sqlite.addTextPart(
		t, "lg-part-u1", legacyID, "lg-msg-u1",
		"legacy question", timeCreated,
	)
	sqlite.addTextPart(
		t, "lg-part-a1", legacyID, "lg-msg-a1",
		"legacy answer", timeCreated+1,
	)
	sqlite.addSession(t, storageID, "proj-1", timeCreated, timeCreated+5000)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentOpenCode},
	})
	// Examined:2/Identical:2 proves the DB-only legacy session was
	// re-parsed and compared alongside the storage session. A Skipped
	// count here means opencode.db was not synthesized for the
	// storage-mode root; a Changed count means the storage-ID filter
	// let the SQLite duplicate shadow the storage transcript.
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 2, Identical: 2,
	}, report.Totals, "both opencode sources must be examined")
	assert.Equal(t, 2, report.FilesExamined,
		"storage session file and opencode.db examined")
	assert.False(t, report.HasFailures(), "clean mixed opencode run")
}

// TestParseDiffKiroSQLitePerSessionError proves a malformed session
// inside the shared Kiro store surfaces as DiffParseError instead of
// being silently dropped (unstored) or misclassified as presence
// drift (stored), so --fail-on-change stays trustworthy.
func TestParseDiffKiroSQLitePerSessionError(t *testing.T) {
	t.Run("stored session turned malformed", func(t *testing.T) {
		env := setupTestEnv(t)
		ks := createKiroSQLiteDB(t, env.kiroDir)
		ks.addSession(
			t, "/home/user/code/kiro-app", "sqlite-session",
			readKiroSQLiteFixture(t, "standard_payload.json"),
			1779012000000, 1779012030000,
		)
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})

		ks.updateSession(t, "sqlite-session", "{corrupt", 1779012060000)

		report := runParseDiff(t, env, sync.ParseDiffOptions{
			Agents: []parser.AgentType{parser.AgentKiro},
		})
		assert.Equal(t, sync.ParseDiffTotals{
			ParseErrors: 1,
		}, report.Totals,
			"a malformed stored session is a parse error, not presence drift")
		assert.Empty(t, report.FieldCounts,
			"no presence diff for a session that failed to parse")

		sd := findSessionDiff(report, "kiro:sqlite-session")
		require.NotNil(t, sd, "stored session must be attributed by ID")
		assert.Equal(t, sync.DiffParseError, sd.Class, "class")
		assert.Contains(t, sd.Reason, "malformed payload", "reason")
		assert.True(t, report.HasFailures(),
			"per-session parse errors must trip --fail-on-change")
	})

	t.Run("unstored malformed session still reported", func(t *testing.T) {
		env := setupTestEnv(t)
		ks := createKiroSQLiteDB(t, env.kiroDir)
		ks.addSession(
			t, "/home/user/code/kiro-app", "good-session",
			readKiroSQLiteFixture(t, "standard_payload.json"),
			1779012000000, 1779012030000,
		)
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})

		// Never synced: written to the store after the sync.
		ks.addSession(
			t, "/home/user/code/kiro-app", "bad-session",
			"{corrupt", 1779012040000, 1779012050000,
		)

		report := runParseDiff(t, env, sync.ParseDiffOptions{
			Agents: []parser.AgentType{parser.AgentKiro},
		})
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 1, Identical: 1, ParseErrors: 1,
		}, report.Totals,
			"good session compared; bad session is a parse error")

		var errEntry *sync.SessionDiff
		for i := range report.Sessions {
			if report.Sessions[i].Class == sync.DiffParseError {
				errEntry = &report.Sessions[i]
			}
		}
		require.NotNil(t, errEntry, "parse error entry listed")
		assert.Contains(t, errEntry.FilePath, "data.sqlite3#bad-session",
			"error attributed to the per-session virtual path")
		assert.True(t, report.HasFailures(), "HasFailures")
	})
}

func TestParseDiffPresenceSweep(t *testing.T) {
	t.Run("current-version row no longer emitted", func(t *testing.T) {
		env := setupTestEnv(t)
		path := env.writeClaudeSession(t, "test-proj", "pd-real.jsonl",
			parseDiffClaudeContent("real prompt", "real reply"))
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})

		// A current-version row under the same source file with an ID
		// today's parser never derives: the loudest drift signal.
		require.NoError(t, env.db.UpsertSession(db.Session{
			ID: "pd-phantom", Project: "test-proj", Machine: "local",
			Agent: "claude", FilePath: &path,
		}), "insert phantom session")
		require.NoError(t,
			env.db.SetSessionDataVersion(
				"pd-phantom", db.CurrentDataVersion(),
			), "stamp current data version")

		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 2, Identical: 1, Changed: 1,
		}, report.Totals, "totals")
		assert.Equal(t, map[string]int{sync.FieldPresence: 1},
			report.FieldCounts, "field counts")

		sd := findSessionDiff(report, "pd-phantom")
		require.NotNil(t, sd, "phantom session not listed")
		assert.Equal(t, sync.DiffChanged, sd.Class, "class")
		assert.Contains(t, sessionDiffFieldNames(sd, false),
			sync.FieldPresence, "presence diff")
		assert.True(t, report.HasFailures(),
			"a current-version presence drop is parser drift")
	})

	t.Run("stale row no longer emitted is pending resync", func(t *testing.T) {
		env := setupTestEnv(t)
		path := env.writeClaudeSession(t, "test-proj", "pd-real.jsonl",
			parseDiffClaudeContent("real prompt", "real reply"))
		runSyncAndAssert(t, env.engine, sync.SyncStats{
			TotalSessions: 1, Synced: 1,
		})

		// Data version 0: an incomplete write preserved by the
		// archive (e.g. a transient fork row left by a live sync).
		require.NoError(t, env.db.UpsertSession(db.Session{
			ID: "pd-zombie", Project: "test-proj", Machine: "local",
			Agent: "claude", FilePath: &path,
		}), "insert zombie session")

		report := runParseDiff(t, env, sync.ParseDiffOptions{})
		assert.Equal(t, sync.ParseDiffTotals{
			Examined: 2, Identical: 1, PendingResync: 1,
		}, report.Totals, "totals")
		assert.Empty(t, report.FieldCounts,
			"stale presence is pipeline history, not drift")

		sd := findSessionDiff(report, "pd-zombie")
		require.NotNil(t, sd, "zombie session not listed")
		assert.Equal(t, sync.DiffPendingResync, sd.Class, "class")
		assert.Contains(t, sessionDiffFieldNames(sd, true),
			sync.FieldPresence,
			"presence field attached for drill-down")
		assert.False(t, report.HasFailures(),
			"stale rows must not trip --fail-on-change")
	})
}
