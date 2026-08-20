package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretFindingsSchemaExists(t *testing.T) {
	d := testDB(t)
	r := d.getReader()
	var n int
	err := r.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='table' AND name='secret_findings'`).Scan(&n)
	require.NoError(t, err, "probe")
	require.Equal(t, 1, n, "secret_findings table missing")
	for _, col := range []string{"secret_leak_count", "secrets_rules_version"} {
		var cnt int
		err := r.QueryRow(
			`SELECT count(*) FROM pragma_table_info('sessions') WHERE name=?`,
			col).Scan(&cnt)
		require.NoError(t, err, "probe col %s", col)
		assert.Equal(t, 1, cnt, "sessions.%s missing", col)
	}
}

// TestHasSecretPartialIndexExists pins the partial index that backs the
// session --has-secret filter (secret_leak_count > 0), so a large sessions
// table does not scan to find the sparse set of leaky sessions.
func TestHasSecretPartialIndexExists(t *testing.T) {
	d := testDB(t)
	var n int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_has_secret'`).Scan(&n)
	require.NoError(t, err, "probe index")
	assert.Equal(t, 1, n, "idx_sessions_has_secret partial index missing")
}

func TestReplaceSessionSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	findings := []SecretFinding{
		{SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 5, MatchEnd: 25,
			MatchIndex: 0, RedactedMatch: "AKIA…MPLE"},
		{SessionID: "s1", RuleName: "high-entropy-assignment", Confidence: "candidate",
			LocationKind: "tool_result", MessageOrdinal: 1, CallIndex: Ptr(0),
			MatchStart: 0, MatchEnd: 24, MatchIndex: 0, RedactedMatch: "…abcd"},
	}
	require.NoError(t,
		d.ReplaceSessionSecretFindings("s1", findings, 1, "rulesv1"),
		"ReplaceSessionSecretFindings")
	got, err := d.SessionSecretFindings(context.Background(), "s1")
	require.NoError(t, err, "read")
	require.Len(t, got, 2)
	// A message finding has no call/event index (NULL round-trips to nil); a
	// tool finding with CallIndex 0 must read back as a non-nil pointer to 0,
	// not collapse to nil (the NULL-vs-zero trap for nullable ints).
	assert.Nil(t, got[0].CallIndex, "message finding CallIndex")
	assert.Nil(t, got[0].EventIndex, "message finding EventIndex")
	require.NotNil(t, got[1].CallIndex, "tool finding CallIndex")
	assert.Equal(t, 0, *got[1].CallIndex, "tool finding CallIndex")
	require.NoError(t,
		d.ReplaceSessionSecretFindings("s1", findings[:1], 1, "rulesv1"),
		"re-replace")
	got, _ = d.SessionSecretFindings(context.Background(), "s1")
	require.Len(t, got, 1, "replace not idempotent")
}

// TestReplaceSessionMessagesResetsSecretState verifies that replacing a
// session's messages clears stale secret findings and resets the scan-state
// columns. A session re-imported with different content (the importer calls
// ReplaceSessionMessages directly) must not keep an obsolete leak count, and
// must be re-scanned by `secrets scan --backfill`, which skips sessions already
// at the current rules version.
func TestReplaceSessionMessagesResetsSecretState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "key AKIA7QHWN2DKR4FYPLJM"},
	}), "seed messages")
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
	}}
	require.NoError(t,
		d.ReplaceSessionSecretFindings("s1", findings, 1, "rulesvX"),
		"seed findings")

	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "nothing secret here"},
	}), "replace messages")

	got, err := d.SessionSecretFindings(ctx, "s1")
	require.NoError(t, err, "read findings")
	assert.Empty(t, got, "stale findings survived message replace")
	var leak int
	var ver string
	err = d.getReader().QueryRow(
		"SELECT secret_leak_count, secrets_rules_version FROM sessions WHERE id = 's1'",
	).Scan(&leak, &ver)
	require.NoError(t, err, "read scan state")
	assert.Equal(t, 0, leak, "secret_leak_count after content replace")
	assert.Empty(t, ver, "secrets_rules_version (forces backfill rescan)")
}

