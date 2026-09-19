//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// seedSecretFindingsSession inserts a session, optional messages, and findings.
func seedSecretFindingsSession(
	t *testing.T, store *Store,
	sid, project, agent string,
	startedAt string,
	msgs []struct {
		ordinal int
		role    string
		content string
	},
	findings []db.SecretFinding,
) {
	t.Helper()
	pg := store.DB()

	_, err := pg.Exec(`DELETE FROM secret_findings WHERE session_id = $1`, sid)
	require.NoError(t, err, "delete secret_findings")
	_, err = pg.Exec(`DELETE FROM messages WHERE session_id = $1`, sid)
	require.NoError(t, err, "delete messages")
	_, err = pg.Exec(`DELETE FROM sessions WHERE id = $1`, sid)
	require.NoError(t, err, "delete session")

	var saVal interface{} = nil
	if startedAt != "" {
		saVal = startedAt
	}
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count,
			 secret_leak_count)
		VALUES ($1, 'test-machine', $2, $3, 'test',
			$4::timestamptz, $5, 0, $6)`,
		sid, project, agent, saVal,
		len(msgs)+1, len(findings),
	)
	require.NoError(t, err, "insert session %s", sid)

	for _, m := range msgs {
		_, err := pg.Exec(`
			INSERT INTO messages
				(session_id, ordinal, role, content, content_length)
			VALUES ($1, $2, $3, $4, $5)`,
			sid, m.ordinal, m.role, m.content, len(m.content),
		)
		require.NoError(t, err, "insert message ord=%d", m.ordinal)
	}

	for _, f := range findings {
		_, err := pg.Exec(`
			INSERT INTO secret_findings
				(session_id, rule_name, confidence, location_kind,
				 message_ordinal, call_index, event_index,
				 match_start, match_end, match_index,
				 redacted_match, rules_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			f.SessionID, f.RuleName, f.Confidence, f.LocationKind,
			f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
		)
		require.NoError(t, err, "insert finding")
	}
}

func TestPGListSecretFindings(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()

	seedSecretFindingsSession(t, store,
		"sf-s1", "alpha-proj", "claude-code",
		"2026-03-12T10:00:00Z",
		nil,
		[]db.SecretFinding{
			{
				SessionID: "sf-s1", RuleName: "aws-access-key",
				Confidence: "definite", LocationKind: "message",
				MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
				MatchIndex: 0, RedactedMatch: "AKIA…", RulesVersion: "v1",
			},
		},
	)
	seedSecretFindingsSession(t, store,
		"sf-s2", "beta-proj", "codex",
		"2026-03-13T10:00:00Z",
		nil,
		[]db.SecretFinding{
			{
				SessionID: "sf-s2", RuleName: "jwt",
				Confidence: "candidate", LocationKind: "message",
				MessageOrdinal: 0, MatchStart: 0, MatchEnd: 10,
				MatchIndex: 0, RedactedMatch: "eyJ…", RulesVersion: "v1",
			},
		},
	)

	// All findings.
	all, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{Limit: 50})
	require.NoError(t, err, "ListSecretFindings all")
	// At least 2 findings (may be more from other tests).
	foundS1, foundS2 := false, false
	for _, f := range all.Findings {
		if f.SessionID == "sf-s1" {
			foundS1 = true
			assert.Equal(t, "alpha-proj", f.Project, "sf-s1 Project")
			assert.Equal(t, "claude-code", f.Agent, "sf-s1 Agent")
		}
		if f.SessionID == "sf-s2" {
			foundS2 = true
			assert.Equal(t, "beta-proj", f.Project, "sf-s2 Project")
			assert.Equal(t, "codex", f.Agent, "sf-s2 Agent")
		}
	}
	assert.True(t, foundS1 && foundS2,
		"foundS1=%v foundS2=%v; both must be present", foundS1, foundS2)

	// Project filter.
	alpha, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Project: "alpha-proj", Limit: 50})
	require.NoError(t, err, "ListSecretFindings project filter")
	for _, f := range alpha.Findings {
		assert.Equal(t, "alpha-proj", f.Project, "project filter leak")
	}
	found := false
	for _, f := range alpha.Findings {
		if f.SessionID == "sf-s1" {
			found = true
		}
	}
	assert.True(t, found, "sf-s1 not found in project filter results")

	// Agent filter.
	codex, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Agent: "codex", Limit: 50})
	require.NoError(t, err, "ListSecretFindings agent filter")
	for _, f := range codex.Findings {
		assert.Equal(t, "codex", f.Agent, "agent filter leak")
	}

	// Confidence filter: definite.
	def, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Confidence: "definite", Limit: 50})
	require.NoError(t, err, "ListSecretFindings confidence filter")
	for _, f := range def.Findings {
		assert.Equal(t, "definite", f.Confidence, "confidence filter leak")
	}

	// Rules-version filter.
	current, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{RulesVersions: []string{"v1"}, Limit: 50})
	require.NoError(t, err, "ListSecretFindings rules version filter")
	foundS1, foundS2 = false, false
	for _, f := range current.Findings {
		assert.Equal(t, "v1", f.RulesVersion, "rules version filter leak")
		if f.SessionID == "sf-s1" {
			foundS1 = true
		}
		if f.SessionID == "sf-s2" {
			foundS2 = true
		}
	}
	assert.True(t, foundS1 && foundS2,
		"rules version filter foundS1=%v foundS2=%v", foundS1, foundS2)
	stale, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{RulesVersions: []string{"v2"}, Limit: 50})
	require.NoError(t, err, "ListSecretFindings stale rules version filter")
	for _, f := range stale.Findings {
		assert.NotContains(t, []string{"sf-s1", "sf-s2"}, f.SessionID,
			"stale rules version filter returned %s", f.SessionID)
	}

	// Rule filter.
	jwt, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Rule: "jwt", Limit: 50})
	require.NoError(t, err, "ListSecretFindings rule filter")
	for _, f := range jwt.Findings {
		assert.Equal(t, "jwt", f.RuleName, "rule filter leak")
	}
}

