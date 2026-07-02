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
	"strings"
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
		AgentDirs: parseDiffAgentDirs(env),
		Machine:   "local",
	})
}

func parseDiffAgentDirs(env *testEnv) map[parser.AgentType][]string {
	dirs := map[parser.AgentType][]string{}
	add := func(agent parser.AgentType, dir string) {
		if dir != "" {
			dirs[agent] = []string{dir}
		}
	}
	add(parser.AgentClaude, env.claudeDir)
	add(parser.AgentCodex, env.codexDir)
	add(parser.AgentCursor, env.cursorDir)
	add(parser.AgentGemini, env.geminiDir)
	add(parser.AgentOpenCode, env.opencodeDir)
	add(parser.AgentForge, env.forgeDir)
	add(parser.AgentPiebald, env.piebaldDir)
	add(parser.AgentWarp, env.warpDir)
	add(parser.AgentIflow, env.iflowDir)
	add(parser.AgentAmp, env.ampDir)
	add(parser.AgentPi, env.piDir)
	add(parser.AgentOMP, env.ompDir)
	add(parser.AgentKiro, env.kiroDir)
	add(parser.AgentKilo, env.kiloDir)
	add(parser.AgentShelley, env.shelleyDir)
	add(parser.AgentAntigravityCLI, env.antigravityCLIDir)
	return dirs
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
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentCodex)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentGemini)

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
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentGemini)

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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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

	// Capture the original mtime so the append can be replayed without
	// tripping the live-write skew guard: appending advances the file
	// mtime past the stored snapshot, which would (correctly) classify
	// the diff as raced. Restoring the mtime keeps this test focused on
	// the skip-layer bypass and the message_count CHANGE detection; the
	// raced path has dedicated coverage in TestParseDiffRacedSourceSkew.
	origInfo, err := os.Stat(path)
	require.NoError(t, err, "stat source before append")

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
	require.NoError(t,
		os.Chtimes(path, origInfo.ModTime(), origInfo.ModTime()),
		"restore source mtime so the change is not classified raced")

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
		env := setupSingleAgentTestEnv(t, parser.AgentClaude)
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
		env := setupSingleAgentTestEnv(t, parser.AgentClaude)
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
		env := setupSingleAgentTestEnv(t, parser.AgentClaude)
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
		env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentGemini)
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
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

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
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentCodex)

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

func TestParseDiffCoversProviderAuthoritativePiFamily(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentPi, parser.AgentOMP)
	env.writeSession(
		t,
		env.piDir,
		filepath.Join("encoded-cwd", "pd-pi.jsonl"),
		piLikeProviderFixture("pd-pi", "/Users/alice/code/pi-app"),
	)
	env.writeSession(
		t,
		env.ompDir,
		filepath.Join("encoded-cwd", "pd-omp.jsonl"),
		piLikeProviderFixture("pd-omp", "/Users/alice/code/omp-app"),
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentPi, parser.AgentOMP},
	})
	assert.Equal(t, []string{"pi", "omp"}, report.Agents)
	assert.Equal(t, 2, report.FilesExamined)
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 2, Identical: 2,
	}, report.Totals)
}

// TestParseDiffCoversKiroSQLite proves that Kiro's shared data.sqlite3
// store, which the provider discovers and fans out to one session per
// row, is actually re-parsed by parse-diff. A
// regressed force-parse guard or missing synthesized discovery would
// surface here as the session being skipped/"not discovered" with
// Examined 0 rather than compared.
func TestParseDiffCoversKiroSQLite(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
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
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)

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

// TestParseDiffCoversMixedKiloRoot is the Kilo mirror of the OpenCode
// case above. Kilo reuses OpenCode's hybrid storage, so a storage-mode
// root that still carries DB-only sessions in kilo.db must have BOTH
// sources re-parsed.
func TestParseDiffCoversMixedKiloRoot(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKilo)

	// File-backed storage session: this makes ResolveKiloSource pick storage
	// mode for the root.
	storage := createOpenCodeStorageFixture(t, env.kiloDir)
	const storageID = "kilo-mixed-storage"
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
	sqlite := createKiloDB(t, env.kiloDir)
	sqlite.addProject(t, "proj-1", "/home/user/code/legacy-app")
	const legacyID = "kilo-mixed-legacy"
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
		Agents: []parser.AgentType{parser.AgentKilo},
	})
	// Examined:2/Identical:2 proves the DB-only legacy session was
	// re-parsed and compared alongside the storage session.
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 2, Identical: 2,
	}, report.Totals, "both kilo sources must be examined")
	assert.Equal(t, 2, report.FilesExamined,
		"storage session file and kilo.db examined")
	assert.False(t, report.HasFailures(), "clean mixed kilo run")
}