func TestReplaceSessionMessagesPreservesSecretStateWhenTranscriptIsUnchanged(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	messages := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "user", Content: "same content",
	}}
	require.NoError(t, d.ReplaceSessionMessages("s1", messages), "seed messages")
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "test-rule", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 4,
		MatchIndex: 0, RedactedMatch: "same",
	}}
	require.NoError(t,
		d.ReplaceSessionSecretFindings("s1", findings, 1, "rules-v1"),
		"seed findings",
	)

	require.NoError(t, d.ReplaceSessionMessages("s1", messages), "no-op replace")

	got, err := d.SessionSecretFindings(ctx, "s1")
	require.NoError(t, err, "read findings")
	require.Len(t, got, 1, "no-op replace must preserve current findings")
	assert.Equal(t, "test-rule", got[0].RuleName)
	var leak int
	var version string
	err = d.getReader().QueryRow(`
		SELECT secret_leak_count, secrets_rules_version
		FROM sessions WHERE id = 's1'`,
	).Scan(&leak, &version)
	require.NoError(t, err, "read scan state")
	assert.Equal(t, 1, leak)
	assert.Equal(t, "rules-v1", version)
}

func TestOrphanCopyPreservesSecretFindings(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB,
		userMsg("s1", 0, "hello"),
		asstMsg("s1", 1, "reply"),
	)
	err = srcDB.ReplaceSessionSecretFindings("s1", []SecretFinding{
		{
			SessionID:      "s1",
			RuleName:       "aws-access-key",
			Confidence:     "definite",
			LocationKind:   "message",
			MessageOrdinal: 0,
			MatchStart:     0,
			MatchEnd:       20,
			MatchIndex:     0,
			RedactedMatch:  "AKIA…MPLE",
		},
	}, 1, "rulesv1")
	require.NoError(t, err, "ReplaceSessionSecretFindings src")
	// Stamp a distinctive past created_at so the copy can be shown to preserve
	// it rather than regenerating from the destination default.
	_, err = srcDB.getWriter().Exec(
		"UPDATE secret_findings SET created_at = ? WHERE session_id = 's1'",
		"2020-01-01T00:00:00.000Z")
	require.NoError(t, err, "stamp created_at")
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected 1 orphaned session")

	s, err := dstDB.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, s, "session s1 not found in dst")
	assert.Equal(t, 1, s.SecretLeakCount, "SecretLeakCount")

	findings, err := dstDB.SessionSecretFindings(ctx, "s1")
	require.NoError(t, err, "SessionSecretFindings")
	require.Len(t, findings, 1)
	f := findings[0]
	assert.Equal(t, "aws-access-key", f.RuleName, "RuleName")
	assert.Equal(t, "message", f.LocationKind, "LocationKind")
	assert.Equal(t, "AKIA…MPLE", f.RedactedMatch, "RedactedMatch")

	var createdAt string
	require.NoError(t, dstDB.getReader().QueryRow(
		"SELECT created_at FROM secret_findings WHERE session_id = 's1'",
	).Scan(&createdAt), "query copied created_at")
	assert.Equal(t, "2020-01-01T00:00:00.000Z", createdAt, "preserved created_at")
}

func TestListSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "alpha", func(s *Session) { s.Agent = "claude" })
	insertSession(t, d, "s2", "beta", func(s *Session) { s.Agent = "codex" })
	_ = d.ReplaceSessionSecretFindings("s1", []SecretFinding{
		{SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
			MatchIndex: 0, RedactedMatch: "AKIA…MPLE"},
	}, 1, "v1")
	_ = d.ReplaceSessionSecretFindings("s2", []SecretFinding{
		{SessionID: "s2", RuleName: "jwt", Confidence: "candidate",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 10,
			MatchIndex: 0, RedactedMatch: "eyJ…"},
	}, 0, "v1")

	all, err := d.ListSecretFindings(context.Background(), SecretFindingFilter{Limit: 50})
	require.NoError(t, err, "ListSecretFindings")
	require.Len(t, all.Findings, 2)
	// project filter
	alpha, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{Project: "alpha", Limit: 50})
	require.Len(t, alpha.Findings, 1, "project filter")
	assert.Equal(t, "s1", alpha.Findings[0].SessionID, "project filter")
	// confidence filter
	def, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{Confidence: "definite", Limit: 50})
	require.Len(t, def.Findings, 1, "confidence filter")
	assert.Equal(t, "aws-access-key", def.Findings[0].RuleName, "confidence filter")
	current, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{RulesVersions: []string{"v1"}, Limit: 50})
	assert.Len(t, current.Findings, 2, "rules version filter current")
	stale, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{RulesVersions: []string{"v2"}, Limit: 50})
	assert.Empty(t, stale.Findings, "rules version filter stale")
}

// TestListSecretFindingsPagination walks pages with a small limit and asserts
// every finding is seen exactly once (no OFFSET drop/duplicate at the page
// boundary) and NextCursor advances then stops.
func TestListSecretFindingsPagination(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj", func(s *Session) { s.Agent = "claude" })
	var findings []SecretFinding
	for i := range 5 {
		findings = append(findings, SecretFinding{
			SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: i * 10, MatchEnd: i*10 + 5, MatchIndex: i,
			RedactedMatch: "AKIA…",
		})
	}
	require.NoError(t,
		d.ReplaceSessionSecretFindings("s1", findings, 5, "v1"),
		"ReplaceSessionSecretFindings")
	seen := map[int]int{}
	cursor, pages := 0, 0
	for {
		page, err := d.ListSecretFindings(context.Background(),
			SecretFindingFilter{Limit: 2, Cursor: cursor})
		require.NoError(t, err, "page at cursor %d", cursor)
		for _, f := range page.Findings {
			seen[f.MatchStart]++
		}
		pages++
		require.LessOrEqual(t, pages, 10, "pagination did not terminate")
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, seen, 5, "distinct findings across pages")
	for start, n := range seen {
		assert.Equal(t, 1, n, "finding at MatchStart=%d seen %d times", start, n)
	}
}

// TestListSecretFindingsDateFilter covers the COALESCE(NULLIF(...)) fallback:
// a session with an empty-string started_at must filter on created_at rather
// than being silently dropped (date(”) is NULL).
func TestListSecretFindingsDateFilter(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "dated", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-03-15T10:00:00Z")
	})
	insertSession(t, d, "empty", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("")
	})
	// Pin the empty session's created_at so the assertions are deterministic.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET created_at = ? WHERE id = 'empty'",
		"2026-05-10T00:00:00Z")
	require.NoError(t, err, "stamp created_at")
	for _, id := range []string{"dated", "empty"} {
		require.NoError(t, d.ReplaceSessionSecretFindings(id, []SecretFinding{{
			SessionID: id, RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0,
			MatchEnd: 20, MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
		}}, 1, "v1"), "ReplaceSessionSecretFindings %s", id)
	}
	// Wide range includes both; the empty started_at falls back to created_at.
	wide, err := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{DateFrom: "2000-01-01", Limit: 50})
	require.NoError(t, err, "wide")
	assert.Len(t, wide.Findings, 2,
		"DateFrom 2000-01-01 (empty must fall back to created_at)")
	// Range covering only the dated session (empty's created_at is in May).
	mar, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{DateFrom: "2026-03-01", DateTo: "2026-03-31", Limit: 50})
	require.Len(t, mar.Findings, 1, "March range")
	assert.Equal(t, "dated", mar.Findings[0].SessionID, "March range")
}

