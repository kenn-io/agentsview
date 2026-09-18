//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sessionVersionProbeDriver struct{}

type sessionVersionProbeConn struct{}

type sessionVersionProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

var sessionVersionProbeRegisterOnce sync.Once

func newSessionVersionProbeStore(t *testing.T) *Store {
	t.Helper()
	sessionVersionProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_session_version_probe", sessionVersionProbeDriver{})
	})
	duck, err := sql.Open("agentsview_session_version_probe", t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, duck.Close()) })
	return &Store{
		duck:           duck,
		connectionKind: duckDBQuackClientConnection,
	}
}

func (sessionVersionProbeDriver) Open(string) (driver.Conn, error) {
	return sessionVersionProbeConn{}, nil
}

func (sessionVersionProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (sessionVersionProbeConn) Close() error { return nil }

func (sessionVersionProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (sessionVersionProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	if !strings.Contains(query, quackAttachmentName+".query(?)") {
		return nil, errors.New("direct session version query should not be used")
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("remote query got %d args, want 1", len(args))
	}
	sqlText, ok := args[0].Value.(string)
	if !ok {
		return nil, fmt.Errorf("remote query arg has type %T", args[0].Value)
	}
	if !strings.Contains(sqlText, "FROM sessions WHERE id") {
		return nil, fmt.Errorf("unexpected remote query: %s", sqlText)
	}
	return &sessionVersionProbeRows{
		columns: []string{
			"message_count", "file_mtime", "file_hash", "updated_at",
		},
		values: [][]driver.Value{{
			int64(7), int64(123), "hash",
			"2026-01-10T00:00:00Z",
		}},
	}, nil
}

func (r *sessionVersionProbeRows) Columns() []string {
	return r.columns
}

func (r *sessionVersionProbeRows) Close() error { return nil }

func (r *sessionVersionProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestDecodeCursorClearsLegacyTotal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := &Store{}
	data, err := json.Marshal(db.SessionCursor{
		EndedAt: "2026-01-10T00:00:00.000Z",
		ID:      "legacy-cursor",
		Total:   42,
	})
	require.NoError(err)
	raw := base64.RawURLEncoding.EncodeToString(data)

	got, err := store.DecodeCursor(raw)
	require.NoError(err)

	assert.Equal("legacy-cursor", got.ID)
	assert.Equal(0, got.Total)
}

func TestQuackStoreGetSessionVersionUsesRemoteQuery(t *testing.T) {
	store := newSessionVersionProbeStore(t)

	count, marker, ok := store.GetSessionVersion("quoted ' session")

	require.True(t, ok)
	assert.Equal(t, 7, count)
	assert.Equal(t,
		db.SessionVersionMarker(
			"123", "hash", "2026-01-10T00:00:00Z",
		),
		marker,
	)
}

func TestStoreReadsSessionsMessagesAndMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)

	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 2)
	assert.Equal(2, page.Total)
	assert.Equal(fixture.betaID, page.Sessions[0].ID)
	assert.Equal(fixture.alphaID, page.Sessions[1].ID)

	sess, err := store.GetSession(ctx, fixture.alphaID)
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal("alpha", sess.Project)
	assert.Equal(2, sess.MessageCount)

	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Equal("alpha first", msgs[0].Content)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("search", msgs[1].ToolCalls[0].ToolName)
	require.Len(msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal("duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(err)
	assert.Equal(2, stats.SessionCount)
	assert.Equal(3, stats.MessageCount)
	assert.Equal(2, stats.ProjectCount)
	assert.Equal(1, stats.MachineCount)
	require.NotNil(stats.EarliestSession)

	projects, err := store.GetProjects(ctx, false, false)
	require.NoError(err)
	assert.Equal([]db.ProjectInfo{
		{Name: "alpha", SessionCount: 1},
		{Name: "beta", SessionCount: 1},
	}, projects)

	agents, err := store.GetAgents(ctx, false, false)
	require.NoError(err)
	assert.Equal([]db.AgentInfo{{Name: "claude", SessionCount: 2}}, agents)

	machines, err := store.GetMachines(ctx, false, false)
	require.NoError(err)
	assert.Equal([]string{"test-machine"}, machines)
}

func TestStoreGetStatsPreservesRootAndScopeFilters(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	duck := syncer.DB()
	store := NewStoreFromDB(duck)

	insertSession := func(
		id, project, relationship, ts string,
		messageCount, userMessageCount int,
		automated bool,
	) {
		t.Helper()
		_, err := duck.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project, machine, agent, message_count,
				user_message_count, relationship_type, is_automated,
				started_at, created_at
			) VALUES (
				?, ?, 'stats-machine', 'claude', ?, ?, ?, ?,
				CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP)
			)`,
			id, project, messageCount, userMessageCount, relationship,
			automated, ts, ts,
		)
		require.NoError(err)
	}

	insertSession(
		"stats-fork", "fork", "fork", "2025-12-26 00:00:00",
		1, 2, false,
	)
	insertSession(
		"stats-subagent", "child", "subagent", "2025-12-27 00:00:00",
		1, 2, false,
	)
	insertSession(
		"stats-empty", "empty", "root", "2025-12-28 00:00:00",
		0, 2, false,
	)
	insertSession(
		"stats-deleted", "deleted", "root", "2025-12-29 00:00:00",
		1, 2, false,
	)
	insertSession(
		"stats-one-shot", "beta", "root", "2025-12-30 00:00:00",
		1, 1, false,
	)
	insertSession(
		"stats-automated", "bot", "root", "2025-12-31 00:00:00",
		1, 1, true,
	)
	insertSession(
		"stats-human", "alpha", "root", "2026-01-01 00:00:00",
		2, 2, false,
	)
	_, err := duck.ExecContext(ctx,
		`UPDATE sessions SET deleted_at = CAST(? AS TIMESTAMP) WHERE id = ?`,
		"2026-01-02 00:00:00", "stats-deleted",
	)
	require.NoError(err)

	assertStats := func(
		name string,
		excludeOneShot, excludeAutomated bool,
		wantSessions, wantMessages, wantProjects int,
		wantEarliest string,
	) {
		t.Helper()
		stats, err := store.GetStats(ctx, excludeOneShot, excludeAutomated)
		require.NoError(err, name)
		assert.Equal(t, wantSessions, stats.SessionCount, name)
		assert.Equal(t, wantMessages, stats.MessageCount, name)
		assert.Equal(t, wantProjects, stats.ProjectCount, name)
		assert.Equal(t, 1, stats.MachineCount, name)
		require.NotNil(stats.EarliestSession, name)
		assert.Equal(t, wantEarliest, *stats.EarliestSession, name)
	}

	assertStats(
		"include all root sessions", false, false, 3, 4, 3,
		"2025-12-30T00:00:00Z",
	)
	assertStats(
		"exclude one-shot keeps automated", true, false, 2, 3, 2,
		"2025-12-31T00:00:00Z",
	)
	assertStats(
		"exclude automated keeps human one-shot", false, true, 2, 3, 2,
		"2025-12-30T00:00:00Z",
	)
	assertStats(
		"exclude one-shot and automated", true, true, 1, 2, 1,
		"2026-01-01T00:00:00Z",
	)

	labels, err := store.GetActiveProjectLabels(ctx)
	require.NoError(err)
	assert.Equal(t, []string{
		"alpha", "beta", "bot", "child", "empty", "fork",
	}, labels)
}

func TestStoreListTrashedSessionsOrdersNewestFirstAndCapsAt500(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	duck := syncer.DB()
	store := NewStoreFromDB(duck)

	_, err := duck.ExecContext(ctx, `
		INSERT INTO sessions (id, project, deleted_at)
		SELECT 'trash-' || i, 'trash-parity',
			TIMESTAMP '2026-01-01 00:00:00' + INTERVAL (i) SECOND
		FROM range(600) t(i)`)
	require.NoError(err)
	_, err = duck.ExecContext(ctx,
		`INSERT INTO sessions (id, project) VALUES ('active', 'trash-parity')`)
	require.NoError(err)

	trashed, err := store.ListTrashedSessions(ctx)
	require.NoError(err)
	// Same cap and ordering as the SQLite and PG stores: newest 500 by
	// deleted_at, excluding active rows.
	require.Len(trashed, 500)
	assert.Equal("trash-599", trashed[0].ID)
	assert.Equal("trash-100", trashed[499].ID)
	for i := 1; i < len(trashed); i++ {
		require.NotNil(trashed[i].DeletedAt)
		require.Less(*trashed[i].DeletedAt, *trashed[i-1].DeletedAt,
			"trash must be ordered newest-first at index %d", i)
	}
}

func TestStoreMessageIDJoinsAreSessionScoped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)
	insertOtherMachineDuckSession(t, store.duck)

	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(err)
	require.Len(msgs, 2)
	assert.Empty(msgs[0].ToolCalls)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("search", msgs[1].ToolCalls[0].ToolName)

	content, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "wrong-session-tool",
		Sources:        []string{"tool_input"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(content.Matches, 1)
	assert.Equal("other-session", content.Matches[0].SessionID)
	assert.Equal(0, content.Matches[0].Ordinal)

	pins, err := store.ListPinnedMessages(ctx, "", "alpha")
	require.NoError(err)
	foundOtherPin := false
	for _, pin := range pins {
		if pin.SessionID == "other-session" {
			foundOtherPin = true
			require.NotNil(pin.Content)
			assert.Equal("from other machine", *pin.Content)
		}
	}
	assert.True(foundOtherPin)
}

func TestStoreSearchesMessagesContentAndSecrets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)

	search, err := store.Search(ctx, db.SearchFilter{Query: "secret token", Limit: 10})
	require.NoError(err)
	require.Len(search.Results, 1)
	assert.Equal(fixture.alphaID, search.Results[0].SessionID)
	assert.Equal(1, search.Results[0].Ordinal)

	ordinals, err := store.SearchSession(ctx, fixture.alphaID, "duck result")
	require.NoError(err)
	assert.Equal([]int{1}, ordinals)

	content, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck result",
		Sources:        []string{"tool_result"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(content.Matches, 1)
	assert.Equal("tool_result", content.Matches[0].Location)
	assert.Equal(fixture.alphaID, content.Matches[0].SessionID)

	excluded, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:           "duck result",
		Sources:           []string{"tool_result"},
		IncludeOneShot:    true,
		ExcludeSessionIDs: []string{fixture.alphaID},
		Limit:             10,
	})
	require.NoError(err)
	assert.Empty(excluded.Matches)

	findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Project: "alpha",
		Limit:   10,
	})
	require.NoError(err)
	require.Len(findings.Findings, 1)
	finding := findings.Findings[0]
	assert.Equal("test_secret", finding.RuleName)
	assert.Equal("alpha", finding.Project)

	source, ok, err := store.SecretFindingSource(ctx, finding.SecretFinding)
	require.NoError(err)
	require.True(ok)
	assert.Equal("secret token sk-duckdb", source)
}

func TestSearchContentFTSSingleTermFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "alpha",
		Mode:           "fts",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.NotEmpty(got.Matches)
	assert.Equal(fixture.alphaID, got.Matches[0].SessionID)
	assert.Equal("message", got.Matches[0].Location)
}

func TestSearchContentFTSMatchesNonContiguousTerms(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	body := strings.Repeat("prefix ", 30) + "the quick brown fox jumps"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-fts-both", "alpha", "first",
				"2026-03-22T10:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-fts-both", 0, "user",
				body,
				"2026-03-22T10:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"duck-fts-one", "alpha", "first",
				"2026-03-22T11:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-fts-one", 0, "user",
				"the quick answer only",
				"2026-03-22T11:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "quick fox",
		Mode:           "fts",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(got.Matches, 1)
	assert.Equal("duck-fts-both", got.Matches[0].SessionID)
	assert.Contains(got.Matches[0].Snippet, "quick")
	assert.Contains(got.Matches[0].Snippet, "fox")
}

// TestSearchContentMatchTimestampIsRFC3339 pins the wire format of
// ContentMatch.Timestamp for the DuckDB (quack) read backend. The mirror
// stores message and tool_result_events timestamps as a DuckDB TIMESTAMP
// column; the search-content queries used to project that column via
// COALESCE(CAST(m.timestamp AS TEXT), the empty string), which DuckDB renders SQL-style
// ("2026-03-22 10:15:30", space-separated, no zone) rather than
// RFC3339/RFC3339Nano. Callers (e.g. the CLI's AGE column and the
// PostgreSQL/SQLite backends via FormatISO8601) expect RFC3339Nano UTC, so
// every source branch (messages, tool_input, tool_result via tool_calls, and
// tool_result via tool_result_events) must format the raw timestamp the same
// way formatDBTime already does for session timestamps.
func TestSearchContentMatchTimestampIsRFC3339(t *testing.T) {
	parentRequire := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-ts-format"
	msgTS := "2026-03-22T10:15:30.000Z"
	toolTS := "2026-03-22T10:15:31.000Z"
	eventTS := "2026-03-22T10:15:32.000Z"
	wantMsg, err := time.Parse(time.RFC3339Nano, msgTS)
	parentRequire.NoError(err)
	wantTool, err := time.Parse(time.RFC3339Nano, toolTS)
	parentRequire.NoError(err)
	wantEvent, err := time.Parse(time.RFC3339Nano, eventTS)
	parentRequire.NoError(err)

	// call0 has no ToolUseID, so it always surfaces via the tc.result_content
	// branch (the tool_result_events exclusion only fires when tc.tool_use_id
	// is non-empty and matches an event). call1 has a ToolUseID matching a
	// ResultEvent, so it is excluded from the tc.result_content branch and
	// instead surfaces via the tool_result_events branch, which reads the
	// event's own timestamp rather than the owning message's.
	call0 := db.ToolCall{
		CallIndex:     0,
		ToolName:      "shell",
		InputJSON:     `{"cmd":"timestampneedle-input"}`,
		ResultContent: "timestampneedle-toolresult",
	}
	call1 := db.ToolCall{
		CallIndex:     1,
		ToolName:      "shell",
		ToolUseID:     "evt-1",
		ResultContent: "no-match-here",
		ResultEvents: []db.ToolResultEvent{{
			ToolUseID:     "evt-1",
			Source:        "test",
			Status:        "success",
			Content:       "timestampneedle-event",
			ContentLength: len("timestampneedle-event"),
			Timestamp:     eventTS,
			EventIndex:    0,
		}},
	}
	msg0 := syncMessage(sessionID, 0, "user", "timestampneedle-message", msgTS)
	msg1 := syncMessage(sessionID, 1, "assistant", "reply", toolTS, call0, call1)

	_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "first", msgTS, 2),
		Messages:        []db.Message{msg0, msg1},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	parentRequire.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	parentRequire.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	parentRequire.NoError(err)

	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode+"/message", func(t *testing.T) {
			require := require.New(t)

			got, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern:        "timestampneedle-message",
				Mode:           mode,
				Sources:        []string{"messages"},
				IncludeOneShot: true,
				Limit:          10,
			})
			require.NoError(err)
			require.Len(got.Matches, 1)
			parsed, err := time.Parse(time.RFC3339Nano, got.Matches[0].Timestamp)
			require.NoError(err, "match timestamp must parse as RFC3339Nano, got %q", got.Matches[0].Timestamp)
			assert.True(t, wantMsg.Equal(parsed), "want %v, got %v", wantMsg, parsed)
		})

		t.Run(mode+"/tool_input", func(t *testing.T) {
			require := require.New(t)

			got, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern:        "timestampneedle-input",
				Mode:           mode,
				Sources:        []string{"tool_input"},
				IncludeOneShot: true,
				Limit:          10,
			})
			require.NoError(err)
			require.Len(got.Matches, 1)
			parsed, err := time.Parse(time.RFC3339Nano, got.Matches[0].Timestamp)
			require.NoError(err, "match timestamp must parse as RFC3339Nano, got %q", got.Matches[0].Timestamp)
			assert.True(t, wantTool.Equal(parsed), "want %v, got %v", wantTool, parsed)
		})

		t.Run(mode+"/tool_result", func(t *testing.T) {
			require := require.New(t)

			got, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern:        "timestampneedle-event",
				Mode:           mode,
				Sources:        []string{"tool_result"},
				IncludeOneShot: true,
				Limit:          10,
			})
			require.NoError(err)
			require.Len(got.Matches, 1)
			parsed, err := time.Parse(time.RFC3339Nano, got.Matches[0].Timestamp)
			require.NoError(err, "match timestamp must parse as RFC3339Nano, got %q", got.Matches[0].Timestamp)
			assert.True(t, wantEvent.Equal(parsed), "want %v, got %v", wantEvent, parsed)
		})
	}
}

// TestSearchContentOrdinalRangeSelfRange pins the ordinal_range contract on
// DuckDB content search: the field is always present and derived from the
// conversation-unit rules, never a zero-valued [0, 0] at a nonzero anchor
// ordinal. This fixture's assistant anchor is a single-member run bounded by
// the user opener, so the derived range equals the self-range. Substring and
// regex modes cover the two scan paths (scanDuckContentRows and the regex
// candidate loop).
func TestSearchContentOrdinalRangeSelfRange(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			"duck-range", "alpha", "first",
			"2026-03-22T10:00:00.000Z", 2,
		),
		Messages: []db.Message{
			syncMessage("duck-range", 0, "user",
				"an unrelated opener", "2026-03-22T10:00:00.000Z"),
			syncMessage("duck-range", 1, "assistant",
				"the rangeneedle reply", "2026-03-22T10:00:01.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)

			got, err := store.SearchContent(ctx, db.ContentSearchFilter{
				Pattern:        "rangeneedle",
				Mode:           mode,
				Sources:        []string{"messages"},
				IncludeOneShot: true,
				Limit:          10,
			})
			require.NoError(err)
			require.Len(got.Matches, 1)
			m := got.Matches[0]
			require.Equal(1, m.Ordinal, "anchor ordinal")
			assert.Equal(t, [2]int{1, 1}, m.OrdinalRange,
				"ordinal_range must be the derived single-member run, not [0, 0]")
		})
	}
}

func TestSearchContentInvalidModeReturnsInputError(t *testing.T) {
	ctx := t.Context()
	store, _ := newSyncedStore(t)

	_, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "alpha",
		Mode:           "bad-mode",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.Error(t, err)
	var inputErr *db.SearchInputError
	assert.ErrorAs(t, err, &inputErr,
		"expected *SearchInputError, got %T: %v", err, err)
}

func TestSearchContentRedactsSecretsUnlessRevealed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-secret-content"
	secretBody := "prefix AKIA" + "7QHWN2DKR4FYPLJM needle suffix"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "secret first", "2026-01-16T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", secretBody, "2026-01-16T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	redacted, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "needle",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(redacted.Matches, 1)
	assert.NotContains(redacted.Matches[0].Snippet, "AKIA"+"7QHWN2DKR4FYPLJM")

	revealed, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "needle",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		RevealSecrets:  true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(revealed.Matches, 1)
	assert.Contains(revealed.Matches[0].Snippet, "AKIA"+"7QHWN2DKR4FYPLJM")
}

func TestSearchGroupsMessagesAndIncludesNameMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)

	nameSession := syncSession("duck-search-name", "alpha", "plain first", "2026-01-15T00:00:00.000Z", 1)
	sessionName := "needle session name"
	nameSession.SessionName = &sessionName
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession("duck-search-content", "alpha", "content first", "2026-01-14T00:00:00.000Z", 2),
			Messages: []db.Message{
				syncMessage("duck-search-content", 0, "user", "prefix needle hit", "2026-01-14T00:00:00.000Z"),
				syncMessage("duck-search-content", 1, "assistant", "needle second hit", "2026-01-14T00:01:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         nameSession,
			Messages:        []db.Message{syncMessage("duck-search-name", 0, "user", "plain body", "2026-01-15T00:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.Search(ctx, db.SearchFilter{Query: "needle", Limit: 10})
	require.NoError(err)
	require.Len(got.Results, 2)
	assert.Equal("duck-search-content", got.Results[0].SessionID)
	assert.Equal(1, got.Results[0].Ordinal)
	assert.Equal("duck-search-name", got.Results[1].SessionID)
	assert.Equal(-1, got.Results[1].Ordinal)
	assert.Equal("needle session name", got.Results[1].Snippet)

	quotedContent, err := store.Search(ctx, db.SearchFilter{Query: `"needle second"`, Limit: 10})
	require.NoError(err)
	require.Len(quotedContent.Results, 1)
	assert.Equal("duck-search-content", quotedContent.Results[0].SessionID)
	assert.Equal(1, quotedContent.Results[0].Ordinal)

	quotedName, err := store.Search(ctx, db.SearchFilter{Query: `"needle session"`, Limit: 10})
	require.NoError(err)
	require.Len(quotedName.Results, 1)
	assert.Equal("duck-search-name", quotedName.Results[0].SessionID)
	assert.Equal(-1, quotedName.Results[0].Ordinal)

	renamed := "needle override rename"
	require.NoError(local.RenameSession("duck-search-name", &renamed))
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	overridden, err := store.Search(ctx, db.SearchFilter{Query: "override", Limit: 10})
	require.NoError(err)
	require.Len(overridden.Results, 1)
	assert.Equal("duck-search-name", overridden.Results[0].SessionID)
	assert.Equal(-1, overridden.Results[0].Ordinal)
	assert.Equal("needle override rename", overridden.Results[0].Snippet)
}

// TestSearchOperatorTokenNoError mirrors the SQLite FTS 500 regression on the
// DuckDB/ILIKE backend: a single token containing operator characters (hyphen,
// colon, embedded quote), whether raw or explicitly quoted, must match content
// and not error. ILIKE has no FTS-operator hazard, but this pins backend parity.
func TestSearchOperatorTokenNoError(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-optok-001"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", "first msg text", "2026-03-20T10:00:00.000Z", 2),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "user", `hit error-401 from the api and say"hi`, "2026-03-20T10:00:00.000Z"),
			syncMessage(sessionID, 1, "assistant", "returned status:500 to client", "2026-03-20T10:00:01.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	for _, raw := range []string{"error-401", "status:500", `say"hi`, `"say""hi"`} {
		page, err := store.Search(ctx, db.SearchFilter{
			Query: raw, Limit: 10,
		})
		require.NoError(err, "Search(%q)", raw)
		require.Len(page.Results, 1, "results for %q", raw)
		assert.Equal(t, sessionID, page.Results[0].SessionID, "session for %q", raw)
	}
}

// TestSearchMultiTermAND verifies that a multi-term query matches a session only
// when every term appears in its content (AND), matching SQLite FTS5's implicit
// AND so the same user query behaves identically across backends. Before the
// fix, DuckDB stripped only the outer quote pair from PrepareFTSQuery's
// `"fix" "bug"` output and matched the literal substring `fix" "bug`, which
// found nothing.
func TestSearchMultiTermAND(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("duck-andboth-001", "alpha", "first msg text", "2026-03-21T10:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("duck-andboth-001", 0, "user", "duckfixterm and duckbugterm both here", "2026-03-21T10:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-andone-001", "alpha", "first msg text", "2026-03-21T11:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("duck-andone-001", 0, "user", "only duckfixterm present", "2026-03-21T11:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	page, err := store.Search(ctx, db.SearchFilter{
		Query: db.PrepareFTSQuery("duckfixterm duckbugterm"), Limit: 10,
	})
	require.NoError(err, "Search")
	require.Len(page.Results, 1, "only the session containing both terms")
	assert.Equal(t, "duck-andboth-001", page.Results[0].SessionID, "session")
}

func TestStoreCurationMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)

	starred, err := store.ListStarredSessionIDs(ctx)
	require.NoError(err)
	assert.Equal([]string{fixture.alphaID}, starred)

	ok, err := store.StarSession(fixture.betaID)
	require.ErrorIs(err, db.ErrReadOnly)
	assert.False(ok)
	require.ErrorIs(store.BulkStarSessions([]string{fixture.betaID}), db.ErrReadOnly)
	require.ErrorIs(store.UnstarSession(fixture.alphaID), db.ErrReadOnly)
	starred, err = store.ListStarredSessionIDs(ctx)
	require.NoError(err)
	assert.Equal([]string{fixture.alphaID}, starred)

	msgs, err := store.GetAllMessages(ctx, fixture.betaID)
	require.NoError(err)
	require.Len(msgs, 1)
	note := "duck pin"
	pinID, err := store.PinMessage(fixture.betaID, msgs[0].ID, &note)
	require.ErrorIs(err, db.ErrReadOnly)
	assert.Zero(pinID)

	require.ErrorIs(store.UnpinMessage(fixture.alphaID, msgs[0].ID), db.ErrReadOnly)
	pins, err := store.ListPinnedMessages(ctx, fixture.alphaID, "")
	require.NoError(err)
	require.Len(pins, 1)
	assert.Equal("pin alpha", *pins[0].Note)
}

func TestStoreAnalyticsUsageAndTrends(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, fixture := newSyncedStore(t)
	filter := db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}

	summary, err := store.GetAnalyticsSummary(ctx, filter)
	require.NoError(err)
	assert.Equal(2, summary.TotalSessions)
	assert.Equal(3, summary.TotalMessages)
	assert.Equal(2, summary.ActiveProjects)

	activity, err := store.GetAnalyticsActivity(ctx, filter, "day")
	require.NoError(err)
	assert.NotEmpty(activity.Series)

	heatmap, err := store.GetAnalyticsHeatmap(ctx, filter, "messages")
	require.NoError(err)
	require.Len(heatmap.Entries, 31)

	projects, err := store.GetAnalyticsProjects(ctx, filter)
	require.NoError(err)
	require.Len(projects.Projects, 2)

	hours, err := store.GetAnalyticsHourOfWeek(ctx, filter)
	require.NoError(err)
	assert.Len(hours.Cells, 168)

	shape, err := store.GetAnalyticsSessionShape(ctx, filter)
	require.NoError(err)
	assert.Equal(2, shape.Count)
	assert.Equal(1, distributionCount(shape.AutonomyDistribution, "1-2"))
	assert.Equal(1, distributionCount(shape.AutonomyDistribution, "<0.5"))

	tools, err := store.GetAnalyticsTools(ctx, filter)
	require.NoError(err)
	assert.Equal(1, tools.TotalCalls)

	velocity, err := store.GetAnalyticsVelocity(ctx, filter)
	require.NoError(err)
	assert.NotNil(velocity)

	top, err := store.GetAnalyticsTopSessions(ctx, filter, "messages")
	require.NoError(err)
	require.NotEmpty(top.Sessions)
	assert.Equal(fixture.alphaID, top.Sessions[0].ID)

	signals, err := store.GetAnalyticsSignals(ctx, filter)
	require.NoError(err)
	assert.Equal(2, signals.UnscoredSessions)

	trendTerms, err := db.ParseTrendTerms([]string{"alpha"})
	require.NoError(err)
	trends, err := store.GetTrendsTerms(ctx, filter, trendTerms, "week")
	require.NoError(err)
	assert.Equal(1, trends.Series[0].Total)

	usageFilter := db.UsageFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}
	usage, err := store.GetDailyUsage(ctx, usageFilter)
	require.NoError(err)
	assert.Equal(13, usage.Totals.InputTokens)
	assert.Equal(11, usage.Totals.OutputTokens)
	assert.Equal(money.MustParseDollars("0.000204"), usage.Totals.TotalCost)

	topCost, err := store.GetTopSessionsByCost(ctx, usageFilter, 10)
	require.NoError(err)
	require.NotEmpty(topCost)
	assert.Equal(fixture.alphaID, topCost[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, usageFilter)
	require.NoError(err)
	assert.Equal(2, counts.Total)
	assert.Equal(1, counts.ByProject["alpha"])

	sessionUsage, err := store.GetSessionUsage(ctx, fixture.alphaID, true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal([]string{"claude-test"}, sessionUsage.Models)
}

func TestLoadPricingUsesDBRowsAsEffectiveTableAndOverlaysOverrides(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	store.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-model": {
			InputMicrodollarsPerMTok: money.MustParseDollars("9").Microdollars, OutputMicrodollarsPerMTok: money.MustParseDollars("10").Microdollars, CacheCreationMicrodollarsPerMTok: money.MustParseDollars("11").Microdollars, CacheReadMicrodollarsPerMTok: money.MustParseDollars("12").Microdollars,
		},
	})

	_, err := conn.ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok, updated_at
		) VALUES
			('claude-sonnet-4-6', 30000000, 150000000, 37500000, 3000000, '2026-06-08T12:00:00Z'),
			('_fallback_version', 999, 999, 999, 999, '')`)
	require.NoError(err)

	got, err := store.loadPricing(ctx)
	require.NoError(err)

	assert.NotContains(got, "gpt-5.5")
	assert.Equal(duckRates{
		input: money.MustParseDollars("30"), output: money.MustParseDollars("150"), cacheCreation: money.MustParseDollars("37.5"), cacheRead: money.MustParseDollars("3"),
		updatedAt: ptrTime(t, "2026-06-08T12:00:00Z"),
		source:    export.PricingRowSourceFetched,
	}, got["claude-sonnet-4-6"])
	assert.NotContains(got, "_fallback_version")
	assert.Equal(duckRates{
		input: money.MustParseDollars("9"), output: money.MustParseDollars("10"), cacheCreation: money.MustParseDollars("11"), cacheRead: money.MustParseDollars("12"),
		source: export.PricingRowSourceCustom,
	}, got["custom-model"])
}