// TestParseDiffCoversShelley proves Shelley's shared shelley.db — which
// the provider discovers as a single source and which normal sync fans
// out to one session per conversation — is re-parsed and compared by parse-diff.
// Examined:1/Identical:1 means the stored conversation was matched and
// vetted, not bucketed as skipped/"not discovered".
func TestParseDiffCoversShelley(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentShelley)
	dbPath := createShelleyDB(t, env.shelleyDir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentShelley},
	})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1,
	}, report.Totals, "shelley conversation must be examined, not skipped")
	assert.Equal(t, 1, report.FilesExamined, "shelley.db examined")
	assert.False(t, report.HasFailures(), "clean shelley run")
}

// TestParseDiffShelleyDBErrorAttributed proves that when the whole
// shelley.db can no longer be read, the stored conversations under it
// are reported as DiffParseError attributed to their session IDs, not
// misclassified as source-missing in the final sweep. This exercises
// stripVirtualSourceSuffix mapping shelley.db#id back to shelley.db so
// the parse error keyed by the real DB path matches the stored rows.
func TestParseDiffShelleyDBErrorAttributed(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentShelley)
	dbPath := createShelleyDB(t, env.shelleyDir)
	seedShelleyConvo(t, dbPath, "cMAIN1", "main", "/home/u/dev/app",
		"claude-sonnet-4-6", "", true,
		"2026-06-15T10:00:00Z", "2026-06-15T10:00:06Z", mainConvoMsgs())

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Break the DB so the conversation meta query fails: the file is
	// still a regular shelley.db (so it is discovered), but reading it
	// errors, forcing the whole-file job error path.
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(`DROP TABLE messages`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentShelley},
	})
	assert.Equal(t, sync.ParseDiffTotals{ParseErrors: 1}, report.Totals,
		"unreadable shelley.db is a parse error for the stored conversation")

	sd := findSessionDiff(report, "shelley:cMAIN1")
	require.NotNil(t, sd, "stored conversation attributed by session ID")
	assert.Equal(t, sync.DiffParseError, sd.Class, "class")
	assert.True(t, report.HasFailures(),
		"a DB read failure must trip --fail-on-change")
}

// TestParseDiffKiroSQLitePerSessionError proves a malformed session
// inside the shared Kiro store surfaces as DiffParseError instead of
// being silently dropped (unstored) or misclassified as presence
// drift (stored), so --fail-on-change stays trustworthy.
func TestParseDiffKiroSQLitePerSessionError(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	standard := readKiroSQLiteFixture(t, "standard_payload.json")
	ks.addSession(
		t, "/home/user/code/kiro-app", "sqlite-session",
		standard, 1779012000000, 1779012030000,
	)
	ks.addSession(
		t, "/home/user/code/kiro-app", "good-session",
		standard, 1779012100000, 1779012130000,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	ks.updateSession(t, "sqlite-session", "{corrupt", 1779012060000)
	// Never synced: written to the store after the sync.
	ks.addSession(
		t, "/home/user/code/kiro-app", "bad-session",
		"{corrupt", 1779012040000, 1779012050000,
	)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentKiro},
	})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1, ParseErrors: 2,
	}, report.Totals,
		"good session compared; both malformed sessions are parse errors")
	assert.Empty(t, report.FieldCounts,
		"no presence diff for sessions that failed to parse")

	stored := findSessionDiff(report, "kiro:sqlite-session")
	require.NotNil(t, stored, "stored session must be attributed by ID")
	assert.Equal(t, sync.DiffParseError, stored.Class, "stored class")
	assert.Contains(t, stored.Reason, "malformed payload", "stored reason")

	var unstored *sync.SessionDiff
	for i := range report.Sessions {
		if report.Sessions[i].Class == sync.DiffParseError &&
			strings.Contains(report.Sessions[i].FilePath, "bad-session") {
			unstored = &report.Sessions[i]
		}
	}
	require.NotNil(t, unstored, "unstored parse error entry listed")
	assert.Contains(t, unstored.FilePath, "data.sqlite3#bad-session",
		"error attributed to the per-session virtual path")
	assert.True(t, report.HasFailures(),
		"per-session parse errors must trip --fail-on-change")
}