// TestUpdateSessionSignalsPreservesSecretColumns verifies a signals-only
// recompute (whose SessionSignalUpdate carries zero secret fields) does not
// reset secret_leak_count/secrets_rules_version or drop findings: the
// secret columns are owned solely by the findings replacement path.
func TestUpdateSessionSignalsPreservesSecretColumns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	require.NoError(t, d.ReplaceSessionSecretFindings("s1", []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
	}}, 1, "rulesv1"), "ReplaceSessionSecretFindings")
	// A signals-only recompute carries zero secret fields; it must not reset
	// the secret summary while findings still exist.
	require.NoError(t, d.UpdateSessionSignals(
		"s1", SessionSignalUpdate{Outcome: "success"},
	), "UpdateSessionSignals")
	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, s, "GetSession")
	assert.Equal(t, 1, s.SecretLeakCount, "SecretLeakCount preserved")
	findings, err := d.SessionSecretFindings(ctx, "s1")
	require.NoError(t, err, "SessionSecretFindings")
	assert.Len(t, findings, 1, "findings preserved")
	var rv string
	err = d.getReader().QueryRow(
		"SELECT secrets_rules_version FROM sessions WHERE id = 's1'",
	).Scan(&rv)
	require.NoError(t, err, "query secrets_rules_version")
	assert.Equal(t, "rulesv1", rv, "secrets_rules_version preserved")
}

func TestSecretScanCandidates(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "scanned", "proj", func(s *Session) { s.MessageCount = 1 })
	insertSession(t, d, "stale", "proj", func(s *Session) { s.MessageCount = 1 })
	insertSession(t, d, "empty", "proj", func(s *Session) { s.MessageCount = 0 })
	// Mark "scanned" at the current version; the others stay at "".
	require.NoError(t, d.ReplaceSessionSecretFindings("scanned", nil, 0, "vCur"),
		"mark scanned")
	ctx := context.Background()
	stale, err := d.SecretScanCandidates(ctx, SecretScanCandidateFilter{
		CurrentVersion: "vCur", OnlyStale: true,
	})
	require.NoError(t, err, "SecretScanCandidates stale")
	// Only "stale" qualifies: "scanned" is current, "empty" has no messages.
	require.Equal(t, []string{"stale"}, stale, "OnlyStale candidates")
	all, err := d.SecretScanCandidates(ctx, SecretScanCandidateFilter{
		CurrentVersion: "vCur", OnlyStale: false,
	})
	require.NoError(t, err, "SecretScanCandidates forced")
	// Forced rescan ignores version but still requires messages.
	require.Len(t, all, 2, "forced candidates (scanned+stale)")
}

func TestSecretFindingSource(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj", func(s *Session) { s.Agent = "claude" })
	msgs := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content:   "key AKIA7QHWN2DKR4FYPLJM here",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{
			{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu0",
				InputJSON:     `{"command":"printenv"}`,
				ResultContent: "AWS_SECRET=topsecretvalue123",
			},
			{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
				ResultEvents: []ToolResultEvent{{
					Source: "stdout", Status: "ok",
					Content: "event-secret-value", EventIndex: 0,
				}},
			},
		},
	}}
	require.NoError(t, d.ReplaceSessionMessages("s1", msgs),
		"ReplaceSessionMessages")
	ctx := context.Background()
	cases := []struct {
		name string
		f    SecretFinding
		want string
		ok   bool
	}{
		{"message", SecretFinding{SessionID: "s1", LocationKind: "message",
			MessageOrdinal: 0}, "key AKIA7QHWN2DKR4FYPLJM here", true},
		{"tool_input", SecretFinding{SessionID: "s1", LocationKind: "tool_input",
			MessageOrdinal: 0, CallIndex: Ptr(0)}, `{"command":"printenv"}`, true},
		{"tool_result", SecretFinding{SessionID: "s1", LocationKind: "tool_result",
			MessageOrdinal: 0, CallIndex: Ptr(0)}, "AWS_SECRET=topsecretvalue123", true},
		{"tool_result_event", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result_event", MessageOrdinal: 0,
			CallIndex: Ptr(1), EventIndex: Ptr(0)}, "event-secret-value", true},
		{"missing ordinal", SecretFinding{SessionID: "s1", LocationKind: "message",
			MessageOrdinal: 99}, "", false},
		{"call index out of range", SecretFinding{SessionID: "s1",
			LocationKind: "tool_input", MessageOrdinal: 0, CallIndex: Ptr(9)}, "", false},
		{"event index not found", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result_event", MessageOrdinal: 0,
			CallIndex: Ptr(1), EventIndex: Ptr(99)}, "", false},
		// Call 1 has result events, so the scanner used those, not
		// result_content; a stale tool_result finding there must not resolve.
		{"tool_result skipped when events present", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result", MessageOrdinal: 0, CallIndex: Ptr(1)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := d.SecretFindingSource(ctx, tc.f)
			require.NoError(t, err, "SecretFindingSource")
			assert.Equal(t, tc.ok, ok, "ok")
			assert.Equal(t, tc.want, got, "got")
		})
	}
}