func TestProjectIdentityMapLegacyFallbackUsesFilePath(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)

	_, err := conn.ExecContext(ctx, `
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES (?, ?)`, "legacy-test-archive", "legacy-test-salt")
	require.NoError(err)

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent, cwd, file_path)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"file-path-identity", "file-project", "laptop", "codex", "",
		"/fixtures/duck-file-project/session.jsonl",
	)
	require.NoError(err)

	got, err := store.BuildProjectIdentityMap(ctx, []string{"file-project"})
	require.NoError(err)
	require.Equal(export.ProjectResolutionUnknown,
		got["file-project"].Resolution)
	assert.Nil(t, got["file-project"].Identity)
}

func TestProjectIdentityMapLegacySessionsUseDistinctFallbackKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	_, err := conn.ExecContext(ctx, `
		INSERT INTO sessions (id, project, machine, agent)
		VALUES
			('legacy-alpha', 'alpha', 'host', 'codex'),
			('legacy-beta', 'beta', 'host', 'codex')`)
	require.NoError(err)

	store := NewStoreFromDB(conn)
	first, err := store.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(err)
	second, err := store.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(err)

	assert.NotEmpty(first["alpha"].ProjectKey)
	assert.NotEmpty(first["beta"].ProjectKey)
	assert.NotEqual(first["alpha"].ProjectKey, first["beta"].ProjectKey)
	assert.Equal(first["alpha"].ProjectKey, second["alpha"].ProjectKey)
	assert.Equal(first["beta"].ProjectKey, second["beta"].ProjectKey)
	assert.Len(export.ProjectMapForWire(first), 2)
}

func TestProjectIdentityObservationRoundTripsRepositoryContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	observedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	_, err := conn.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			source_archive_id, source_archive_salt,
			project, machine, root_path, git_remote, git_remote_name,
			repository_path, worktree_name, worktree_root_path,
			worktree_relationship, checkout_state, git_branch,
			remote_resolution, remote_candidate_count, observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"archive-source", "archive-salt",
		"app", "host", "/private/app-worktree", "", "",
		"/private/app/.git", "feature", "/private/app-worktree",
		export.WorktreeLinked, export.CheckoutDetached, "",
		export.ProjectResolutionAmbiguous, 2, observedAt,
	)
	require.NoError(err)

	got, err := NewStoreFromDB(conn).ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal("archive-source", got[0].SourceArchiveID)
	assert.Equal("archive-salt", got[0].SourceArchiveSalt)
	assert.Equal("/private/app/.git", got[0].RepositoryPath)
	assert.Equal(export.WorktreeLinked, got[0].WorktreeRelationship)
	assert.Equal(export.CheckoutDetached, got[0].CheckoutState)
	assert.Equal(export.ProjectResolutionAmbiguous, got[0].RemoteResolution)
	assert.Equal(2, got[0].RemoteCandidateCount)
}

func TestProjectIdentityObservationsAggregateSourceArchives(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	for i, archiveID := range []string{"archive-a", "archive-b"} {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO source_archives (source_archive_id, source_archive_salt)
			VALUES (?, ?)`, archiveID, archiveID+"-salt")
		require.NoError(err)
		_, err = conn.ExecContext(ctx, `
			INSERT INTO source_project_identity_observations (
				source_archive_id, source_archive_salt, project, machine,
				root_path, git_remote, observed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			archiveID, archiveID+"-salt", "app", "host", "/repo/app",
			fmt.Sprintf("https://github.com/acme/app-%d.git", i), time.Now().UTC(),
		)
		require.NoError(err)
	}

	store := NewStoreFromDB(conn)
	all, err := store.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(err)
	require.Len(all, 2)
	aggregate, err := store.BuildProjectIdentityMap(ctx, []string{"app", "missing"})
	require.NoError(err)
	assert.NotEmpty(aggregate["app"].ProjectKey)
	assert.NotEmpty(aggregate["missing"].ProjectKey)
	assert.Contains(export.ProjectMapForWire(aggregate), aggregate["app"].ProjectKey)
	assert.Contains(export.ProjectMapForWire(aggregate), aggregate["missing"].ProjectKey)

}