// TestParseDiffKiloSQLitePerSessionError proves a malformed session
// inside the shared Kilo store surfaces as DiffParseError instead of
// being silently dropped, so --fail-on-change stays trustworthy even
// for a session that was never stored.
func TestParseDiffKiloSQLitePerSessionError(t *testing.T) {

	env := setupSingleAgentTestEnv(t, parser.AgentKilo)
	ks := createKiloDB(t, env.kiloDir)
	ks.addProject(t, "proj-1", "/home/user/code/kilo-app")
	const goodID = "good-session"
	good := int64(1779012000000)
	ks.addSession(t, goodID, "proj-1", good, good+30000)
	ks.addMessage(t, "g-msg-u1", goodID, "user", good)
	ks.addMessage(t, "g-msg-a1", goodID, "assistant", good+1)
	ks.addTextPart(t, "g-part-u1", goodID, "g-msg-u1", "good question", good)
	ks.addTextPart(t, "g-part-a1", goodID, "g-msg-a1", "good answer", good+1)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Never synced: a session row whose non-integer time_created is
	// listed for fan-out but fails the per-session parse afterward.
	const badID = "bad-session"
	ks.mustExec(t, "insert malformed session",
		`INSERT INTO session
			(id, project_id, time_created, time_updated)
		 VALUES (?, ?, ?, ?)`,
		badID, "proj-1", "not-a-number", good+40000,
	)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentKilo},
	})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1, ParseErrors: 1,
	}, report.Totals,
		"good session compared; malformed session is a parse error")

	var errEntry *sync.SessionDiff
	for i := range report.Sessions {
		if report.Sessions[i].Class == sync.DiffParseError {
			errEntry = &report.Sessions[i]
		}
	}
	require.NotNil(t, errEntry, "parse error entry listed")
	assert.Contains(t, errEntry.FilePath, "kilo.db#bad-session",
		"error attributed to the per-session virtual path")
	assert.True(t, report.HasFailures(), "HasFailures")
}

// TestParseDiffRacedSourceSkew is the end-to-end live-write skew guard:
// a stored row that drifted from its source is normally DiffChanged, but
// when the on-disk source advances past the snapshot file_mtime mid-run
// (simulating a daemon or active session rewriting the file after the
// snapshot) the change is inconclusive and must be reclassified DiffRaced
// without tripping --fail-on-change. A control session whose source was
// not touched stays a real DiffChanged so a genuine regression is never
// masked.
func TestParseDiffRacedSourceSkew(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	racedPath := env.writeClaudeSession(t, "test-proj", "pd-raced.jsonl",
		parseDiffClaudeContent("raced prompt", "raced reply"))
	env.writeClaudeSession(t, "test-proj", "pd-control.jsonl",
		parseDiffClaudeContent("control prompt", "control reply"))
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	// Drift the stored rows so a re-parse of the unchanged source content
	// would report a real first_message change for both.
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "pd-raced")
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "pd-control")

	// Simulate a mid-run write to pd-raced's source only: push its mtime
	// well past the stored snapshot file_mtime. The content is unchanged,
	// so the comparison still detects the seeded drift, but the advanced
	// mtime marks the comparison as a torn read. pd-control is left
	// untouched, so its drift stays a genuine change.
	future := time.Now().Add(48 * time.Hour)
	require.NoError(t, os.Chtimes(racedPath, future, future),
		"advance raced source mtime")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})

	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 2, Changed: 1, Raced: 1,
	}, report.Totals, "totals")
	// Only the untouched-source change is counted as drift; the raced
	// session's masked field diff is excluded from FieldCounts.
	assert.Equal(t, map[string]int{sync.FieldFirstMessage: 1},
		report.FieldCounts,
		"only the genuine change contributes to FieldCounts")

	raced := findSessionDiff(report, "pd-raced")
	require.NotNil(t, raced, "raced session not listed")
	assert.Equal(t, sync.DiffRaced, raced.Class, "raced class")
	assert.NotEmpty(t, raced.Reason, "raced reason")
	// The would-be change is attached for drill-down even though it is
	// not counted, so an operator can see what the skew masked.
	assert.Contains(t, sessionDiffFieldNames(raced, true),
		sync.FieldFirstMessage,
		"raced field diff attached for drill-down")

	changed := findSessionDiff(report, "pd-control")
	require.NotNil(t, changed, "control session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class,
		"untouched-source drift stays changed")
	assert.Contains(t, sessionDiffFieldNames(changed, false),
		sync.FieldFirstMessage, "control change field")

	// The run fails because of the genuine change, not the raced one.
	assert.True(t, report.HasFailures(),
		"the untouched-source change must still trip --fail-on-change")
}