// TestOrphanCopyPreservesToolCallIndex verifies that a secret finding whose
// call_index points at a NON-zero (second) tool call resolves to the correct
// content after CopyOrphanedDataFrom. Without ORDER BY otc.id in the orphan
// tool_call INSERT, SQLite may assign new ids in an arbitrary order, causing
// call_index=1 to resolve to the wrong tool call.
func TestOrphanCopyPreservesToolCallIndex(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// --- Source DB: one session, one assistant message, two tool calls ---
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")

	insertSession(t, srcDB, "s1", "proj")
	msgs := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content:   "ran two tools",
		Timestamp: "2026-01-01T00:00:00Z",
		ToolCalls: []ToolCall{
			{
				ToolName:      "Bash",
				Category:      "Bash",
				ToolUseID:     "tu0",
				InputJSON:     `{"command":"echo ZERO-AKIA"}`,
				ResultContent: "ZERO-AKIA",
			},
			{
				ToolName:      "Bash",
				Category:      "Bash",
				ToolUseID:     "tu1",
				InputJSON:     `{"command":"echo secret"}`,
				ResultContent: "AKIA7QHWN2DKR4FYPLJM",
			},
		},
	}}
	require.NoError(t, srcDB.ReplaceSessionMessages("s1", msgs),
		"ReplaceSessionMessages")

	// Finding at call_index=1 (second tool call), tool_result location.
	finding := SecretFinding{
		SessionID:      "s1",
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "tool_result",
		MessageOrdinal: 0,
		CallIndex:      Ptr(1),
		MatchStart:     0,
		MatchEnd:       20,
		MatchIndex:     0,
		RedactedMatch:  "AKIA…MPLE",
	}
	require.NoError(t, srcDB.ReplaceSessionSecretFindings(
		"s1", []SecretFinding{finding}, 1, "rulesv1",
	), "ReplaceSessionSecretFindings")
	srcDB.Close()

	// --- Destination DB: copy orphaned data ---
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected 1 orphaned session")

	// --- Verify: call_index=1 must resolve to the SECOND tool call ---
	findings, err := dstDB.SessionSecretFindings(ctx, "s1")
	require.NoError(t, err, "SessionSecretFindings")
	require.Len(t, findings, 1)
	f := findings[0]
	require.NotNil(t, f.CallIndex, "CallIndex")
	require.Equal(t, 1, *f.CallIndex, "CallIndex")

	got, ok, err := dstDB.SecretFindingSource(ctx, f)
	require.NoError(t, err, "SecretFindingSource")
	require.True(t, ok, "SecretFindingSource returned ok=false; "+
		"tool call order corrupted during orphan copy")
	const wantContent = "AKIA7QHWN2DKR4FYPLJM"
	assert.Equal(t, wantContent, got,
		"SecretFindingSource (call_index=1 resolved to wrong tool call; "+
			"ORDER BY otc.id may be missing from orphan copy)")
}