func TestSourceArchiveScopeRejectsSaltMismatch(t *testing.T) {
	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, conn))
	exec := func(query string, args ...any) error {
		_, err := conn.ExecContext(ctx, query, args...)
		return err
	}

	queryRow := func(query string, args ...any) *sql.Row {
		return conn.QueryRowContext(ctx, query, args...)
	}
	require.NoError(t, upsertSourceArchiveScope(
		exec, queryRow, "archive-a", "salt-a"))
	err := upsertSourceArchiveScope(exec, queryRow, "archive-a", "salt-b")
	require.ErrorContains(t, err, "archive salt mismatch")
}

func TestLoadPricingUsesFallbackWhenEffectiveTableEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)

	got, err := store.loadPricing(ctx)
	require.NoError(err)

	fallback := pricingByPattern(t, pricingpkg.FallbackPricing(), "gpt-5.5")
	require.Contains(got, "gpt-5.5")
	assert.Equal(fallback.InputPerMTok, got["gpt-5.5"].input)
	assert.Equal(fallback.OutputPerMTok, got["gpt-5.5"].output)
	assert.Equal(export.PricingRowSourceEmbedded, got["gpt-5.5"].source)
	assert.Equal(duckCatalogPricingBands(fallback.Bands), got["gpt-5.5"].bands)
}

func TestLoadPricingClassifiesBandOnlyFallbackMismatchAsFetched(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	fallback := pricingByPattern(t, pricingpkg.FallbackPricing(), "gpt-5.5")
	require.NotEmpty(fallback.Bands)

	_, err := conn.ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_microdollars_per_mtok,
			output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok,
			cache_read_microdollars_per_mtok, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		fallback.ModelPattern,
		fallback.InputPerMTok.Microdollars,
		fallback.OutputPerMTok.Microdollars,
		fallback.CacheCreationPerMTok.Microdollars,
		fallback.CacheReadPerMTok.Microdollars,
		"2026-07-29T12:00:00Z",
	)
	require.NoError(err)

	got, err := store.loadPricing(ctx)
	require.NoError(err)
	assert.Equal(t, export.PricingRowSourceFetched, got["gpt-5.5"].source)
}

func TestLoadPricingRetainsCustomOverrideSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	fallback := pricingByPattern(t, pricingpkg.FallbackPricing(), "gpt-5.5")
	store.SetCustomPricing(map[string]config.CustomModelRate{
		"gpt-5.5": {
			InputMicrodollarsPerMTok:         fallback.InputPerMTok.Microdollars,
			OutputMicrodollarsPerMTok:        fallback.OutputPerMTok.Microdollars,
			CacheCreationMicrodollarsPerMTok: fallback.CacheCreationPerMTok.Microdollars,
			CacheReadMicrodollarsPerMTok:     fallback.CacheReadPerMTok.Microdollars,
		},
	})

	got, err := store.loadPricing(ctx)
	require.NoError(err)
	assert.Empty(got["gpt-5.5"].bands)
	block, err := export.NewPricingResolver(duckPricingRows(got)).BuildBlock()
	require.NoError(err)

	assert.Equal("custom+embedded", block.Source)
	assert.Equal(1, block.CustomOverrideCount)
}

func TestDuckDailyAndSessionUsageApplyPricingBandsOnlyToRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "banded-model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []db.PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
		}},
	}}))
	sessionID := "duck-pricing-band"
	msg := syncMessage(
		sessionID, 0, "assistant", "request", "2026-03-12T10:00:00.000Z")
	msg.Model = "banded-model"
	msg.TokenUsage = jsontext.Value(`{"input_tokens":300000}`)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:  syncSession(sessionID, "proj", "banded", "2026-03-12T10:00:00.000Z", 1),
		Messages: []db.Message{msg},
		UsageEvents: []db.UsageEvent{{
			Source: "goose-request", Model: "banded-model", InputTokens: 300_000,
			OccurredAt: "2026-03-12T10:00:30.000Z", DedupKey: "goose-request",
		}, {
			Source: "aggregate", Model: "banded-model", InputTokens: 300_000,
			OccurredAt: "2026-03-12T10:01:00.000Z", DedupKey: "aggregate",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-03-12", To: "2026-03-12", Timezone: "UTC",
	})
	require.NoError(err)
	assert.Equal(900_000, daily.Totals.InputTokens)
	assert.Equal(money.Money{Microdollars: 1_500_000}, daily.Totals.TotalCost)
	require.NotNil(daily.Pricing)
	provenance := daily.Pricing.Models["banded-model"]
	require.Len(provenance.Resolutions, 1)
	assert.Equal(export.PricingApplication{
		AggregateRowCount: 1,
		Bands: []export.AppliedPricingBand{{
			AboveInputTokens: 200_000,
			RequestCount:     2,
		}},
	}, provenance.Resolutions[0].Application)

	session, err := store.GetSessionUsage(ctx, sessionID, true)
	require.NoError(err)
	require.NotNil(session)
	assert.True(session.HasCost)
	assert.Equal(money.Money{Microdollars: 1_500_000}, session.Cost)
	require.Len(session.Breakdown, 3)
	assert.Equal(money.Money{Microdollars: 600_000}, session.Breakdown[0].Cost)
	assert.Equal(money.Money{Microdollars: 600_000}, session.Breakdown[1].Cost)
	assert.Equal(money.Money{Microdollars: 300_000}, session.Breakdown[2].Cost)
}

func pricingByPattern(t *testing.T, prices []pricingpkg.ModelPricing, pattern string) pricingpkg.ModelPricing {
	t.Helper()
	for _, p := range prices {
		if p.ModelPattern == pattern {
			return p
		}
	}
	t.Fatalf("missing fallback pricing for %s", pattern)
	return pricingpkg.ModelPricing{}
}

func ptrTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	utc := parsed.UTC()
	return &utc
}

func TestAnalyticsTopSessionsFiltersMetricEligibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-top-valid-output", "alpha", "valid output",
				"2026-01-20T00:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-top-valid-output", 0, "assistant", "valid output",
				"2026-01-20T00:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"duck-top-untracked-output", "alpha", "untracked output",
				"2026-01-20T01:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-top-untracked-output", 0, "assistant", "untracked output",
				"2026-01-20T01:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"duck-top-valid-duration", "alpha", "valid duration",
				"2026-01-20T02:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-top-valid-duration", 0, "assistant", "valid duration",
				"2026-01-20T02:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"duck-top-missing-duration", "alpha", "missing duration",
				"2026-01-20T03:00:00.000Z", 1,
			),
			Messages: []db.Message{syncMessage(
				"duck-top-missing-duration", 0, "assistant", "missing duration",
				"2026-01-20T03:00:00.000Z",
			)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET total_output_tokens = 25, has_total_output_tokens = TRUE
		WHERE id = 'duck-top-valid-output'`)
	require.NoError(err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET total_output_tokens = 999, has_total_output_tokens = FALSE
		WHERE id = 'duck-top-untracked-output'`)
	require.NoError(err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET started_at = '2026-01-20T02:00:00.000Z',
			ended_at = '2026-01-20T02:30:00.000Z'
		WHERE id = 'duck-top-valid-duration'`)
	require.NoError(err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET started_at = NULL, ended_at = NULL
		WHERE id = 'duck-top-missing-duration'`)
	require.NoError(err)

	store := NewStoreFromDB(syncer.DB())
	filter := db.AnalyticsFilter{From: "2026-01-20", To: "2026-01-20"}
	output, err := store.GetAnalyticsTopSessions(ctx, filter, "output_tokens")
	require.NoError(err)
	assert.Equal("output_tokens", output.Metric)
	require.NotEmpty(output.Sessions)
	assert.NotEqual("duck-top-untracked-output", output.Sessions[0].ID)
	for _, session := range output.Sessions {
		assert.NotEqual("duck-top-untracked-output", session.ID)
	}

	duration, err := store.GetAnalyticsTopSessions(ctx, filter, "duration")
	require.NoError(err)
	assert.Equal("duration", duration.Metric)
	require.NotEmpty(duration.Sessions)
	seenValidDuration := false
	for _, session := range duration.Sessions {
		assert.NotEqual("duck-top-missing-duration", session.ID)
		if session.ID == "duck-top-valid-duration" {
			seenValidDuration = true
			assert.Equal(30.0, session.DurationMin)
		}
	}
	assert.True(seenValidDuration, "valid duration session was filtered out")

	unknown, err := store.GetAnalyticsTopSessions(ctx, filter, "not-a-metric")
	require.NoError(err)
	assert.Equal("messages", unknown.Metric)
}