// TestParseDiffRacedAloneDoesNotFail isolates the gate contract: a run
// whose only drift is masked by a live-write skew classifies raced and
// must NOT trip --fail-on-change, so a concurrent daemon write can never
// turn a vet run red on its own.
func TestParseDiffRacedAloneDoesNotFail(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	racedPath := env.writeClaudeSession(t, "test-proj", "pd-solo.jsonl",
		parseDiffClaudeContent("solo prompt", "solo reply"))
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "pd-solo")
	future := time.Now().Add(48 * time.Hour)
	require.NoError(t, os.Chtimes(racedPath, future, future),
		"advance raced source mtime")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Raced: 1,
	}, report.Totals, "totals")
	assert.Empty(t, report.FieldCounts,
		"raced field diffs must be excluded from FieldCounts")
	assert.False(t, report.HasFailures(),
		"a raced session alone must not trip --fail-on-change")
}

// TestParseDiffRacedDoesNotMaskCleanRun proves the skew guard never
// invents a raced session out of an identical comparison: advancing the
// source mtime of a session whose stored rows still match the parse
// leaves it identical, not raced (the raced reclass only applies when
// there is a real change to mask).
func TestParseDiffRacedDoesNotMaskCleanRun(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	path := env.writeClaudeSession(t, "test-proj", "pd-clean.jsonl",
		parseDiffClaudeContent("clean prompt", "clean reply"))
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Advance the mtime without changing content: the comparison is still
	// identical, so the session must remain identical despite the skew.
	future := time.Now().Add(72 * time.Hour)
	require.NoError(t, os.Chtimes(path, future, future), "advance mtime")

	report := runParseDiff(t, env, sync.ParseDiffOptions{})
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 1, Identical: 1,
	}, report.Totals, "an unchanged session must stay identical, not raced")
	assert.False(t, report.HasFailures(), "clean run")
}

// TestParseDiffDBBackedSourceNotMaskedAsRaced pins the reliability gate for
// composite, DB-backed sources. Many Kiro sessions share ONE data.sqlite3, and
// the live mtime the skew guard could observe (a composite db stat or per-row
// updated_at) is NOT a basis-matching stat of a literal file whose mtime
// populated file_mtime. Because that comparison is unreliable, the raced
// reclassification must NOT apply to these sources at all -- even when a
// session's row updated_at advanced past its snapshot, a detected drift stays a
// genuine DiffChanged rather than being masked as DiffRaced. This fails CLOSED:
// real parser regressions on DB-backed agents are never hidden from
// --fail-on-change.
func TestParseDiffDBBackedSourceNotMaskedAsRaced(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	ks := createKiroSQLiteDB(t, env.kiroDir)
	const (
		advancedID  = "kiro-advanced"
		quiescentID = "kiro-quiescent"
	)
	// Both sessions share the same payload content and the same initial
	// updated_at, so each is stored with the same per-session snapshot
	// file_mtime (updated_at * 1e6).
	payload := readKiroSQLiteFixture(t, "standard_payload.json")
	const initialUpdatedAt = int64(1779012030000)
	ks.addSession(
		t, "/home/user/code/kiro-app", advancedID,
		payload, 1779012000000, initialUpdatedAt,
	)
	ks.addSession(
		t, "/home/user/code/kiro-app", quiescentID,
		payload, 1779012000000, initialUpdatedAt,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 2, Synced: 2,
	})

	// Seed real parser drift in the archive for BOTH sessions so a
	// re-parse of the unchanged payload detects a first_message change.
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "kiro:"+advancedID)
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "kiro:"+quiescentID)

	// Advance ONLY one session's row updated_at past its snapshot. Under the
	// old per-session raced check this would have masked the advanced session
	// as DiffRaced; the reliability gate now skips the raced check for this
	// DB-backed (virtual-path) source entirely, so the drift stays genuine.
	ks.updateSession(t, advancedID, payload, initialUpdatedAt+60000)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentKiro},
	})

	// Neither session is masked as raced: a DB-backed source has no
	// basis-matching live mtime, so both genuine drifts are reported as
	// changes and --fail-on-change fires.
	assert.Equal(t, sync.ParseDiffTotals{
		Examined: 2, Changed: 2,
	}, report.Totals, "DB-backed drift must not be masked as raced")
	// Both seeded first_message drifts are counted; the advanced session's
	// updated_at bump additionally surfaces its ended_at change. Under the old
	// raced reclassification the advanced session (and all its field diffs)
	// would have been masked, hiding genuine drift from --fail-on-change.
	assert.Equal(t, map[string]int{
		sync.FieldFirstMessage: 2,
		sync.FieldEndedAt:      1,
	}, report.FieldCounts,
		"genuine drift on DB-backed sources is no longer masked")

	advanced := findSessionDiff(report, "kiro:"+advancedID)
	require.NotNil(t, advanced, "advanced session not listed")
	assert.Equal(t, sync.DiffChanged, advanced.Class,
		"advanced DB-backed drift stays changed, not raced")
	assert.Contains(t, sessionDiffFieldNames(advanced, false),
		sync.FieldFirstMessage, "advanced field")

	quiescent := findSessionDiff(report, "kiro:"+quiescentID)
	require.NotNil(t, quiescent, "quiescent session not listed")
	assert.Equal(t, sync.DiffChanged, quiescent.Class,
		"untouched-source drift stays changed")
	assert.Contains(t, sessionDiffFieldNames(quiescent, false),
		sync.FieldFirstMessage, "quiescent field")

	assert.True(t, report.HasFailures(),
		"genuine DB-backed drift must trip --fail-on-change")
}