func TestPGListSecretFindingsPagination(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()

	// Seed 5 findings on a single session with a distinctive rule name.
	findings := make([]db.SecretFinding, 5)
	for i := range findings {
		findings[i] = db.SecretFinding{
			SessionID: "sf-page-s1", RuleName: "pg-pagination-test",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: i * 10, MatchEnd: i*10 + 5,
			MatchIndex: i, RedactedMatch: "X…", RulesVersion: "v1",
		}
	}
	seedSecretFindingsSession(t, store,
		"sf-page-s1", "page-proj", "claude-code",
		"2026-04-01T10:00:00Z",
		nil, findings,
	)

	seen := map[int]int{}
	cursor, pages := 0, 0
	for {
		page, err := store.ListSecretFindings(ctx,
			db.SecretFindingFilter{
				Rule:  "pg-pagination-test",
				Limit: 2, Cursor: cursor,
			})
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
		assert.Equal(t, 1, n,
			"finding at MatchStart=%d seen %d times", start, n)
	}
}

func TestPGListSecretFindingsDateFilter(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()

	seedSecretFindingsSession(t, store,
		"sf-date-mar", "df-proj", "claude-code",
		"2026-03-15T10:00:00Z",
		nil,
		[]db.SecretFinding{{
			SessionID: "sf-date-mar", RuleName: "df-rule",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5,
			MatchIndex: 0, RedactedMatch: "X", RulesVersion: "v1",
		}},
	)
	seedSecretFindingsSession(t, store,
		"sf-date-may", "df-proj", "claude-code",
		"2026-05-10T10:00:00Z",
		nil,
		[]db.SecretFinding{{
			SessionID: "sf-date-may", RuleName: "df-rule",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5,
			MatchIndex: 0, RedactedMatch: "Y", RulesVersion: "v1",
		}},
	)

	// Wide range includes both.
	wide, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{
			Rule: "df-rule", DateFrom: "2000-01-01", Limit: 50,
		})
	require.NoError(t, err, "wide")
	require.Len(t, wide.Findings, 2)

	// March-only range.
	mar, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{
			Rule:     "df-rule",
			DateFrom: "2026-03-01", DateTo: "2026-03-31",
			Limit: 50,
		})
	require.NoError(t, err, "march")
	require.Len(t, mar.Findings, 1)
	assert.Equal(t, "sf-date-mar", mar.Findings[0].SessionID)
}