func TestAnalyticsTopSessionsDurationUsesActiveDuration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	wallSession := syncSession(
		"duck-wall-dominant", "alpha", "wall session",
		"2026-01-20T09:00:00.000Z", 3,
	)
	wallEndedAt := "2026-01-20T11:00:00.000Z"
	wallSession.EndedAt = &wallEndedAt
	activeSession := syncSession(
		"duck-actively-working", "alpha", "active session",
		"2026-01-20T09:30:00.000Z", 3,
	)
	activeEndedAt := "2026-01-20T09:50:00.000Z"
	activeSession.EndedAt = &activeEndedAt
	writes := []db.SessionBatchWrite{
		{
			Session: wallSession,
			Messages: []db.Message{
				syncMessage(
					"duck-wall-dominant", 0, "user", "wall start",
					"2026-01-20T09:00:00.000Z",
				),
				syncMessage(
					"duck-wall-dominant", 1, "assistant", "wall tool",
					"2026-01-20T10:59:00.000Z",
					db.ToolCall{
						ToolName:  "Read",
						Category:  "Read",
						ToolUseID: "duck-wall-tool",
						InputJSON: `{"file_path":"README.md"}`,
					},
				),
				syncMessage(
					"duck-wall-dominant", 2, "user", "wall finish",
					"2026-01-20T11:00:00.000Z",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: activeSession,
			Messages: []db.Message{
				syncMessage(
					"duck-actively-working", 0, "user", "active start",
					"2026-01-20T09:30:00.000Z",
				),
				syncMessage(
					"duck-actively-working", 1, "assistant", "active tool",
					"2026-01-20T09:35:00.000Z",
					db.ToolCall{
						ToolName:  "Edit",
						Category:  "Write",
						ToolUseID: "duck-active-tool",
						InputJSON: `{"file_path":"main.go"}`,
					},
				),
				syncMessage(
					"duck-actively-working", 2, "user", "active finish",
					"2026-01-20T09:50:00.000Z",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	store := NewStoreFromDB(syncer.DB())
	filter := db.AnalyticsFilter{From: "2026-01-20", To: "2026-01-20"}
	resp, err := store.GetAnalyticsTopSessions(ctx, filter, "duration")
	require.NoError(err)
	require.Len(resp.Sessions, 2)

	assert.Equal("duck-actively-working", resp.Sessions[0].ID)
	assert.Equal(20.0, resp.Sessions[0].DurationMin)
	// 5 min user->asst gap + a 15 min gap capped at the 5 min idle
	// cap = 10.
	assert.Equal(10.0, resp.Sessions[0].ActiveDurationMin)
	assert.Equal("duck-wall-dominant", resp.Sessions[1].ID)
	assert.Equal(120.0, resp.Sessions[1].DurationMin)
	// 119 min idle gap capped to 5 + a 1 min gap = 6.
	assert.Equal(6.0, resp.Sessions[1].ActiveDurationMin)
}

func TestAnalyticsProjectsPopulateDailyTrendAndSortByMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession("duck-project-alpha", "alpha", "alpha first", "2026-01-20T00:00:00.000Z", 5),
			Messages: []db.Message{
				syncMessage("duck-project-alpha", 0, "user", "alpha 0", "2026-01-20T00:00:00.000Z"),
				syncMessage("duck-project-alpha", 1, "assistant", "alpha 1", "2026-01-20T00:01:00.000Z"),
				syncMessage("duck-project-alpha", 2, "user", "alpha 2", "2026-01-20T00:02:00.000Z"),
				syncMessage("duck-project-alpha", 3, "assistant", "alpha 3", "2026-01-20T00:03:00.000Z"),
				syncMessage("duck-project-alpha", 4, "user", "alpha 4", "2026-01-20T00:04:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-project-zeta-a", "zeta", "zeta a", "2026-01-20T01:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("duck-project-zeta-a", 0, "user", "zeta a", "2026-01-20T01:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-project-zeta-b", "zeta", "zeta b", "2026-01-20T02:00:00.000Z", 1),
			Messages:        []db.Message{syncMessage("duck-project-zeta-b", 0, "user", "zeta b", "2026-01-20T02:00:00.000Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsProjects(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(err)
	require.Len(got.Projects, 2)
	assert.Equal("alpha", got.Projects[0].Name)
	assert.Equal(5, got.Projects[0].Messages)
	assert.Equal(5.0, got.Projects[0].DailyTrend)
	assert.Equal("zeta", got.Projects[1].Name)
	assert.Equal(2.0, got.Projects[1].DailyTrend)
}

func TestAnalyticsVelocityUsesMessageCyclesAndBreakdowns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-velocity-cycles"
	call := db.ToolCall{
		ToolName:  "search",
		Category:  "search",
		ToolUseID: "duck-velocity-tool",
		InputJSON: `{"query":"velocity"}`,
	}
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", "velocity first", "2026-01-22T00:00:00.000Z", 4),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "user", "u1", "2026-01-22T00:00:00.000Z"),
			syncMessage(sessionID, 1, "assistant", "assistant-one", "2026-01-22T00:00:30.000Z", call),
			syncMessage(sessionID, 2, "user", "u2", "2026-01-22T00:01:30.000Z"),
			syncMessage(sessionID, 3, "assistant", "assistant-two", "2026-01-22T00:02:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(err)
	assert.Equal(30.0, got.Overall.TurnCycleSec.P50)
	assert.Equal(30.0, got.Overall.FirstResponseSec.P50)
	assert.Equal(2.0, got.Overall.MsgsPerActiveMin)
	assert.Equal(13.0, got.Overall.CharsPerActiveMin)
	assert.Equal(0.5, got.Overall.ToolCallsPerActiveMin)
	require.Len(got.ByAgent, 1)
	assert.Equal("claude", got.ByAgent[0].Label)
	assert.Equal(1, got.ByAgent[0].Sessions)
	require.Len(got.ByComplexity, 1)
	assert.Equal("1-15", got.ByComplexity[0].Label)
}

func TestAnalyticsVelocitySingleMessageSessionsReturnArrays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-velocity-single"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "single", "2026-01-22T01:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "single", "2026-01-22T01:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(err)
	assert.NotNil(got.ByAgent)
	assert.Empty(got.ByAgent)
	assert.NotNil(got.ByComplexity)
	assert.Empty(got.ByComplexity)
}

func TestGetSessionTimingPopulatesSharedTimingPayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-timing"
	startedAt := "2026-01-20T00:00:00.000Z"
	endedAt := "2026-01-20T12:38:06.000Z"
	sess := syncSession(sessionID, "alpha", "timing first", startedAt, 3)
	sess.EndedAt = &endedAt
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		Messages: []db.Message{
			syncMessage(sessionID, 0, "user", "timing first", startedAt),
			syncMessage(sessionID, 1, "assistant", "tool response", "2026-01-20T00:01:00.000Z",
				db.ToolCall{
					ToolName:  "Read",
					Category:  "Read",
					ToolUseID: "tool-timing",
					InputJSON: `{"file_path":"README.md"}`,
					ResultEvents: []db.ToolResultEvent{
						{
							ToolUseID: "tool-timing",
							Source:    "tool_execution",
							Status:    "started",
							Timestamp: "2026-01-20T00:01:00.100Z",
						},
						{
							ToolUseID: "tool-timing",
							Source:    "tool_execution",
							Status:    "completed",
							Timestamp: "2026-01-20T00:01:03.825Z",
						},
					},
				}),
			syncMessage(sessionID, 2, "user", "next request", "2026-01-20T12:38:05.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	timing, err := store.GetSessionTiming(ctx, sessionID)
	require.NoError(err)
	require.NotNil(timing)
	assert.Equal(sessionID, timing.SessionID)
	assert.Equal(int64(45_486_000), timing.TotalDurationMs)
	assert.Equal(1, timing.TurnCount)
	assert.Equal(1, timing.ToolCallCount)
	assert.False(timing.Running)
	require.Len(timing.Turns, 1)
	assert.Equal(1, timing.Turns[0].Ordinal)
	require.NotNil(timing.Turns[0].DurationMs)
	assert.Equal(int64(3_825), *timing.Turns[0].DurationMs)
	require.Len(timing.Turns[0].Calls, 1)
	require.NotNil(timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(int64(3_725), *timing.Turns[0].Calls[0].DurationMs)
}

func TestGetAllMessagesDoesNotTruncateAtDefaultLimit(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-large-session"
	const messageCount = db.MaxMessageLimit + 5

	messages := make([]db.Message, 0, messageCount)
	for i := range messageCount {
		messages = append(messages, syncMessage(
			sessionID, i, "assistant",
			fmt.Sprintf("message-%04d", i),
			"2026-01-12T00:00:00.000Z",
		))
	}
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "large", "large first", "2026-01-12T00:00:00.000Z", messageCount),
		Messages:        messages,
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAllMessages(ctx, sessionID)
	require.NoError(err)
	require.Len(got, messageCount)
	assert.Equal(t, "message-1004", got[messageCount-1].Content)
}

func TestSearchContentRegexDoesNotUseLiteralLikePrefilter(t *testing.T) {
	ctx := t.Context()
	store, _ := newSyncedStore(t)

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `duck\s+result`,
		Mode:           "regex",
		Sources:        []string{"tool_result"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "duck result", got.Matches[0].Snippet)
}

func TestSearchContentRegexPaginatesAfterGlobalOrdering(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, _ := newSyncedStore(t)

	first, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `alpha|duck\s+result`,
		Mode:           "regex",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
	})
	require.NoError(err)
	require.Len(first.Matches, 1)
	assert.Equal("message", first.Matches[0].Location)
	assert.Equal("alpha first", first.Matches[0].Snippet)
	assert.Equal(1, first.NextCursor)

	second, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `alpha|duck\s+result`,
		Mode:           "regex",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
		Cursor:         first.NextCursor,
	})
	require.NoError(err)
	require.Len(second.Matches, 1)
	assert.Equal("tool_result", second.Matches[0].Location)
	assert.Equal("duck result", second.Matches[0].Snippet)
}

func TestSearchContentRegexOrdersBySessionRecency(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("a-old-regex", "alpha", "old", "2026-01-11T00:00:00Z", 1),
			Messages:        []db.Message{syncMessage("a-old-regex", 0, "user", "target word old", "2026-01-11T00:00:00Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("z-new-regex", "alpha", "new", "2026-01-11T00:00:00.500Z", 1),
			Messages:        []db.Message{syncMessage("z-new-regex", 0, "user", "target word new", "2026-01-11T00:00:00.500Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `target\s+word`,
		Mode:           "regex",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(got.Matches, 2)
	assert.Equal("z-new-regex", got.Matches[0].SessionID)
	assert.Equal("a-old-regex", got.Matches[1].SessionID)
}

func TestSearchContentGitBranchFilter(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	alphaMain := syncSession("branch-alpha-main", "alpha", "main session", "2026-01-11T00:00:00Z", 1)
	alphaMain.GitBranch = "main"
	alphaFeature := syncSession("branch-alpha-feature", "alpha", "feature session", "2026-01-11T00:01:00Z", 1)
	alphaFeature.GitBranch = "feature"
	betaMain := syncSession("branch-beta-main", "beta", "beta session", "2026-01-11T00:02:00Z", 1)
	betaMain.GitBranch = "main"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         alphaMain,
			Messages:        []db.Message{syncMessage(alphaMain.ID, 0, "user", "BRANCHNEEDLE alpha main", "2026-01-11T00:00:00Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         alphaFeature,
			Messages:        []db.Message{syncMessage(alphaFeature.ID, 0, "user", "BRANCHNEEDLE alpha feature", "2026-01-11T00:01:00Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         betaMain,
			Messages:        []db.Message{syncMessage(betaMain.ID, 0, "user", "BRANCHNEEDLE beta main", "2026-01-11T00:02:00Z")},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "BRANCHNEEDLE",
		Mode:           "substring",
		Sources:        []string{"messages"},
		GitBranch:      db.EncodeBranchFilterToken("alpha", "main"),
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(err)
	require.Len(got.Matches, 1)
	assert.Equal(t, alphaMain.ID, got.Matches[0].SessionID)
}

func TestSearchContentDateFilterUsesRequestedTimezone(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	previousDay := syncSession(
		"duck-new-york-previous-day", "alpha", "previous", "2024-06-16T01:00:00Z", 1)
	requestedDay := syncSession(
		"duck-new-york-requested-day", "alpha", "requested", "2024-06-16T05:00:00Z", 1)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: previousDay,
			Messages: []db.Message{syncMessage(
				previousDay.ID, 0, "user", "TIMEZONE_NEEDLE", *previousDay.StartedAt)},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: requestedDay,
			Messages: []db.Message{syncMessage(
				requestedDay.ID, 0, "user", "TIMEZONE_NEEDLE", *requestedDay.StartedAt)},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "TIMEZONE_NEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Date: "2024-06-16",
		Timezone: "America/New_York", IncludeOneShot: true, Limit: 10,
	})
	require.NoError(err)
	require.Len(got.Matches, 1)
	assert.Equal(t, requestedDay.ID, got.Matches[0].SessionID)
}

func TestSearchContentSubstringPaginatesAfterGlobalOrdering(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, _ := newSyncedStore(t)

	first, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
	})
	require.NoError(err)
	require.Len(first.Matches, 1)
	assert.Equal("message", first.Matches[0].Location)
	assert.Equal("secret token sk-duckdb", first.Matches[0].Snippet)
	assert.Equal(1, first.NextCursor)

	second, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
		Cursor:         first.NextCursor,
	})
	require.NoError(err)
	require.Len(second.Matches, 1)
	assert.Equal("tool_result", second.Matches[0].Location)
	assert.Equal("duck result", second.Matches[0].Snippet)
}

func TestSearchContentToolResultEmptyToolUseIDNotSuppressedByEvents(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-empty-tool-use"
	call := db.ToolCall{
		ToolName:            "legacy",
		Category:            "other",
		ResultContent:       "legacy needle result",
		ResultContentLength: len("legacy needle result"),
		ResultEvents: []db.ToolResultEvent{{
			Source:        "tool",
			Status:        "complete",
			Content:       "event result without the target",
			ContentLength: len("event result without the target"),
			Timestamp:     "2026-01-19T00:02:00.000Z",
			EventIndex:    0,
		}},
	}
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", "empty tool use", "2026-01-19T00:00:00.000Z", 1),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "assistant", "called tool", "2026-01-19T00:01:00.000Z", call),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			filter := db.ContentSearchFilter{
				Pattern:        "legacy needle",
				Sources:        []string{"tool_result"},
				IncludeOneShot: true,
				Limit:          10,
			}
			if mode == "regex" {
				filter.Mode = "regex"
				filter.Pattern = `legacy\s+needle`
			}
			got, err := store.SearchContent(ctx, filter)
			require.NoError(err)
			require.Len(got.Matches, 1)
			assert.Equal("tool_result", got.Matches[0].Location)
			assert.Contains(got.Matches[0].Snippet, "legacy needle")
		})
	}
}

func TestSearchContentLegacyToolResultsUseCallIndexTieBreaker(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-legacy-tool-result-order"
	first := "legacy needle first"
	second := "legacy needle second"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "tool result order",
			"2026-01-19T00:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				sessionID, 0, "assistant", "called tools",
				"2026-01-19T00:01:00.000Z",
				db.ToolCall{
					ToolName:            "legacy",
					Category:            "other",
					ResultContent:       first,
					ResultContentLength: len(first),
				},
				db.ToolCall{
					ToolName:            "legacy",
					Category:            "other",
					ResultContent:       second,
					ResultContentLength: len(second),
				},
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			filter := db.ContentSearchFilter{
				Pattern:        "legacy needle",
				Sources:        []string{"tool_result"},
				IncludeOneShot: true,
				Limit:          1,
			}
			if mode == "regex" {
				filter.Mode = "regex"
				filter.Pattern = `legacy\s+needle`
			}
			page, err := store.SearchContent(ctx, filter)
			require.NoError(err)
			require.Len(page.Matches, 1)
			assert.Contains(page.Matches[0].Snippet, first)
			require.NotZero(page.NextCursor)

			filter.Cursor = page.NextCursor
			page, err = store.SearchContent(ctx, filter)
			require.NoError(err)
			require.Len(page.Matches, 1)
			assert.Contains(page.Matches[0].Snippet, second)
		})
	}
}

func TestSearchContentToolResultEventsUseCallIndexTieBreaker(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-tool-result-event-order"
	first := "event needle first"
	second := "event needle second"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "tool result event order",
			"2026-01-19T00:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				sessionID, 0, "assistant", "called tools",
				"2026-01-19T00:01:00.000Z",
				db.ToolCall{
					ToolName: "legacy",
					Category: "other",
					ResultEvents: []db.ToolResultEvent{{
						Source:        "tool",
						Status:        "complete",
						Content:       first,
						ContentLength: len(first),
						Timestamp:     "2026-01-19T00:02:00.000Z",
					}},
				},
				db.ToolCall{
					ToolName: "legacy",
					Category: "other",
					ResultEvents: []db.ToolResultEvent{{
						Source:        "tool",
						Status:        "complete",
						Content:       second,
						ContentLength: len(second),
						Timestamp:     "2026-01-19T00:02:00.000Z",
					}},
				},
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			filter := db.ContentSearchFilter{
				Pattern:        "event needle",
				Sources:        []string{"tool_result"},
				IncludeOneShot: true,
				Limit:          1,
			}
			if mode == "regex" {
				filter.Mode = "regex"
				filter.Pattern = `event\s+needle`
			}
			page, err := store.SearchContent(ctx, filter)
			require.NoError(err)
			require.Len(page.Matches, 1)
			assert.Contains(page.Matches[0].Snippet, first)
			require.NotZero(page.NextCursor)

			filter.Cursor = page.NextCursor
			page, err = store.SearchContent(ctx, filter)
			require.NoError(err)
			require.Len(page.Matches, 1)
			assert.Contains(page.Matches[0].Snippet, second)
		})
	}
}

func TestAnalyticsActivityMessageCountsRespectSessionFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	store, _ := newSyncedStore(t)

	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From:    "2026-01-01",
		To:      "2026-01-31",
		Project: "alpha",
	}, "day")
	require.NoError(err)
	require.Len(activity.Series, 1)
	assert.Equal("2026-01-10", activity.Series[0].Date)
	assert.Equal(1, activity.Series[0].UserMessages)
	assert.Equal(1, activity.Series[0].AssistantMessages)
	assert.Equal(2, activity.Series[0].ByAgent["claude"])
}

func TestAnalyticsActivityCountsToolCallRows(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-activity-tool-rows"
	first := `{"query":"alpha"}`
	second := `{"query":"beta"}`
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "activity tools",
			"2026-01-23T00:00:00.000Z", 1,
		),
		Messages: []db.Message{
			syncMessage(
				sessionID, 0, "assistant", "called tools",
				"2026-01-23T00:01:00.000Z",
				db.ToolCall{
					ToolName:  "search",
					Category:  "search",
					InputJSON: first,
				},
				db.ToolCall{
					ToolName:  "search",
					Category:  "search",
					InputJSON: second,
				},
			),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	_, err = syncer.DB().ExecContext(ctx,
		`UPDATE messages SET has_tool_use = FALSE WHERE session_id = ?`,
		sessionID,
	)
	require.NoError(err)

	store := NewStoreFromDB(syncer.DB())
	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2026-01-23",
		To:   "2026-01-23",
	}, "day")
	require.NoError(err)
	require.Len(activity.Series, 1)
	assert.Equal(t, 2, activity.Series[0].ToolCalls)
}

func TestAnalyticsActivitySkipsSystemUserMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-activity-system"
	systemMsg := syncMessage(sessionID, 0, "user", "system banner", "2026-01-23T00:00:00.000Z")
	systemMsg.IsSystem = true
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", "activity", "2026-01-23T00:00:00.000Z", 2),
		Messages: []db.Message{
			systemMsg,
			syncMessage(sessionID, 1, "user", "real user", "2026-01-23T00:01:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2026-01-23",
		To:   "2026-01-23",
	}, "day")
	require.NoError(err)
	require.Len(activity.Series, 1)
	assert.Equal(2, activity.Series[0].Messages)
	assert.Equal(1, activity.Series[0].UserMessages)
	assert.Equal(2, activity.Series[0].ByAgent["claude"])
}

func TestAnalyticsSessionFiltersUseMessageTimeForHourAndDay(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession("duck-time-a", "alpha", "time a", "2026-01-21T01:00:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("duck-time-a", 0, "user", "time a", "2026-01-21T09:15:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("duck-time-b", "alpha", "time b", "2026-01-21T09:00:00.000Z", 1),
			Messages: []db.Message{
				syncMessage("duck-time-b", 0, "user", "time b", "2026-01-21T10:15:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	hour := 9
	dow := 2
	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From:      "2026-01-21",
		To:        "2026-01-21",
		Timezone:  "UTC",
		DayOfWeek: &dow,
		Hour:      &hour,
	})
	require.NoError(err)
	assert.Equal(t, 1, summary.TotalSessions)
}

func TestAnalyticsTerminationFilterUsesSharedStateSemantics(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	clean := "clean"
	pending := "tool_call_pending"
	truncated := "truncated"
	old := "2026-01-21T09:00:00.000Z"

	cleanSession := syncSession("duck-term-clean", "alpha", "clean", old, 1)
	cleanSession.TerminationStatus = &clean
	pendingSession := syncSession("duck-term-pending", "alpha", "pending", old, 1)
	pendingSession.TerminationStatus = &pending
	truncatedSession := syncSession("duck-term-truncated", "alpha", "truncated", old, 1)
	truncatedSession.TerminationStatus = &truncated
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         cleanSession,
			Messages:        []db.Message{syncMessage(cleanSession.ID, 0, "user", "clean", old)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         pendingSession,
			Messages:        []db.Message{syncMessage(pendingSession.ID, 0, "user", "pending", old)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         truncatedSession,
			Messages:        []db.Message{syncMessage(truncatedSession.ID, 0, "user", "truncated", old)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From:        "2026-01-21",
		To:          "2026-01-21",
		Timezone:    "UTC",
		Termination: "unclean",
	})
	require.NoError(err)
	assert.Equal(t, 2, summary.TotalSessions)
}

func TestAnalyticsActiveSinceParsesEquivalentOffsets(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-active-since-offset"
	session := syncSession(sessionID, "alpha", "offset active", "2026-01-21T08:00:00.000Z", 1)
	endedAt := "2026-01-21T10:00:00.000Z"
	session.EndedAt = &endedAt
	session.LocalModifiedAt = &endedAt

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         session,
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "offset active", "2026-01-21T08:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		ActiveSince: "2026-01-21T11:00:00+02:00",
	})
	require.NoError(err)
	assert.Equal(t, 1, summary.TotalSessions)
}

func TestAnalyticsHourOfWeekRespectsSessionFilters(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	ts := "2026-01-21T09:15:00.000Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("duck-how-alpha", "alpha", "hour alpha", ts, 1),
			Messages:        []db.Message{syncMessage("duck-how-alpha", 0, "user", "alpha", ts)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-how-beta", "beta", "hour beta", ts, 1),
			Messages:        []db.Message{syncMessage("duck-how-beta", 0, "user", "beta", ts)},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From:     "2026-01-21",
		To:       "2026-01-21",
		Timezone: "UTC",
		Project:  "alpha",
	})
	require.NoError(err)
	assert.Equal(t, 1, hourOfWeekMessages(got.Cells, 2, 9))
}

func TestAnalyticsHourOfWeekIncludesOvernightMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	start := "2026-01-21T23:30:00.000Z"
	session := syncSession("duck-how-overnight", "alpha", "overnight", start, 2)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: session,
			Messages: []db.Message{
				syncMessage("duck-how-overnight", 0, "user", "before midnight", start),
				syncMessage("duck-how-overnight", 1, "assistant", "after midnight",
					"2026-01-22T00:30:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From:     "2026-01-21",
		To:       "2026-01-21",
		Timezone: "UTC",
	})
	require.NoError(err)
	// 2026-01-21 is a Wednesday (ISO dow 2). The session falls inside the
	// date window, so all of its messages count, including the one whose
	// local date crosses past the To bound.
	assert.Equal(1, hourOfWeekMessages(got.Cells, 2, 23))
	assert.Equal(1, hourOfWeekMessages(got.Cells, 3, 0))
}