// TestParseDiffCodexIndexSkewDoesNotMaskTranscriptDrift pins that Codex's
// shared session_index.jsonl mtime is not a per-session raced signal for
// transcript-derived diffs. Advancing ONLY the index past the snapshot leaves
// the transcript untouched; seeded first_message drift must stay DiffChanged
// so --fail-on-change cannot pass because some unrelated title/index write
// advanced the global index.
func TestParseDiffCodexIndexSkewDoesNotMaskTranscriptDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentCodex, []string{codexDir},
	)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding coverage.").
		String()
	sessionPath := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", content,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Codex title",`+
			`"updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))
	// Pin both files to one past instant so the stored snapshot file_mtime
	// (max of transcript and index) is that instant for both.
	base := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(sessionPath, base, base), "chtimes session")
	require.NoError(t, os.Chtimes(indexPath, base, base), "chtimes index")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	// Seed real parser drift in the archive so a re-parse of the unchanged
	// transcript reports a first_message change to reclassify.
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "codex:"+uuid)

	// Advance ONLY the index past the snapshot; the transcript is untouched.
	// CodexEffectiveMtime folds the index in, but that global index mtime is
	// not evidence that this session's transcript-derived first_message diff
	// raced with a live write.
	future := time.Now().Add(48 * time.Hour)
	require.NoError(t, os.Chtimes(indexPath, future, future),
		"advance index mtime")

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "index-only skew must not mask transcript drift")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"first_message drift must remain counted")
	changed := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, changed, "codex session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class, "changed class")
	assert.True(t, report.HasFailures(),
		"transcript drift must trip --fail-on-change")
}

// TestParseDiffCodexTranscriptSkewUsesTranscriptStoredMtime pins the other
// Codex mtime basis: the raced guard must compare the transcript-only live
// mtime against the transcript-only stored mtime. Stored file_mtime still folds
// in session_index.jsonl for normal sync invalidation, and that index mtime can
// be newer than both transcript mtimes; it must not prevent a later transcript
// write from being classified DiffRaced.
func TestParseDiffCodexTranscriptSkewUsesTranscriptStoredMtime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	codexDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(codexDir, 0o755))
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentCodex, []string{codexDir},
	)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e2"
	original := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Add tests").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding coverage.").
		String()
	sessionPath := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", original,
	)

	indexPath := filepath.Join(root, "session_index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Codex title",`+
			`"updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))
	transcriptSnapshot := time.Now().Add(-4 * time.Hour)
	transcriptWrite := time.Now().Add(-3 * time.Hour)
	indexSnapshot := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(
		sessionPath, transcriptSnapshot, transcriptSnapshot,
	), "chtimes session snapshot")
	require.NoError(t, os.Chtimes(indexPath, indexSnapshot, indexSnapshot),
		"chtimes index snapshot")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	stored, err := env.db.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, stored, "stored Codex session")
	require.NotNil(t, stored.FileMtime, "stored file_mtime")
	assert.Equal(t, indexSnapshot.UnixNano(), *stored.FileMtime,
		"stored file_mtime should be index-folded")

	changed := testjsonl.NewSessionBuilder().
		AddCodexMeta(tsEarly, uuid, "/home/user/code/api", "user").
		AddCodexMessage(tsEarlyS1, "user", "Changed prompt").
		AddCodexMessage(tsEarlyS5, "assistant", "Adding coverage.").
		String()
	require.NoError(t, os.WriteFile(sessionPath, []byte(changed), 0o644),
		"rewrite transcript")
	require.NoError(t, os.Chtimes(sessionPath, transcriptWrite, transcriptWrite),
		"advance transcript below index-folded stored mtime")

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Raced: 1},
		report.Totals, "transcript write should be raced")
	assert.Empty(t, report.FieldCounts,
		"raced field diffs must not count as parser drift")
	raced := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, raced, "codex session not listed")
	assert.Equal(t, sync.DiffRaced, raced.Class, "raced class")
	assert.False(t, report.HasFailures(),
		"transcript write skew must not trip --fail-on-change")
}