func TestPGSecretFindingSource(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pg := store.DB()
	ctx := context.Background()

	// Clean up prior runs. The sessions table uses "id" not "session_id".
	sid := "sf-src-s1"
	for _, tbl := range []string{
		"secret_findings", "tool_result_events",
		"tool_calls", "messages",
	} {
		_, err := pg.Exec("DELETE FROM "+tbl+" WHERE session_id = $1", sid)
		require.NoError(t, err, "cleanup %s", tbl)
	}
	_, err = pg.Exec("DELETE FROM sessions WHERE id = $1", sid)
	require.NoError(t, err, "cleanup sessions")

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES ($1, 'test-machine', 'src-proj', 'claude-code',
			'test', '2026-05-01T00:00:00Z'::timestamptz, 1, 0)`,
		sid,
	)
	require.NoError(t, err, "insert session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, content_length)
		VALUES ($1, 0, 'assistant',
			'key AKIA7QHWN2DKR4FYPLJM here', 28)`,
		sid,
	)
	require.NoError(t, err, "insert message")
	// Insert a tool_call at call_index=0 with result_content.
	_, err = pg.Exec(`
		INSERT INTO tool_calls
			(session_id, message_ordinal, call_index, tool_name, category,
			 tool_use_id, input_json, result_content)
		VALUES ($1, 0, 0, 'Bash', 'Bash', 'tu0',
			'{"command":"printenv"}', 'AWS_SECRET=topsecretvalue123')`,
		sid,
	)
	require.NoError(t, err, "insert tool_call")
	// Insert a tool_call at call_index=1 with a result event.
	_, err = pg.Exec(`
		INSERT INTO tool_calls
			(session_id, message_ordinal, call_index, tool_name, category,
			 tool_use_id)
		VALUES ($1, 0, 1, 'Bash', 'Bash', 'tu1')`,
		sid,
	)
	require.NoError(t, err, "insert tool_call 2")
	_, err = pg.Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, source, status, content, event_index)
		VALUES ($1, 0, 1, 'tu1', 'stdout', 'ok',
			'event-secret-value', 0)`,
		sid,
	)
	require.NoError(t, err, "insert tool_result_event")

	cases := []struct {
		name string
		f    db.SecretFinding
		want string
		ok   bool
	}{
		{
			"message",
			db.SecretFinding{
				SessionID: sid, LocationKind: "message",
				MessageOrdinal: 0,
			},
			"key AKIA7QHWN2DKR4FYPLJM here", true,
		},
		{
			"tool_input",
			db.SecretFinding{
				SessionID: sid, LocationKind: "tool_input",
				MessageOrdinal: 0, CallIndex: ptr(0),
			},
			`{"command":"printenv"}`, true,
		},
		{
			"tool_result",
			db.SecretFinding{
				SessionID: sid, LocationKind: "tool_result",
				MessageOrdinal: 0, CallIndex: ptr(0),
			},
			"AWS_SECRET=topsecretvalue123", true,
		},
		{
			"tool_result_event",
			db.SecretFinding{
				SessionID: sid, LocationKind: "tool_result_event",
				MessageOrdinal: 0, CallIndex: ptr(1), EventIndex: ptr(0),
			},
			"event-secret-value", true,
		},
		{
			"missing ordinal",
			db.SecretFinding{
				SessionID: sid, LocationKind: "message",
				MessageOrdinal: 99,
			},
			"", false,
		},
		{
			"call index out of range",
			db.SecretFinding{
				SessionID: sid, LocationKind: "tool_input",
				MessageOrdinal: 0, CallIndex: ptr(9),
			},
			"", false,
		},
		{
			"tool_result skipped when events present",
			db.SecretFinding{
				SessionID: sid, LocationKind: "tool_result",
				MessageOrdinal: 0, CallIndex: ptr(1),
			},
			"", false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := store.SecretFindingSource(ctx, tc.f)
			require.NoError(t, err, "SecretFindingSource")
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func ptr(i int) *int { return &i }