func TestTrendsTermsApplySessionFiltersAndSystemPrefixExclusion(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	start := "2026-01-22T09:00:00.000Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession("duck-trend-a", "alpha", "trend a", start, 2),
			Messages: []db.Message{
				syncMessage("duck-trend-a", 0, "user", db.SystemMsgPrefixes[0]+" seam", start),
				syncMessage("duck-trend-a", 1, "user", "seam", start),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("duck-trend-b", "beta", "trend b", start, 1),
			Messages: []db.Message{
				syncMessage("duck-trend-b", 0, "user", "seam", start),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	trendTerms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(err)
	trends, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From:     "2026-01-22",
		To:       "2026-01-22",
		Timezone: "UTC",
		Project:  "alpha",
	}, trendTerms, "day")
	require.NoError(err)
	require.Len(trends.Series, 1)
	assert.Equal(t, 1, trends.Series[0].Total)
}

func TestDailyUsageDefaultsToLocalTimezone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldLocal := time.Local                             //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	time.Local = time.FixedZone("DuckLocal", -5*60*60) //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	t.Cleanup(func() { time.Local = oldLocal })        //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))
	sessionID := "duck-usage-local-day"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(sessionID, "alpha", "local usage", "2026-01-02T02:00:00.000Z", 1),
		Messages: []db.Message{
			syncMessage(sessionID, 0, "assistant", "local usage", "2026-01-02T02:00:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-01",
		To:   "2026-01-01",
	})
	require.NoError(err)
	require.Len(got.Daily, 1)
	assert.Equal("2026-01-01", got.Daily[0].Date)
	assert.Equal(1, got.Totals.InputTokens)
	assert.Equal(2, got.Totals.OutputTokens)
}

func TestDailyUsageActiveSinceUsesSessionActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-usage-session-activity"
	session := syncSession(sessionID, "alpha", "activity usage", "2026-01-01T00:00:00.000Z", 1)
	endedAt := "2026-01-03T00:00:00.000Z"
	session.EndedAt = &endedAt
	session.LocalModifiedAt = &endedAt

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{
			syncMessage(sessionID, 0, "assistant", "activity usage", "2026-01-01T01:00:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:        "2026-01-01",
		To:          "2026-01-01",
		Timezone:    "UTC",
		ActiveSince: "2026-01-02T00:00:00.000Z",
	})
	require.NoError(err)
	assert.Equal(1, got.Totals.InputTokens)
	assert.Equal(2, got.Totals.OutputTokens)
}

func TestDailyUsageHandlesBlankMessageTimestampWithoutSessionStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	sessionID := "duck-usage-blank-ts"
	session := syncSession(sessionID, "alpha", "blank timestamp usage", "", 2)
	session.StartedAt = nil

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{
			{
				SessionID:  sessionID,
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "",
				Model:      "claude-test",
				TokenUsage: jsontext.Value(`{"input_tokens":100,"output_tokens":50}`),
			},
			{
				SessionID:  sessionID,
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "",
				Model:      "claude-test",
				TokenUsage: jsontext.Value(`{"input_tokens":200,"output_tokens":75}`),
			},
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
	require.NoError(err)
	assert.Equal(300, got.Totals.InputTokens)
	assert.Equal(125, got.Totals.OutputTokens)
}

func hourOfWeekMessages(cells []db.HourOfWeekCell, dow, hour int) int {
	for _, cell := range cells {
		if cell.DayOfWeek == dow && cell.Hour == hour {
			return cell.Messages
		}
	}
	return 0
}

func distributionCount(buckets []db.DistributionBucket, label string) int {
	for _, bucket := range buckets {
		if bucket.Label == label {
			return bucket.Count
		}
	}
	return 0
}

func TestUsageDedupesClaudeMessageIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))

	first := syncMessage("duck-usage-a", 0, "assistant", "shared usage", "2026-01-13T00:00:00.000Z")
	first.ClaudeMessageID = "shared-message"
	first.ClaudeRequestID = "shared-request"
	second := syncMessage("duck-usage-b", 0, "assistant", "replayed usage", "2026-01-13T00:01:00.000Z")
	second.ClaudeMessageID = "shared-message"
	second.ClaudeRequestID = "shared-request"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("duck-usage-a", "alpha", "usage a", "2026-01-13T00:00:00.000Z", 1),
			Messages:        []db.Message{first},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-usage-b", "beta", "usage b", "2026-01-13T00:01:00.000Z", 1),
			Messages:        []db.Message{second},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31"}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(1, daily.Totals.InputTokens)
	assert.Equal(2, daily.Totals.OutputTokens)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal("duck-usage-a", top[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(err)
	assert.Equal(1, counts.Total)
	assert.Equal(1, counts.ByProject["alpha"])
	assert.NotContains(counts.ByProject, "beta")

	sessionUsage, err := store.GetSessionUsage(ctx, "duck-usage-b", true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal(money.MustParseDollars("0.000033"), sessionUsage.Cost)
	assert.Equal([]string{"claude-test"}, sessionUsage.Models)
	require.Len(sessionUsage.Breakdown, 1)
	entry := sessionUsage.Breakdown[0]
	assert.Equal(1, entry.Ordinal)
	require.NotNil(entry.MessageOrdinal)
	assert.Equal(0, *entry.MessageOrdinal)
	assert.Equal("message", entry.Source)
	assert.Equal("Prompt 1", entry.Label)
	assert.Equal("2026-01-13T00:01:00Z", entry.Timestamp)
	assert.Equal("claude-test", entry.Model)
	assert.Equal(1, entry.InputTokens)
	assert.Equal(2, entry.OutputTokens)
	assert.True(entry.HasCost)
	assert.Equal(money.MustParseDollars("0.000033"), entry.Cost)
}

func TestSessionUsagePrefersCompleteClaudeSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("5"),
		OutputPerMTok: money.MustParseDollars("25"),
	}}))

	first := syncMessage(
		"duck-streamed", 0, "assistant", "partial",
		"2026-01-13T00:00:00.000Z")
	first.ClaudeMessageID = "msg-stream"
	first.ClaudeRequestID = "req-stream"
	first.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":5}`)
	first.OutputTokens = 5
	second := syncMessage(
		"duck-streamed", 1, "assistant", "complete",
		"2026-01-13T00:01:00.000Z")
	second.ClaudeMessageID = "msg-stream"
	second.ClaudeRequestID = "req-stream"
	second.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":631}`)
	second.OutputTokens = 631

	session := syncSession(
		"duck-streamed", "alpha", "streamed",
		"2026-01-13T00:00:00.000Z", 2)
	session.TotalOutputTokens = 636
	session.HasTotalOutputTokens = true
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         session,
		Messages:        []db.Message{first, second},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetSessionUsage(ctx, "duck-streamed", true)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(631, got.TotalOutputTokens)
	assert.Equal(money.MustParseDollars("0.020775"), got.Cost)
	require.Len(got.Breakdown, 1)
	assert.Equal(631, got.Breakdown[0].OutputTokens)
	require.NotNil(got.Breakdown[0].MessageOrdinal)
	assert.Equal(1, *got.Breakdown[0].MessageOrdinal)

	withoutBreakdown, err := store.GetSessionUsage(
		ctx, "duck-streamed", false)
	require.NoError(err)
	assert.Equal(1, withoutBreakdown.BreakdownCount)
}

func TestUsageAggregatesPreferCompleteClaudeSnapshotAcrossSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("5"),
		OutputPerMTok: money.MustParseDollars("25"),
	}}))

	first := syncMessage(
		"duck-daily-streamed", 0, "assistant", "partial",
		"2026-01-13T00:00:00.000Z")
	first.ClaudeMessageID = "msg-stream"
	first.ClaudeRequestID = "req-stream"
	first.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":5}`)
	first.OutputTokens = 5
	second := syncMessage(
		"duck-daily-streamed-child", 0, "assistant", "complete",
		"2026-01-13T00:01:00.000Z")
	second.ClaudeMessageID = "msg-stream"
	second.ClaudeRequestID = "req-stream"
	second.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":631}`)
	second.OutputTokens = 631

	parent := syncSession(
		"duck-daily-streamed", "parent-project", "parent first message",
		"2026-01-13T00:00:00.000Z", 1)
	parent.Agent = "parent-agent"
	parent.Machine = "parent-machine"
	parent.DisplayName = new("parent display")
	child := syncSession(
		"duck-daily-streamed-child", "child-project", "child first message",
		"2026-01-13T00:01:00.000Z", 1)
	child.Agent = "child-agent"
	child.Machine = "child-machine"
	child.DisplayName = new("child display")
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         parent,
			Messages:        []db.Message{first},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         child,
			Messages:        []db.Message{second},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC", Breakdowns: true,
	})
	require.NoError(err)
	require.Len(result.Daily, 1)
	assert.Equal(1000, result.Totals.InputTokens)
	assert.Equal(631, result.Totals.OutputTokens)
	require.Len(result.Daily[0].ProjectBreakdowns, 1)
	assert.Equal("parent-project", result.Daily[0].ProjectBreakdowns[0].Project)
	require.Len(result.Daily[0].AgentBreakdowns, 1)
	assert.Equal("parent-agent", result.Daily[0].AgentBreakdowns[0].Agent)
	require.Len(result.Daily[0].MachineBreakdowns, 1)
	assert.Equal("parent-machine", result.Daily[0].MachineBreakdowns[0].MachineName)

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
	}, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal("duck-daily-streamed", top[0].SessionID)
	assert.Equal("parent first message", top[0].DisplayName)
	assert.Equal("parent-project", top[0].Project)
	assert.Equal("parent-agent", top[0].Agent)
	assert.Equal("2026-01-13T00:00:00Z", top[0].StartedAt)
	assert.Equal(1000, top[0].InputTokens)
	assert.Equal(631, top[0].OutputTokens)

	filtered, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	})
	require.NoError(err)
	assert.Equal(1000, filtered.Totals.InputTokens)
	assert.Equal(631, filtered.Totals.OutputTokens,
		"the attributed parent filter must retain the complete child snapshot")

	filteredTop, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
		ProjectLabels: []string{"parent-project"},
	}, 10)
	require.NoError(err)
	require.Len(filteredTop, 1)
	assert.Equal(631, filteredTop[0].OutputTokens)

	childFiltered, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
		ProjectLabels: []string{"child-project"},
	})
	require.NoError(err)
	assert.Zero(childFiltered.Totals.OutputTokens,
		"the source child metadata must not override parent attribution")
}

func TestUsageSessionCountsFilterAfterCrossSessionSnapshotSelection(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	parent := syncSession(
		"count-parent", "parent-project", "parent",
		"2026-01-13T00:00:00.000Z", 1)
	parent.Agent = "claude"
	child := syncSession(
		"count-child", "child-project", "child",
		"2026-01-13T00:01:00.000Z", 1)
	child.Agent = "claude"
	partial := syncMessage(
		"count-parent", 0, "assistant", "partial",
		"2026-01-13T00:00:00.000Z")
	partial.Model = "partial-model"
	partial.TokenUsage = jsontext.Value(`{"input_tokens":10,"output_tokens":5}`)
	partial.OutputTokens = 5
	partial.ClaudeMessageID = "count-message"
	partial.ClaudeRequestID = "count-request"
	complete := syncMessage(
		"count-child", 0, "assistant", "complete",
		"2026-01-13T00:01:00.000Z")
	complete.Model = "complete-model"
	complete.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":631}`)
	complete.OutputTokens = 631
	complete.ClaudeMessageID = "count-message"
	complete.ClaudeRequestID = "count-request"
	store := activityReportStore(t, []db.SessionBatchWrite{
		{Session: parent, Messages: []db.Message{partial},
			DataVersion: 1, ReplaceMessages: true},
		{Session: child, Messages: []db.Message{complete},
			DataVersion: 1, ReplaceMessages: true},
	}, nil)

	partialCounts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
		Model: "partial-model",
	})
	require.NoError(err)
	assert.Zero(partialCounts.Total,
		"the discarded partial model must not count a session")

	completeParentCounts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
		Model: "complete-model", ProjectLabels: []string{"parent-project"},
	})
	require.NoError(err)
	assert.Equal(1, completeParentCounts.Total)
	assert.Equal(1, completeParentCounts.ByProject["parent-project"])
	assert.NotContains(completeParentCounts.ByProject, "child-project")
}

func TestUsageAggregatesPreferLatestEqualOutputSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("5"),
		OutputPerMTok: money.MustParseDollars("25"),
	}}))

	zMessage := syncMessage(
		"z-snapshot", 0, "assistant", "z", "2026-01-13T00:01:00.000Z")
	zMessage.Model = "claude-test"
	zMessage.ClaudeMessageID = "msg-tie"
	zMessage.ClaudeRequestID = "req-tie"
	zMessage.TokenUsage = jsontext.Value(
		`{"input_tokens":900,"output_tokens":100}`)
	zMessage.OutputTokens = 100
	aMessage := syncMessage(
		"a-snapshot", 0, "assistant", "a", "2026-01-13T00:00:00.000Z")
	aMessage.Model = "claude-test"
	aMessage.ClaudeMessageID = "msg-tie"
	aMessage.ClaudeRequestID = "req-tie"
	aMessage.TokenUsage = jsontext.Value(
		`{"input_tokens":10,"output_tokens":100}`)
	aMessage.OutputTokens = 100

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: syncSession(
				"z-snapshot", "alpha", "z snapshot",
				"2026-01-13T00:00:00.000Z", 1),
			Messages: []db.Message{zMessage}, DataVersion: 1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"a-snapshot", "alpha", "a snapshot",
				"2026-01-13T00:00:00.000Z", 1),
			Messages: []db.Message{aMessage}, DataVersion: 1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
	})
	require.NoError(err)
	assert.Equal(900, result.Totals.InputTokens)
	assert.Equal(100, result.Totals.OutputTokens)
}

func TestUsageDedupesSourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))

	first := syncMessage("duck-usage-source-a", 0, "assistant", "shared usage", "2026-01-13T00:00:00.000Z")
	first.ClaudeMessageID = "shared-message"
	first.SourceUUID = "shared-source"
	second := syncMessage("duck-usage-source-b", 0, "assistant", "replayed usage", "2026-01-13T00:01:00.000Z")
	second.ClaudeMessageID = "shared-message"
	second.SourceUUID = "shared-source"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("duck-usage-source-a", "alpha", "usage a", "2026-01-13T00:00:00.000Z", 1),
			Messages:        []db.Message{first},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-usage-source-b", "beta", "usage b", "2026-01-13T00:01:00.000Z", 1),
			Messages:        []db.Message{second},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31"}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(1, daily.Totals.InputTokens)
	assert.Equal(2, daily.Totals.OutputTokens)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal("duck-usage-source-a", top[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(err)
	assert.Equal(1, counts.Total)
	assert.Equal(1, counts.ByProject["alpha"])
	assert.NotContains(counts.ByProject, "beta")

	sessionUsage, err := store.GetSessionUsage(ctx, "duck-usage-source-b", true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal(money.MustParseDollars("0.000033"), sessionUsage.Cost)
	assert.Equal([]string{"claude-test"}, sessionUsage.Models)
	require.Len(sessionUsage.Breakdown, 1)
	require.NotNil(sessionUsage.Breakdown[0].MessageOrdinal)
	assert.Equal(0, *sessionUsage.Breakdown[0].MessageOrdinal)
}

func TestUsagePreservesSessionSummaryUsageEventTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "summary-model",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("2"),
	}}))

	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	sessionID := "duck-summary-usage"
	sess := syncSession(sessionID, "alpha", "summary first", "2026-01-18T00:00:00.000Z", 0)
	sess.Agent = "hermes"
	sess.TotalOutputTokens = rawOutput
	sess.PeakContextTokens = rawInput
	sess.HasTotalOutputTokens = true
	sess.HasPeakContextTokens = true

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{{
			Source:       "session",
			Model:        "summary-model",
			InputTokens:  rawInput,
			OutputTokens: rawOutput,
			OccurredAt:   "2026-01-18T00:01:00.000Z",
			DedupKey:     "summary",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(rawInput, daily.Totals.InputTokens)
	assert.Equal(rawOutput, daily.Totals.OutputTokens)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal(sessionID, top[0].SessionID)
	assert.Equal(rawInput+rawOutput, top[0].TotalTokens)
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(err)
	assert.Equal(wantCost, top[0].Cost)

	sessionUsage, err := store.GetSessionUsage(ctx, sessionID, true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.Equal(rawOutput, sessionUsage.TotalOutputTokens)
	assert.Equal(rawInput, sessionUsage.PeakContextTokens)
	assert.True(sessionUsage.HasCost)
	assert.Equal(wantCost, sessionUsage.Cost)
	assert.Equal([]string{"summary-model"}, sessionUsage.Models)
	require.Len(sessionUsage.Breakdown, 1)
	entry := sessionUsage.Breakdown[0]
	assert.Equal("session", entry.Source)
	assert.Equal("session", entry.Label)
	assert.Nil(entry.MessageOrdinal)
	assert.Equal(rawInput, entry.InputTokens)
	assert.Equal(rawOutput, entry.OutputTokens)
	assert.True(entry.HasCost)
	assert.Equal(wantCost, entry.Cost)
}

func TestCopilotReportedCostSurvivesDuckDBPush(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-opus-4-6", InputPerMTok: money.MustParseDollars("10"), OutputPerMTok: money.MustParseDollars("15"),
	}}))
	reportedCost := money.MustParseDollars("0.035")
	sess := syncSession(
		"copilot:duck-reported", "alpha", "reported cost",
		"2026-01-18T00:00:00.000Z", 0)
	sess.Agent = "copilot"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{
			{
				Source: "shutdown", Model: "claude-opus-4-6",
				InputTokens: 1000, OutputTokens: 500,
				OccurredAt: "2026-01-18T00:01:00.000Z",
				DedupKey:   "shutdown-1",
			},
			{
				Source: "shutdown", Model: "claude-opus-4-6",
				InputTokens: 1000, OutputTokens: 500,
				Cost: &reportedCost, CostStatus: "exact",
				CostSource: db.CopilotReportedCostSource,
				OccurredAt: "2026-01-19T00:01:00.000Z",
				DedupKey:   "shutdown-2",
			},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	usage, err := store.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(reportedCost, usage.Cost)
	assert.InDelta(3.5, usage.AICredits, 1e-9)
	require.Len(usage.Breakdown, 2)
	assert.Equal(money.MustParseDollars("0.0175"), usage.Breakdown[0].Cost)
	assert.Equal(money.MustParseDollars("0.0175"), usage.Breakdown[1].Cost)
	assert.Equal(usage.Cost,
		money.MustAdd(usage.Breakdown[0].Cost, usage.Breakdown[1].Cost))

	daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-18", To: "2026-01-19", Timezone: "UTC",
	})
	require.NoError(err)
	require.Len(daily.Daily, 2)
	assert.Equal(money.MustParseDollars("0.0175"), daily.Daily[0].TotalCost)
	assert.Equal(money.MustParseDollars("0.0175"), daily.Daily[1].TotalCost)
	for _, day := range daily.Daily {
		require.Len(day.ModelBreakdowns, 1)
		assert.Equal(day.TotalCost, day.ModelBreakdowns[0].Cost)
	}
	assert.Equal(reportedCost, daily.Totals.TotalCost)
	require.NotNil(daily.Pricing)
	assert.Equal(export.CostSourceMixed, daily.Pricing.CostSource,
		"authoritative reported cost must surface in pricing provenance")
	assert.Equal(export.CostSourceComputed,
		daily.Pricing.Models["claude-opus-4-6"].CostSource)
}

func TestDuckDBDailyUsageKeepsAuthoritativeCostSessionScoped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "authoritative-cost-model",
		InputPerMTok:  money.MustParseDollars("10"),
		OutputPerMTok: money.MustParseDollars("20"),
	}}))
	reportedCost := money.MustParseDollars("0.035")
	authoritative := syncSession(
		"copilot:authoritative", "alpha", "reported",
		"2026-01-18T00:00:00.000Z", 1,
	)
	authoritative.Agent = "copilot"
	estimated := syncSession(
		"copilot:estimated", "alpha", "estimated",
		"2026-01-18T01:00:00.000Z", 1,
	)
	estimated.Agent = "copilot"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: authoritative,
			UsageEvents: []db.UsageEvent{{
				Source: "shutdown", Model: "authoritative-cost-model",
				InputTokens: 1000, OutputTokens: 500,
				Cost: &reportedCost, CostStatus: "exact",
				CostSource: db.CopilotReportedCostSource,
				OccurredAt: "2026-01-18T00:01:00.000Z",
				DedupKey:   "authoritative",
			}},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: estimated,
			UsageEvents: []db.UsageEvent{{
				Source: "shutdown", Model: "authoritative-cost-model",
				InputTokens: 1000, OutputTokens: 500,
				OccurredAt: "2026-01-18T01:01:00.000Z",
				DedupKey:   "estimated",
			}},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
	require.NoError(err)

	filter := db.UsageFilter{
		From: "2026-01-18", To: "2026-01-18", Timezone: "UTC",
	}
	want, err := local.GetDailyUsage(ctx, filter)
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	got, err := NewStoreFromDB(syncer.DB()).GetDailyUsage(ctx, filter)
	require.NoError(err)

	assert.Equal(money.MustParseDollars("0.055"), want.Totals.TotalCost)
	assert.Equal(want.Totals.TotalCost, got.Totals.TotalCost)
	assert.InDelta(5.5, want.Totals.CopilotAICredits, 1e-9,
		"credits derive from the authoritative-substituted totals")
	assert.InDelta(want.Totals.CopilotAICredits,
		got.Totals.CopilotAICredits, 1e-9)
	require.Len(got.Daily, 1)
	require.Len(want.Daily, 1)
	assert.Equal(want.Daily[0].Date, got.Daily[0].Date)
	assert.Equal(want.Daily[0].InputTokens, got.Daily[0].InputTokens)
	assert.Equal(want.Daily[0].OutputTokens, got.Daily[0].OutputTokens)
	assert.Equal(want.Daily[0].ModelsUsed, got.Daily[0].ModelsUsed)
	assert.Equal(want.Daily[0].TotalCost, got.Daily[0].TotalCost)
	require.Len(got.Daily[0].ModelBreakdowns, 1)
	require.Len(want.Daily[0].ModelBreakdowns, 1)
	assert.Equal(want.Daily[0].ModelBreakdowns[0].ModelName,
		got.Daily[0].ModelBreakdowns[0].ModelName)
	assert.Equal(want.Daily[0].ModelBreakdowns[0].Cost,
		got.Daily[0].ModelBreakdowns[0].Cost)
}

func TestDuckDBCostOnlyReportedSessionMatchesSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	reportedCost := money.MustParseDollars("0.0175")
	sess := syncSession(
		"copilot:cost-only", "alpha", "cost only",
		"2026-01-18T00:00:00.000Z", 0)
	sess.Agent = "copilot"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{{
			Source: "shutdown", Model: "copilot",
			Cost: &reportedCost, CostStatus: "exact",
			CostSource: db.CopilotReportedCostSource,
			OccurredAt: "2026-01-18T00:01:00.000Z",
			DedupKey:   "cost-only",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	want, err := local.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(want)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(got)
	assert.True(got.HasCost)
	assert.Equal(reportedCost, got.Cost)
	assert.False(got.HasTokenData,
		"a cost-only reported row is not token data")
	assert.Empty(got.Models,
		"a cost-only carrier row must not surface a model")
	assert.Zero(got.BreakdownCount)
	assert.Empty(got.Breakdown)
	assert.Equal(want.HasTokenData, got.HasTokenData)
	assert.Equal(want.Models, got.Models)
	assert.Equal(want.BreakdownCount, got.BreakdownCount)
	assert.Equal(want.Cost, got.Cost)

	gotNoBreakdown, err := store.GetSessionUsage(ctx, sess.ID, false)
	require.NoError(err)
	require.NotNil(gotNoBreakdown)
	assert.Zero(gotNoBreakdown.BreakdownCount,
		"the row-count path must exclude cost-only reported rows")
}

// TestDuckDBCostOnlyCodebuffSessionHasTokenDataFalse pins the backend
// parity rule surfaced by roborev on ab050f8: a Codebuff session whose
// contributor rows report only explicitCost (cost_source !=
// 'copilot-reported', zero billable tokens) must NOT flip HasTokenData.
// (*Store).sessionUsage computes HasTokenData from the session flags
// alone, matching SQLite and Postgres; a row-derived `hasRows ||`
// term previously spilled cost-only contributor rows into
// HasTokenData. Mirrors the copilot pattern in
// TestDuckDBCostOnlyReportedSessionMatchesSQLite but uses a
// non-copilot cost_source so the row passes the `contributes = true`
// gate (the copilot path early-returns).
func TestDuckDBCostOnlyCodebuffSessionHasTokenDataFalse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	reportedCost := money.MustParseDollars("0.0250")
	sess := syncSession(
		"codebuff:cost-only", "alpha", "cost only",
		"2026-01-18T00:00:00.000Z", 0)
	sess.Agent = "codebuff"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{{
			Source:     "shutdown",
			Model:      "codebuff-base",
			Cost:       &reportedCost,
			CostStatus: "exact",
			// Anything other than db.CopilotReportedCostSource causes
			// the SQL to set reported_cost_rows = 1, keeping the row
			// out of the copilot early-return and inside the
			// contributes=true branch that previously leaked into
			// HasTokenData.
			CostSource: "provider",
			OccurredAt: "2026-01-18T00:01:00.000Z",
			DedupKey:   "codebuff-cost-only",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(got)
	assert.True(got.HasCost, "cost-only Codebuff row must still report HasCost")
	assert.Equal(reportedCost, got.Cost)
	assert.False(got.HasTokenData,
		"a cost-only Codebuff row is not token data; the hasRows || "+
			"sess.HasTotalOutputTokens || sess.HasPeakContextTokens "+
			"short-circuit in (*Store).sessionUsage is the regression "+
			"this test pins")
}

// TestDuckDBTokenRowsWithoutSessionFlagsMatchSQLite pins HasTokenData
// parity with SQLite and PostgreSQL for the inverse of the cost-only
// case: a session whose usage_events rows DO carry billable tokens but
// whose session row has HasTotalOutputTokens and HasPeakContextTokens
// false. SQLite (internal/db) and PostgreSQL compute HasTokenData from
// the session flags alone, so both report false here; DuckDB must
// agree instead of deriving true from the token-bearing rows.
func TestDuckDBTokenRowsWithoutSessionFlagsMatchSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	// syncSession leaves TotalOutputTokens/PeakContextTokens and both
	// Has* flags at their zero values, and with no messages in the
	// batch the sanitizer keeps them false even though the usage
	// event below carries tokens.
	sess := syncSession(
		"duck-flags-off", "alpha", "tokens without flags",
		"2026-01-18T00:00:00.000Z", 0)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: sess,
		UsageEvents: []db.UsageEvent{{
			Source: "shutdown", Model: "claude-sonnet-4-6",
			InputTokens: 1000, OutputTokens: 500,
			OccurredAt: "2026-01-18T00:01:00.000Z",
			DedupKey:   "flags-off",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	want, err := local.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(want)
	require.False(want.HasTokenData,
		"SQLite computes HasTokenData from session flags only")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetSessionUsage(ctx, sess.ID, true)
	require.NoError(err)
	require.NotNil(got)
	assert.False(got.HasTokenData,
		"DuckDB must compute HasTokenData from session flags only, "+
			"not from token-bearing usage rows, to match SQLite and "+
			"PostgreSQL")
	assert.Equal(want.HasTokenData, got.HasTokenData)
}

func TestDailyUsageCostsReasoningOnlyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "reasoning-only",
		OutputPerMTok: money.MustParseDollars("2"),
	}}))

	sessionID := "duck-reasoning-only"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "reasoning only",
			"2026-01-19T00:00:00.000Z", 0),
		UsageEvents: []db.UsageEvent{{
			Source:          "provider",
			Model:           "reasoning-only",
			ReasoningTokens: 300,
			OccurredAt:      "2026-01-19T00:01:00.000Z",
			DedupKey:        "reasoning-only",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}
	wantCost := money.MustParseDollars("0.0006")

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Zero(daily.Totals.OutputTokens,
		"reasoning-only rows do not change reported output-token totals")
	assert.Equal(wantCost, daily.Totals.TotalCost)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal(wantCost, top[0].Cost)

	sessionUsage, err := store.GetSessionUsage(ctx, sessionID, true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal(wantCost, sessionUsage.Cost)
	require.Len(sessionUsage.Breakdown, 1,
		"reasoning-only rows must appear in the breakdown")
	entry := sessionUsage.Breakdown[0]
	assert.Equal("provider", entry.Source)
	assert.Zero(entry.OutputTokens,
		"reasoning stays out of reported output tokens")
	assert.True(entry.HasCost)
	assert.Equal(wantCost, entry.Cost,
		"reasoning-only breakdown row bills at the output rate")
}

func TestDailyUsageCostsMessageReasoningTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "reasoning-model",
		InputPerMTok:  money.MustParseDollars("1"),
		OutputPerMTok: money.MustParseDollars("2"),
	}}))

	msg := syncMessage(
		"duck-message-reasoning", 0, "assistant", "message reasoning",
		"2026-01-19T00:01:00.000Z")
	msg.Model = "reasoning-model"
	msg.TokenUsage = jsontext.Value(
		`{"input_tokens":1000,"output_tokens":0,"reasoning_tokens":500}`)
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			"duck-message-reasoning", "alpha", "message reasoning",
			"2026-01-19T00:00:00.000Z", 1),
		Messages:        []db.Message{msg},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(1000, daily.Totals.InputTokens)
	assert.Zero(daily.Totals.OutputTokens)
	assert.Equal(money.MustParseDollars("0.002"), daily.Totals.TotalCost)

	sessionUsage, err := store.GetSessionUsage(ctx, "duck-message-reasoning", true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal(money.MustParseDollars("0.002"), sessionUsage.Cost)
	require.Len(sessionUsage.Breakdown, 1,
		"reasoning-bearing message must appear in the breakdown")
	entry := sessionUsage.Breakdown[0]
	assert.Equal("message", entry.Source)
	assert.Equal(1000, entry.InputTokens)
	assert.Zero(entry.OutputTokens)
	assert.True(entry.HasCost)
	assert.Equal(money.MustParseDollars("0.002"), entry.Cost,
		"breakdown cost must include reasoning billed as output")
}

func TestDailyUsageCostsMixedOutputAndReasoningOnlyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "reasoning-mix",
		OutputPerMTok: money.MustParseDollars("2"),
	}}))

	sessionID := "duck-reasoning-mixed"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			sessionID, "alpha", "reasoning mixed",
			"2026-01-19T00:00:00.000Z", 0),
		UsageEvents: []db.UsageEvent{
			{
				Source:          "provider",
				Model:           "reasoning-mix",
				OutputTokens:    100,
				ReasoningTokens: 20,
				OccurredAt:      "2026-01-19T00:01:00.000Z",
				DedupKey:        "normal-output",
			},
			{
				Source:          "provider",
				Model:           "reasoning-mix",
				ReasoningTokens: 300,
				OccurredAt:      "2026-01-19T00:02:00.000Z",
				DedupKey:        "reasoning-only",
			},
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}
	wantCost := money.MustParseDollars("0.0008")

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(100, daily.Totals.OutputTokens,
		"reasoning-only rows do not change reported output-token totals")
	assert.Equal(wantCost, daily.Totals.TotalCost)
	require.NotNil(daily.Pricing)
	assert.Equal(export.CostSourceComputed,
		daily.Pricing.Models["reasoning-mix"].CostSource)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal(wantCost, top[0].Cost)

	sessionUsage, err := store.GetSessionUsage(ctx, sessionID, true)
	require.NoError(err)
	require.NotNil(sessionUsage)
	assert.True(sessionUsage.HasCost)
	assert.Equal(wantCost, sessionUsage.Cost)
	require.Len(sessionUsage.Breakdown, 2,
		"both output and reasoning-only rows must appear in the breakdown")
	breakdownCost := money.Money{}
	for _, entry := range sessionUsage.Breakdown {
		assert.True(entry.HasCost)
		breakdownCost = money.MustAdd(breakdownCost, entry.Cost)
	}
	assert.Equal(sessionUsage.Cost, breakdownCost,
		"breakdown costs must sum to the session cost")
}

func TestUsageDedupPrefersInRangeDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))

	before := syncMessage("duck-usage-edge-a", 0, "assistant", "before midnight", "2026-01-12T23:30:00.000Z")
	before.ClaudeMessageID = "edge-message"
	before.ClaudeRequestID = "edge-request"
	after := syncMessage("duck-usage-edge-b", 0, "assistant", "after midnight", "2026-01-13T00:30:00.000Z")
	after.ClaudeMessageID = "edge-message"
	after.ClaudeRequestID = "edge-request"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         syncSession("duck-usage-edge-a", "alpha", "edge a", "2026-01-12T23:30:00.000Z", 1),
			Messages:        []db.Message{before},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         syncSession("duck-usage-edge-b", "alpha", "edge b", "2026-01-13T00:30:00.000Z", 1),
			Messages:        []db.Message{after},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	// The duplicate before midnight is outside the window but inside
	// the padded UTC bounds and sorts first by timestamp. It must not
	// win the dedup and suppress the in-range duplicate.
	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
	})
	require.NoError(err)
	assert.Equal(1, got.Totals.InputTokens)
	assert.Equal(2, got.Totals.OutputTokens)
}