// TestParseDiffCodexIncrementalAppendDoesNotLookRaced covers the stable-source
// side of the Codex transcript fingerprint fallback. A Codex incremental append
// advances the stored source snapshot, so later parser drift against that
// unchanged transcript must remain DiffChanged rather than being hidden as a
// stale-fingerprint race.
func TestParseDiffCodexIncrementalAppendDoesNotLookRaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e3"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", initial,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "world", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, f.Close(), "close after append")
	require.NoError(t, err, "append")
	env.engine.SyncPaths([]string{path})
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)

	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "codex:"+uuid)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "unchanged-source drift must stay changed")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"first_message drift must be counted")
	changed := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, changed, "codex session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class, "changed class")
	assert.True(t, report.HasFailures(),
		"stable-source parser drift must trip --fail-on-change")
}

func TestParseDiffCodexLegacyStaleIncrementalHashDoesNotLookRaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", initial,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})
	beforeAppend, err := env.db.GetSessionFull(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err, "GetSessionFull before append")
	require.NotNil(t, beforeAppend, "session before append")
	require.NotNil(t, beforeAppend.FileHash, "file_hash before append")
	staleHash := *beforeAppend.FileHash

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "world", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, f.Close(), "close after append")
	require.NoError(t, err, "append")
	env.engine.SyncPaths([]string{path})
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)

	// Simulate a current data-version archive written by the legacy
	// incremental path: size/mtime advanced, but file_hash stayed on the
	// previous full-parse snapshot.
	mutateDB(t, env,
		"UPDATE sessions SET file_hash = ? WHERE id = ?",
		staleHash, "codex:"+uuid)
	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "codex:"+uuid)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "legacy stale hash drift must stay changed")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"first_message drift must be counted")
	changed := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, changed, "codex session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class, "changed class")
	assert.True(t, report.HasFailures(),
		"stable-source parser drift must trip --fail-on-change")
}

func TestParseDiffCodexFullParsePartialTailDoesNotLookRaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e6"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	) + `{"timestamp":"2024-01-01T10:00:10Z"`
	path := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", content,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 1)

	stored, err := env.db.GetSessionFull(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err, "GetSessionFull after full parse")
	require.NotNil(t, stored, "stored session after full parse")
	require.NotNil(t, stored.FileSize, "stored file_size")
	info, err := os.Stat(path)
	require.NoError(t, err, "stat transcript")
	assert.Equal(t, info.Size(), *stored.FileSize,
		"full Codex parse stores raw file size including ignored tail")

	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "codex:"+uuid)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "unchanged full-parse partial tail drift must stay changed")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"first_message drift must be counted")
	changed := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, changed, "codex session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class, "changed class")
	assert.True(t, report.HasFailures(),
		"stable-source parser drift must trip --fail-on-change")
}

func TestParseDiffCodexIncrementalPartialTailDoesNotLookRaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentCodex)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e4"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp/proj", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "hello", tsEarlyS1),
	)
	path := env.writeCodexSession(
		t, filepath.Join("2026", "06", "11"),
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl", initial,
	)
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "world", tsEarlyS5),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	require.NoError(t, err, "append complete message")
	_, err = f.WriteString(`{"timestamp":"2024-01-01T10:00:10Z"`)
	require.NoError(t, err, "append partial trailing JSON")
	require.NoError(t, f.Close(), "close after append")
	env.engine.SyncPaths([]string{path})
	assertSessionMessageCount(t, env.db, "codex:"+uuid, 2)

	mutateDB(t, env,
		"UPDATE sessions SET first_message = ? WHERE id = ?",
		"drifted first message", "codex:"+uuid)

	report := runParseDiff(t, env, sync.ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	assert.Equal(t, sync.ParseDiffTotals{Examined: 1, Changed: 1},
		report.Totals, "unchanged consumed prefix drift must stay changed")
	assert.Equal(t, 1, report.FieldCounts[sync.FieldFirstMessage],
		"first_message drift must be counted")
	changed := findSessionDiff(report, "codex:"+uuid)
	require.NotNil(t, changed, "codex session not listed")
	assert.Equal(t, sync.DiffChanged, changed.Class, "changed class")
	assert.True(t, report.HasFailures(),
		"stable-source parser drift must trip --fail-on-change")
}

// writeHermesFanoutStateDB writes a Hermes state.db at <root>/state.db holding
// two sessions (each one user message) and no sibling transcripts, so both
// sessions resolve to the SAME shared state.db File.Path -- the literal-path
// fan-out shape.
func writeHermesFanoutStateDB(t *testing.T, root string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	require.NoError(t, err, "open hermes state.db")
	defer conn.Close()
	_, err = conn.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT,
			model TEXT, model_config TEXT, system_prompt TEXT,
			parent_session_id TEXT, started_at REAL NOT NULL, ended_at REAL,
			end_reason TEXT, message_count INTEGER DEFAULT 0,
			tool_call_count INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0, cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
			billing_provider TEXT, billing_base_url TEXT, billing_mode TEXT,
			estimated_cost_usd REAL, actual_cost_usd REAL, cost_status TEXT,
			cost_source TEXT, pricing_version TEXT, title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
			role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT,
			tool_name TEXT, timestamp REAL NOT NULL, token_count INTEGER,
			finish_reason TEXT, reasoning TEXT, reasoning_content TEXT,
			reasoning_details TEXT, codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (id, source, model, started_at, ended_at, message_count, title)
			VALUES ('alpha', 'discord', 'gpt-5.5', 1778767200.0, 1778767800.0, 1, 'Alpha');
		INSERT INTO sessions (id, source, model, started_at, ended_at, message_count, title)
			VALUES ('beta', 'discord', 'gpt-5.5', 1778768200.0, 1778768800.0, 1, 'Beta');
		INSERT INTO messages (session_id, role, content, timestamp)
			VALUES ('alpha', 'user', 'alpha prompt', 1778767210.0);
		INSERT INTO messages (session_id, role, content, timestamp)
			VALUES ('beta', 'user', 'beta prompt', 1778768210.0);
	`)
	require.NoError(t, err, "seed hermes state.db")
}

// TestParseDiffHermesSharedStateDBNotMaskedAsRaced pins the literal-path
// fan-out gate. Many Hermes sessions resolve to one shared state.db File.Path,
// so its mtime is not a per-session signal: a write that advances state.db for
// any session would, without the fan-out guard, race-mask genuine drift on
// every sibling. Both sessions must therefore stay DiffChanged (fail closed),
// not DiffRaced, even though the shared source mtime advanced past the snapshot.
func TestParseDiffHermesSharedStateDBNotMaskedAsRaced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	writeHermesFanoutStateDB(t, root)

	database := dbtest.OpenTestDB(t)
	cfg := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	}
	engine := sync.NewEngine(database, cfg)
	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 2, stats.Synced, "expected both Hermes sessions synced")

	// Seed real parser drift in the archive for BOTH sessions so a re-parse
	// of the unchanged state.db reports a first_message change.
	for _, id := range []string{"hermes:alpha", "hermes:beta"} {
		require.NoError(t, database.Update(func(tx *sql.Tx) error {
			_, err := tx.Exec(
				"UPDATE sessions SET first_message = ? WHERE id = ?",
				"drifted first message", id,
			)
			return err
		}), "drift %s", id)
	}

	// Advance the shared state.db mtime well past the stored snapshot, as a
	// concurrent write to any session would.
	future := time.Now().Add(48 * time.Hour)
	require.NoError(t, os.Chtimes(
		filepath.Join(root, "state.db"), future, future,
	), "advance state.db mtime")

	report, err := sync.NewDiffEngine(database, cfg).ParseDiff(
		context.Background(),
		sync.ParseDiffOptions{Agents: []parser.AgentType{parser.AgentHermes}},
	)
	require.NoError(t, err, "ParseDiff")
	require.NotNil(t, report)

	assert.Equal(t, sync.ParseDiffTotals{Examined: 2, Changed: 2},
		report.Totals, "shared-source drift must not be masked as raced")
	for _, id := range []string{"hermes:alpha", "hermes:beta"} {
		sd := findSessionDiff(report, id)
		require.NotNil(t, sd, "%s not listed", id)
		assert.Equal(t, sync.DiffChanged, sd.Class,
			"%s stays changed, not raced", id)
	}
	assert.True(t, report.HasFailures(),
		"genuine shared-source drift must trip --fail-on-change")
}

// TestParseDiffEngineRefusesWrites proves sub-item (4): the report-only
// engine NewDiffEngine returns must refuse the write entrypoints
// (SyncAll/ResyncAll and friends) so a forceParse engine can never be
// driven into rewriting the archive. The refusal is a no-op (zero stats,
// nil error) and persists nothing.
func TestParseDiffEngineRefusesWrites(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)

	path := env.writeClaudeSession(t, "test-proj", "pd-guard.jsonl",
		parseDiffClaudeContent(
			"guard prompt with AKIA7QHWN2DKR4FYPLJM",
			"guard reply",
		))

	diffEngine := newParseDiffEngine(env)
	ctx := context.Background()

	assert.Equal(t, sync.SyncStats{}, diffEngine.SyncAll(ctx, nil),
		"SyncAll on a report-only engine must be a no-op")
	assert.Equal(t, sync.SyncStats{}, diffEngine.ResyncAll(ctx, nil),
		"ResyncAll on a report-only engine must be a no-op")
	assert.Equal(t, sync.SyncStats{},
		diffEngine.SyncAllSince(ctx, time.Time{}, nil),
		"SyncAllSince on a report-only engine must be a no-op")
	assert.Equal(t, sync.SyncStats{},
		diffEngine.SyncRootsSince(ctx, nil, time.Time{}, nil),
		"SyncRootsSince on a report-only engine must be a no-op")
	stats, err := diffEngine.SyncThenRun(
		ctx, false, nil, func(forceFull bool) error {
			return env.db.UpsertSession(db.Session{
				ID: "sync-then-run-wrote",
			})
		},
	)
	require.NoError(t, err,
		"SyncThenRun on a report-only engine should refuse cleanly")
	assert.Equal(t, sync.SyncStats{}, stats,
		"SyncThenRun on a report-only engine must be a no-op")
	require.Error(t, diffEngine.RunExclusive(func() error {
		return env.db.UpsertSession(db.Session{
			ID: "run-exclusive-wrote",
		})
	}), "RunExclusive on a report-only engine must error")
	require.Error(t, diffEngine.SyncSingleSession("claude:pd-guard"),
		"SyncSingleSession on a report-only engine must error")

	// Nothing was written despite a discoverable source on disk.
	require.FileExists(t, path)
	all, err := env.db.ListSessionsModifiedBetween(ctx, "", "", nil, nil)
	require.NoError(t, err, "list sessions")
	assert.Empty(t, all,
		"refused writes must not persist any session rows")

	// The real sync engine (forceParse off) still syncs the same source,
	// proving the guard is scoped to report-only engines only.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1, Synced: 1,
	})

	const sessionID = "pd-guard"
	mutateDB(t, env,
		"UPDATE sessions SET secret_leak_count = 0, "+
			"secrets_rules_version = '', quality_signal_version = 0"+
			" WHERE id = ?", sessionID)
	mutateDB(t, env,
		"DELETE FROM secret_findings WHERE session_id = ?", sessionID)
	require.Error(t, diffEngine.RecomputeSignals(ctx, sessionID),
		"RecomputeSignals on a report-only engine must error")
	mutateDB(t, env,
		"UPDATE sessions SET secret_leak_count = 0, "+
			"secrets_rules_version = '', quality_signal_version = 0"+
			" WHERE id = ?", sessionID)
	mutateDB(t, env,
		"DELETE FROM secret_findings WHERE session_id = ?", sessionID)
	_, err = diffEngine.ScanSecrets(
		ctx, sync.SecretScanInput{Backfill: true}, nil,
	)
	require.Error(t, err,
		"ScanSecrets on a report-only engine must error")
	stored, err := env.db.GetSessionFull(ctx, sessionID)
	require.NoError(t, err, "GetSessionFull after refused scans")
	require.NotNil(t, stored, "stored session after refused scans")
	assert.Zero(t, stored.SecretLeakCount,
		"refused scans must not update secret_leak_count")
	assert.Empty(t, stored.SecretsRulesVersion,
		"refused scans must not update secrets_rules_version")
	assert.Nil(t, stored.StoredQualitySignals(),
		"refused recompute must not update quality signals")
	findings, err := env.db.SessionSecretFindings(ctx, sessionID)
	require.NoError(t, err, "SessionSecretFindings after refused scans")
	assert.Empty(t, findings,
		"refused scans must not persist secret findings")
}

func TestParseDiffPresenceSweep(t *testing.T) {

	t.Run("current-version row no longer emitted", func(t *testing.T) {
		env := setupSingleAgentTestEnv(t, parser.AgentClaude)
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
		env := setupSingleAgentTestEnv(t, parser.AgentClaude)
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