func TestPushSyncsCursorUsageEventsIntoDuckDBDailyUsage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.InsertCursorUsageEvents([]db.CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 12,
		CacheReadTokens:  34,
		Charged:          money.MustParseDollars("0.1566"),
		CursorTokenFee:   money.MustParseDollars("0.0332"),
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(err)
	assertDuckDBCount(t, syncer.DB(), "cursor_usage_events", 1)

	store := NewStoreFromDB(syncer.DB())
	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(err)
	require.Len(result.Daily, 1)
	assert.Equal(1234, result.Daily[0].InputTokens)
	assert.Equal(567, result.Daily[0].OutputTokens)
	assert.Equal(12, result.Daily[0].CacheCreationTokens)
	assert.Equal(34, result.Daily[0].CacheReadTokens)
	assert.Equal(money.MustParseDollars("0.1566"), result.Daily[0].TotalCost)
	assert.Empty(result.Projects, "cursor-only usage should not emit project identities")
	assert.NotContains(result.Projects, "")
	assert.Equal(0, result.SessionCounts.Total)
	assert.Empty(result.SessionCounts.ByAgent)
	assert.Empty(result.SessionCounts.ByProject)
	require.Len(result.Daily[0].AgentBreakdowns, 1)
	assert.Equal("cursor", result.Daily[0].AgentBreakdowns[0].Agent)
}

func TestTrendsTermsWordBoundaryAndOverlapParity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	start := "2026-01-22T09:00:00.000Z"
	content := "seam seams seamless testing test attest"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession("duck-trend-parity", "alpha", "trend parity", start, 1),
		Messages: []db.Message{
			syncMessage("duck-trend-parity", 0, "user", content, start),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	terms, err := db.ParseTrendTerms([]string{"seam", "test|testing"})
	require.NoError(err)
	filter := db.AnalyticsFilter{
		From: "2026-01-22", To: "2026-01-22", Timezone: "UTC",
	}

	got, err := store.GetTrendsTerms(ctx, filter, terms, "day")
	require.NoError(err)
	require.Len(got.Series, 2)
	// Word-bounded: "seamless" does not count for "seam", and
	// "testing" is not double-counted via its "test" substring.
	assert.Equal(2, got.Series[0].Total)
	assert.Equal(2, got.Series[1].Total)

	want, err := local.GetTrendsTerms(ctx, filter, terms, "day")
	require.NoError(err)
	require.Len(want.Series, 2)
	assert.Equal(want.Series[0].Total, got.Series[0].Total)
	assert.Equal(want.Series[1].Total, got.Series[1].Total)
}

func TestDailyUsageBreakdownsAndCacheSavings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         money.MustParseDollars("3"),
		OutputPerMTok:        money.MustParseDollars("15"),
		CacheCreationPerMTok: money.MustParseDollars("1"),
		CacheReadPerMTok:     money.MustParseDollars("0.5"),
	}}))
	sessionID := "duck-usage-breakdowns"
	primarySession := syncSession(
		sessionID, "alpha", "usage first", "2026-01-17T00:00:00.000Z", 1,
	)
	primarySession.Machine = "host-a"
	secondaryID := "duck-usage-breakdowns-secondary"
	secondarySession := syncSession(
		secondaryID, "alpha", "usage second", "2026-01-17T00:02:00.000Z", 1,
	)
	secondarySession.Machine = "host-b"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:  primarySession,
		Messages: []db.Message{syncMessage(sessionID, 0, "user", "usage first", "2026-01-17T00:00:00.000Z")},
		UsageEvents: []db.UsageEvent{{
			Source:               "hermes",
			Model:                "claude-test",
			InputTokens:          10,
			OutputTokens:         5,
			CacheReadInputTokens: 4,
			OccurredAt:           "2026-01-17T00:01:00.000Z",
			DedupKey:             "breakdown",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}, {
		Session:  secondarySession,
		Messages: []db.Message{syncMessage(secondaryID, 0, "user", "usage second", "2026-01-17T00:02:00.000Z")},
		UsageEvents: []db.UsageEvent{{
			Source:       "hermes",
			Model:        "claude-test",
			InputTokens:  2,
			OutputTokens: 1,
			OccurredAt:   "2026-01-17T00:03:00.000Z",
			DedupKey:     "breakdown-secondary",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET machine = CASE id
			WHEN ? THEN 'host-a'
			WHEN ? THEN 'host-b'
			ELSE machine
		END`, sessionID, secondaryID)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-01-01",
		To:         "2026-01-31",
		Breakdowns: true,
	})
	require.NoError(err)
	require.Len(got.Daily, 1)
	day := got.Daily[0]
	require.Len(day.ModelBreakdowns, 1)
	require.Len(day.ProjectBreakdowns, 1)
	require.Len(day.AgentBreakdowns, 1)
	require.Len(day.MachineBreakdowns, 2)
	assert.Equal("alpha", day.ProjectBreakdowns[0].Project)
	assert.Equal("claude", day.AgentBreakdowns[0].Agent)
	assert.Equal("host-a", day.MachineBreakdowns[0].MachineName)
	assert.Equal("host-b", day.MachineBreakdowns[1].MachineName)
	assert.Equal(day.TotalCost,
		money.MustAdd(day.MachineBreakdowns[0].Cost, day.MachineBreakdowns[1].Cost))
	assert.Equal(money.MustParseDollars("0.00001"), got.Totals.CacheSavings)

	noCounts, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:              "2026-01-01",
		To:                "2026-01-31",
		SkipSessionCounts: true,
	})
	require.NoError(err)
	assert.Equal(got.Totals.InputTokens, noCounts.Totals.InputTokens)
	assert.Zero(noCounts.SessionCounts.Total)
	assert.Nil(noCounts.SessionCounts.ByProject)
	assert.Nil(noCounts.SessionCounts.ByAgent)
}

func TestGetChildSessionsOrderedByStartedAt(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)

	parent := syncSession("duck-parent", "alpha", "parent first", "2026-01-10T00:00:00.000Z", 1)
	early := syncSession("duck-child-early", "alpha", "early child", "2026-01-10T01:00:00.000Z", 1)
	late := syncSession("duck-child-late", "alpha", "late child", "2026-01-10T02:00:00.000Z", 1)
	deleted := syncSession("duck-child-deleted", "alpha", "deleted child", "2026-01-10T01:30:00.000Z", 1)
	parentID := parent.ID
	for _, child := range []*db.Session{&early, &late, &deleted} {
		child.ParentSessionID = &parentID
		child.RelationshipType = "subagent"
	}

	writes := make([]db.SessionBatchWrite, 0, 4)
	for _, sess := range []db.Session{parent, early, late, deleted} {
		writes = append(writes, db.SessionBatchWrite{
			Session:         sess,
			Messages:        []db.Message{syncMessage(sess.ID, 0, "user", *sess.FirstMessage, *sess.StartedAt)},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)
	require.NoError(local.SoftDeleteSession("duck-child-deleted"))

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	children, err := store.GetChildSessions(ctx, "duck-parent")
	require.NoError(err)
	assert.Equal(t,
		[]string{"duck-child-early", "duck-child-late"},
		duckSessionIDs(children))
}

func TestStoreSessionUsageRollupParity(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-test", InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
	}}))
	root := syncSession("duck-rollup-root", "alpha", "root", "2026-01-10T00:00:00.000Z", 1)
	continuation := syncSession("duck-rollup-continuation", "alpha", "continuation", "2026-01-10T00:30:00.000Z", 0)
	child := syncSession("duck-rollup-child", "alpha", "child", "2026-01-10T01:00:00.000Z", 1)
	parentID := root.ID
	continuationParentID := continuation.ID
	continuation.ParentSessionID = &parentID
	continuation.RelationshipType = "continuation"
	child.ParentSessionID = &continuationParentID
	child.RelationshipType = "subagent"
	rootMessage := syncMessage(root.ID, 0, "assistant", "root", *root.StartedAt)
	childMessage := syncMessage(child.ID, 0, "assistant", "child", *child.StartedAt)
	childUnique := syncMessage(child.ID, 1, "assistant", "child unique", "2026-01-10T01:05:00.000Z")
	rootMessage.ClaudeMessageID, rootMessage.ClaudeRequestID = "duck-rollup-shared", "duck-rollup-request"
	childMessage.ClaudeMessageID, childMessage.ClaudeRequestID = "duck-rollup-shared", "duck-rollup-request"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{Session: root, Messages: []db.Message{rootMessage}, DataVersion: 1, ReplaceMessages: true},
		{Session: continuation, DataVersion: 1, ReplaceMessages: true},
		{Session: child, Messages: []db.Message{childMessage, childUnique}, DataVersion: 1, ReplaceMessages: true},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	rollup, err := service.GetSessionUsageRollup(
		ctx, NewStoreFromDB(syncer.DB()), root.ID, false,
	)
	require.NoError(err)
	require.Equal(1, rollup.SubagentCount)
	require.True(rollup.HasCost)
	assert.Equal(t, money.MustParseDollars("0.000066"), rollup.Cost)
}

func TestStoreSessionUsageRollupUsesCopilotReportedSessionCost(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "gpt-5.1", InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
	}}))
	root := syncSession(
		"duck-copilot-rollup-root", "alpha", "root",
		"2026-01-10T00:00:00.000Z", 1)
	root.Agent = "copilot"
	child := syncSession(
		"duck-copilot-rollup-child", "alpha", "child",
		"2026-01-10T01:00:00.000Z", 1)
	child.Agent = "copilot"
	parentID := root.ID
	child.ParentSessionID = &parentID
	child.RelationshipType = "subagent"
	reportedRootCost := money.MustParseDollars("0.03")
	reportedChildCost := money.MustParseDollars("0.02")
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: root,
			UsageEvents: []db.UsageEvent{
				{
					Source: "shutdown", Model: "gpt-5.1",
					InputTokens: 1000, OutputTokens: 500,
					OccurredAt: "2026-01-10T00:01:00.000Z", DedupKey: "first",
				},
				{
					Source: "shutdown", Model: "gpt-5.1",
					InputTokens: 1000, OutputTokens: 500,
					Cost: &reportedRootCost, CostStatus: "exact",
					CostSource: db.CopilotReportedCostSource,
					OccurredAt: "2026-01-10T00:02:00.000Z", DedupKey: "final",
				},
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: child,
			UsageEvents: []db.UsageEvent{{
				Source: "provider", Model: "gpt-5.1",
				Cost: &reportedChildCost, CostStatus: "exact", CostSource: "provider",
				OccurredAt: "2026-01-10T01:01:00.000Z", DedupKey: "child",
			}},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	rollup, err := service.GetSessionUsageRollup(
		ctx, NewStoreFromDB(syncer.DB()), root.ID, false)
	require.NoError(err)
	require.True(rollup.HasCost)
	assert.Equal(t, money.MustAdd(reportedRootCost, reportedChildCost),
		rollup.Cost)
}

func TestStoreSessionUsageRollupIncludesUntimedRows(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-test", InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
	}}))
	root := syncSession("duck-rollup-untimed-root", "alpha", "root", "2026-01-10T00:00:00.000Z", 1)
	child := syncSession("duck-rollup-untimed-child", "alpha", "child", "2026-01-10T01:00:00.000Z", 1)
	parentID := root.ID
	child.ParentSessionID = &parentID
	child.RelationshipType = "subagent"
	rootMessage := syncMessage(root.ID, 0, "assistant", "root", "")
	childMessage := syncMessage(child.ID, 0, "assistant", "child", "")
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         root,
			Messages:        []db.Message{rootMessage},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         child,
			Messages:        []db.Message{childMessage},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	rollup, err := service.GetSessionUsageRollup(
		ctx, NewStoreFromDB(syncer.DB()), root.ID, false,
	)
	require.NoError(err)
	require.Equal(1, rollup.SubagentCount)
	require.True(rollup.HasCost)
	assert.Equal(t, money.MustParseDollars("0.000066"), rollup.Cost)
}

func TestStoreSessionUsageRollupHandlesNullMessageAndSessionTimestamps(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-test", InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
	}}))
	root := syncSession("duck-rollup-null-ts-root", "alpha", "root", "2026-01-10T00:00:00.000Z", 1)
	child := syncSession("duck-rollup-null-ts-child", "alpha", "child", "2026-01-10T01:00:00.000Z", 1)
	parentID := root.ID
	child.ParentSessionID = &parentID
	child.RelationshipType = "subagent"
	child.StartedAt = nil
	rootMessage := syncMessage(root.ID, 0, "assistant", "root", *root.StartedAt)
	childMessage := syncMessage(child.ID, 0, "assistant", "child", "")
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session:         root,
			Messages:        []db.Message{rootMessage},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session:         child,
			Messages:        []db.Message{childMessage},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)

	rollup, err := service.GetSessionUsageRollup(
		ctx, NewStoreFromDB(syncer.DB()), root.ID, false,
	)
	require.NoError(err)
	require.Equal(1, rollup.SubagentCount)
	require.True(rollup.HasCost)
	assert.Equal(t, money.MustParseDollars("0.000066"), rollup.Cost)
}

// TestDuckGetAnalyticsSkillsAggregatesAcrossWeeks exercises the SQL
// pushdown path: COUNT(*) aggregation per message timestamp and trend
// buckets spread across the weeks a skill was actually used.
func TestDuckGetAnalyticsSkillsAggregatesAcrossWeeks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)

	const sid = "dk-multi"
	skill := func(use string) db.ToolCall {
		return db.ToolCall{
			ToolName: "Skill", Category: "Skill",
			SkillName: "deploy", ToolUseID: use,
		}
	}
	writes := []db.SessionBatchWrite{{
		Session: syncSession(sid, "alpha", "first",
			"2026-01-06T09:00:00.000Z", 3),
		Messages: []db.Message{
			syncMessage(sid, 0, "user", "go", "2026-01-06T09:00:00.000Z"),
			syncMessage(sid, 1, "assistant", "two calls",
				"2026-01-06T10:00:00.000Z",
				skill("tu-1"), skill("tu-2")),
			syncMessage(sid, 2, "assistant", "one call",
				"2026-01-20T10:00:00.000Z",
				skill("tu-3")),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	resp, err := store.GetAnalyticsSkills(ctx, db.AnalyticsFilter{
		From: "2026-01-01", To: "2026-01-31", Timezone: "UTC",
	}, "week")
	require.NoError(err, "GetAnalyticsSkills")
	require.Len(resp.BySkill, 1, "BySkill")
	assert.Equal("deploy", resp.BySkill[0].SkillName)
	assert.Equal(3, resp.BySkill[0].CallCount, "CallCount")
	assert.Equal(1, resp.BySkill[0].SessionCount, "SessionCount")
	assert.Equal("2026-01-20T10:00:00Z", resp.BySkill[0].LastUsedAt,
		"LastUsedAt is the latest message timestamp")

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if c := e.BySkill["deploy"]; c > 0 {
			trend[e.Date] += c
		}
	}
	assert.Equal(map[string]int{"2026-01-05": 2, "2026-01-19": 1}, trend,
		"calls bucket into their own message-timestamp weeks")
}

// TestDuckGetAnalyticsSkillsFiltersByMessageDate checks that the date
// filter applies to each call's message timestamp, not the session start:
// a session that started before the range still contributes its in-range
// call, and its out-of-range calls are dropped.
func TestDuckGetAnalyticsSkillsFiltersByMessageDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)

	const sid = "dk-span"
	skill := func() db.ToolCall {
		return db.ToolCall{
			ToolName: "Skill", Category: "Skill", SkillName: "deploy",
		}
	}
	writes := []db.SessionBatchWrite{{
		Session: syncSession(sid, "alpha", "first",
			"2026-01-20T09:00:00.000Z", 4),
		Messages: []db.Message{
			syncMessage(sid, 0, "user", "go", "2026-01-20T09:00:00.000Z"),
			syncMessage(sid, 1, "assistant", "before",
				"2026-01-25T10:00:00.000Z", skill()),
			syncMessage(sid, 2, "assistant", "inrange",
				"2026-02-10T10:00:00.000Z", skill()),
			syncMessage(sid, 3, "assistant", "after",
				"2026-03-05T10:00:00.000Z", skill()),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	resp, err := store.GetAnalyticsSkills(ctx, db.AnalyticsFilter{
		From: "2026-02-01", To: "2026-02-28", Timezone: "UTC",
	}, "week")
	require.NoError(err, "GetAnalyticsSkills")
	require.Len(resp.BySkill, 1, "BySkill")
	assert.Equal("deploy", resp.BySkill[0].SkillName)
	assert.Equal(1, resp.BySkill[0].CallCount,
		"only the in-range call counts")
	assert.Equal("2026-02-10T10:00:00Z", resp.BySkill[0].LastUsedAt)

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if c := e.BySkill["deploy"]; c > 0 {
			trend[e.Date] += c
		}
	}
	assert.Equal(map[string]int{"2026-02-09": 1}, trend,
		"only the in-range week is bucketed")
}

func newSyncedStore(t *testing.T) (*Store, syncFixture) {
	t.Helper()
	ctx := t.Context()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB()), fixture
}

func TestDuckTopSessionsRanksBySelectedTokenTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession(
				"input-heavy", "demo", "input-heavy",
				"2026-02-01T12:00:00Z", 1,
			),
			UsageEvents: []db.UsageEvent{{
				Source: "provider", Model: "model",
				InputTokens: 1000, OutputTokens: 1,
				OccurredAt: "2026-02-01T12:01:00Z",
				DedupKey:   "input-heavy",
			}},
			DataVersion: 1,
		},
		{
			Session: syncSession(
				"output-heavy", "demo", "output-heavy",
				"2026-02-01T13:00:00Z", 1,
			),
			UsageEvents: []db.UsageEvent{{
				Source: "provider", Model: "model",
				InputTokens: 10, OutputTokens: 50,
				OccurredAt: "2026-02-01T13:01:00Z",
				DedupKey:   "output-heavy",
			}},
			DataVersion: 1,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From:                  "2026-02-01",
		To:                    "2026-02-01",
		Timezone:              "UTC",
		TopSessionsSort:       db.TopSessionsSortTokens,
		TopSessionsTokenTypes: db.UsageTokenTypeOutput,
	}, 1)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal("output-heavy", top[0].SessionID)
	assert.Equal(10, top[0].InputTokens)
	assert.Equal(50, top[0].OutputTokens)
	assert.Equal(60, top[0].TotalTokens)
}

func TestDuckUsageQuantizesCostBeforeAggregation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "sub-micro-model",
		InputPerMTok: money.Money{Microdollars: 400_000},
	}}))
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			"sub-micro-session", "alpha", "sub-micro", "2026-02-01T12:00:00Z", 1,
		),
		Messages: []db.Message{{
			SessionID: "sub-micro-session", Ordinal: 0, Role: "user",
			Content: "sub-micro", ContentLength: len("sub-micro"),
			Timestamp: "2026-02-01T12:00:00Z",
		}},
		UsageEvents: []db.UsageEvent{
			{
				Source: "session", Model: "sub-micro-model", InputTokens: 1,
				OccurredAt: "2026-02-01T12:01:00Z", DedupKey: "sub-micro-1",
			},
			{
				Source: "session", Model: "sub-micro-model", InputTokens: 1,
				OccurredAt: "2026-02-01T12:02:00Z", DedupKey: "sub-micro-2",
			},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	filter := db.UsageFilter{
		From: "2026-02-01", To: "2026-02-01", Timezone: "UTC",
	}
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(err)
	assert.Equal(money.Money{}, daily.Totals.TotalCost)
	assert.Equal(1, daily.SessionCounts.Total)
	assert.Equal(1, daily.SessionCounts.ByAgent["claude"])

	top, err := store.GetTopSessionsByCost(ctx, filter, 1)
	require.NoError(err)
	require.Len(top, 1)
	assert.Equal(money.Money{}, top[0].Cost)

	usage, err := store.GetSessionUsage(ctx, "sub-micro-session", false)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(money.Money{}, usage.Cost)
}

func TestDuckDailyUsageReturnsAggregateCostOverflow(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	large := money.Money{Microdollars: 1 << 62}
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: syncSession(
			"duck-overflow", "alpha", "overflow", "2026-02-01T12:00:00Z", 1,
		),
		Messages: []db.Message{syncMessage(
			"duck-overflow", 0, "user", "overflow", "2026-02-01T12:00:00Z",
		)},
		UsageEvents: []db.UsageEvent{
			{
				Source: "provider", Model: "model", Cost: &large,
				OccurredAt: "2026-02-01T12:01:00Z", DedupKey: "overflow-1",
			},
			{
				Source: "provider", Model: "model", Cost: &large,
				OccurredAt: "2026-02-01T12:02:00Z", DedupKey: "overflow-2",
			},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	_, err = store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-02-01", To: "2026-02-01", Timezone: "UTC",
	})

	require.ErrorIs(err, money.ErrOverflow)
}

func TestDuckDBBranchDimension(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "claude-test", InputPerMTok: money.MustParseDollars("3"), OutputPerMTok: money.MustParseDollars("15"),
	}}))

	seed := []struct {
		id, project, branch string
		input, output       int
	}{
		{"d-a", "alpha", "main", 100, 10},
		{"d-b", "alpha", "feature-x", 200, 20},
		{"d-c", "beta", "main", 300, 30},
		{"d-d", "alpha", "", 400, 40},
		{"d-e", "alpha", "unknown", 500, 50},
	}
	var writes []db.SessionBatchWrite
	for _, s := range seed {
		sess := syncSession(s.id, s.project, s.id+" first", "2026-02-01T12:00:00.000Z", 1)
		sess.GitBranch = s.branch
		writes = append(writes, db.SessionBatchWrite{
			Session: sess,
			// A token-free user message so only the usage event below feeds the
			// usage totals (syncMessage would inject a stray input token).
			Messages: []db.Message{{
				SessionID:     s.id,
				Ordinal:       0,
				Role:          "user",
				Content:       s.id + " first",
				Timestamp:     "2026-02-01T12:00:00.000Z",
				ContentLength: len(s.id + " first"),
			}},
			UsageEvents: []db.UsageEvent{{
				Source: "session", Model: "claude-test",
				InputTokens: s.input, OutputTokens: s.output,
				OccurredAt: "2026-02-01T12:01:00.000Z", DedupKey: s.id + "-usage",
			}},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(err)
	store := NewStoreFromDB(syncer.DB())

	branches, err := store.GetBranches(ctx, false, false)
	require.NoError(err)
	assert.Equal([]db.BranchInfo{
		{
			Project: "alpha",
			Branch:  "",
			Token:   db.EncodeBranchFilterToken("alpha", ""),
		},
		{
			Project: "alpha",
			Branch:  "feature-x",
			Token:   db.EncodeBranchFilterToken("alpha", "feature-x"),
		},
		{
			Project: "alpha",
			Branch:  "main",
			Token:   db.EncodeBranchFilterToken("alpha", "main"),
		},
		{
			Project: "alpha",
			Branch:  "unknown",
			Token:   db.EncodeBranchFilterToken("alpha", "unknown"),
		},
		{
			Project: "beta",
			Branch:  "main",
			Token:   db.EncodeBranchFilterToken("beta", "main"),
		},
	}, branches)

	filtered, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-01", To: "2026-12-31",
		GitBranch: db.EncodeBranchFilterToken("alpha", "main"),
	})
	require.NoError(err)
	total := 0
	for _, day := range filtered.Daily {
		total += day.InputTokens
	}
	assert.Equal(100, total, "branch filter restricts usage to alpha/main")
}

func TestSearch_DateRange(t *testing.T) {
	fixtures := []struct{ id, start, end string }{
		{"early", "2024-06-01T10:00:00Z", "2024-06-01T11:00:00Z"},
		{"boundary", "2024-06-02T23:59:59Z", "2024-06-02T23:59:59Z"},
		{"late", "2024-06-03T00:00:00Z", "2024-06-03T01:00:00Z"},
		{"spanning", "2024-06-01T23:00:00Z", "2024-06-03T01:00:00Z"},
	}
	var writes []db.SessionBatchWrite
	for _, f := range fixtures {
		sess := syncSession(f.id, "project-a", "seed", f.start, 1)
		sess.EndedAt = new(f.end)
		sess.SessionName = new("datefilter name")
		writes = append(writes, db.SessionBatchWrite{
			Session:     sess,
			Messages:    []db.Message{syncMessage(f.id, 0, "user", "datefilter message", f.start)},
			DataVersion: 1, ReplaceMessages: true,
		})
	}
	store := newUnitsStore(t, writes)
	for _, tc := range []struct {
		name, from, to string
		want           []string
	}{
		{"omitted", "", "", []string{"early", "boundary", "late", "spanning"}},
		{"lower only", "2024-06-02", "", []string{"boundary", "late", "spanning"}},
		{"upper only", "", "2024-06-02", []string{"early", "boundary", "spanning"}},
		{"same day", "2024-06-02", "2024-06-02", []string{"boundary", "spanning"}},
		{"no matches", "2024-06-04", "2024-06-04", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, query := range []string{"message", "name"} {
				filter := db.SearchFilter{Query: query, Project: "project-a", DateFrom: tc.from, DateTo: tc.to, Limit: 1}
				var ids []string
				for range len(fixtures) + 1 {
					out, err := store.Search(t.Context(), filter)
					require.NoError(t, err)
					for _, hit := range out.Results {
						ids = append(ids, hit.SessionID)
					}
					if out.NextCursor == 0 {
						break
					}
					filter.Cursor = out.NextCursor
				}
				assert.ElementsMatch(t, tc.want, ids, "query %s", query)
			}
		})
	}
}

func TestGetSessionTimingActivityTimingParity(t *testing.T) {
	type execution struct {
		category, start, end string
		wantDuration         *int64
	}
	for _, tc := range []struct {
		name                                                 string
		executions                                           []execution
		noPrompt, staleEnd, carriers, openChild, closedChild bool
		legacyPrefix                                         bool
		wantDuration, wantTool, wantUnattributed             int64
		wantCategories                                       []db.CategoryTotal
	}{
		{name: "measured thinking followed by tool", executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "missing execution", executions: []execution{{category: "Bash"}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "same and cross category overlap", executions: []execution{{"Bash", "02", "04", new(int64(2000))}, {"Bash", "03", "05", new(int64(2000))}, {"Read", "04", "06", new(int64(2000))}}, wantDuration: 6000, wantTool: 4000, wantUnattributed: 2000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 3000, CallCount: 2}, {Category: "Read", DurationMs: 2000, CallCount: 1}}},
		{name: "stale session end", staleEnd: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 1000, wantUnattributed: 1000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "no visible prompt", noPrompt: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantTool: 2000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "system and tool result carriers", carriers: true, executions: []execution{{"Bash", "02", "04", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Bash", DurationMs: 2000, CallCount: 1}}},
		{name: "open child", openChild: true, executions: []execution{{category: "Task"}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Task", CallCount: 1}}},
		{name: "legacy prefix and child precedence", closedChild: true, legacyPrefix: true, executions: []execution{{"Task", "01", "01.500", new(int64(2000))}}, wantDuration: 6000, wantTool: 2000, wantUnattributed: 4000, wantCategories: []db.CategoryTotal{{Category: "Task", DurationMs: 2000, CallCount: 1}}},
		{name: "zero execution", executions: []execution{{"Bash", "02", "02", new(int64(0))}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "backward execution", executions: []execution{{"Bash", "04", "02", nil}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
		{name: "open execution", executions: []execution{{"Bash", "02", "", nil}}, wantDuration: 6000, wantUnattributed: 6000, wantCategories: []db.CategoryTotal{{Category: "Bash", CallCount: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			local := newLocalDB(t)
			const sessionID = "duck-timing-activity"
			end := "2026-04-26T10:00:06Z"
			if tc.staleEnd {
				end = "2026-04-26T10:00:01Z"
			}
			sess := syncSession(sessionID, "timing", "go", "2026-04-26T10:00:00Z", 3)
			sess.EndedAt = &end
			role := "user"
			if tc.noPrompt {
				role = "assistant"
			}
			var calls []db.ToolCall
			for i, execution := range tc.executions {
				id := fmt.Sprintf("call-%d", i)
				call := db.ToolCall{ToolUseID: id, ToolName: execution.category, Category: execution.category, CallIndex: i, InputJSON: "{}"}
				if tc.openChild || tc.closedChild {
					call.SubagentSessionID = "duck-timing-child"
				}
				for _, event := range []struct{ status, timestamp string }{{"started", execution.start}, {"completed", execution.end}} {
					if event.timestamp == "" {
						continue
					}
					call.ResultEvents = append(call.ResultEvents, db.ToolResultEvent{ToolUseID: id, Source: "tool_execution", Status: event.status, Timestamp: "2026-04-26T10:00:" + event.timestamp + "Z"})
				}
				calls = append(calls, call)
			}
			messages := []db.Message{
				syncMessage(sessionID, 0, role, "go", "2026-04-26T10:00:00Z"),
				syncMessage(sessionID, 1, "assistant", "thinking then tools", "2026-04-26T10:00:01Z", calls...),
			}
			messages[1].HasThinking = true
			messages[1].ThinkingText = "considering"
			if tc.carriers {
				system := syncMessage(sessionID, 2, "user", "system", "2026-04-26T10:00:02Z")
				system.IsSystem = true
				result := syncMessage(sessionID, 3, "user", "result", "2026-04-26T10:00:03Z")
				result.SourceSubtype = "tool_result"
				messages = append(messages, system, result, syncMessage(sessionID, 4, "user", "", "2026-04-26T10:00:04Z"))
			}
			if tc.legacyPrefix {
				messages = append(messages, syncMessage(sessionID, 2, "user", "This session is being continued from another session.", "2026-04-26T10:00:05Z"))
			}
			if !tc.noPrompt && !tc.staleEnd {
				messages = append(messages, syncMessage(sessionID, 5, "user", "next", "2026-04-26T10:00:06Z"))
			}
			writes := []db.SessionBatchWrite{{Session: sess, Messages: messages, DataVersion: 1, ReplaceMessages: true}}
			if tc.openChild || tc.closedChild {
				child := syncSession("duck-timing-child", "timing", "child", "2026-04-26T10:00:02Z", 0)
				if tc.openChild {
					child.EndedAt = nil
				} else {
					childEnd := "2026-04-26T10:00:04Z"
					child.EndedAt = &childEnd
				}
				writes = append(writes, db.SessionBatchWrite{Session: child, DataVersion: 1, ReplaceMessages: true})
			}
			_, err := local.WriteSessionBatchAtomic(writes)
			require.NoError(t, err)
			syncer := newInMemoryTestSync(t, local, SyncOptions{})
			require.NoError(t, createSchema(ctx, syncer.DB()))
			_, err = syncer.pushEverything(ctx, nil)
			require.NoError(t, err)
			store := NewStoreFromDB(syncer.DB())
			got, err := store.GetSessionTiming(ctx, sessionID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantTool, got.ToolDurationMs)
			assert.Equal(t, db.ActivityTotals{ToolMs: tc.wantTool, UnattributedMs: tc.wantUnattributed}, got.ActivityTotals)
			assert.ElementsMatch(t, tc.wantCategories, got.ByCategory)
			assert.Equal(t, len(tc.executions), got.ToolCallCount)
			require.Len(t, got.Turns, 1)
			require.Len(t, got.Turns[0].Calls, len(tc.executions))
			for i, call := range tc.executions {
				assert.Equal(t, call.wantDuration, got.Turns[0].Calls[i].DurationMs)
			}
			if tc.openChild {
				assert.Equal(t, new("duck-timing-child"), got.Turns[0].Calls[0].SubagentSessionID)
			}
			sqlite, err := local.GetSessionTiming(ctx, sessionID)
			require.NoError(t, err)
			assert.Equal(t, sqlite.Activity, got.Activity)
			assert.Equal(t, sqlite.ActivityTotals, got.ActivityTotals)
			assert.Equal(t, sqlite.ByCategory, got.ByCategory)
			if tc.noPrompt {
				assert.Empty(t, got.Activity)
				return
			}
			count := 2
			if tc.staleEnd {
				count = 1
			}
			require.Len(t, got.Activity, count)
			assert.EqualValues(t, 0, got.Activity[0].Ordinal)
			assert.Equal(t, tc.wantDuration, got.Activity[0].DurationMs)
			assert.Equal(t, tc.wantTool, got.Activity[0].ToolMs)
			assert.Equal(t, tc.wantUnattributed, got.Activity[0].UnattributedMs)
			assert.False(t, got.Activity[0].Running)
		})
	}
}
