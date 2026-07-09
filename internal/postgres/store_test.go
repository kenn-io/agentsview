//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const testSchema = "agentsview_store_test"

// ensureStoreSchema creates the test schema and seed data.
func ensureStoreSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	_, err = pg.Exec(`
		DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE;
	`)
	require.NoError(t, err, "dropping schema")

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, testSchema), "creating schema")

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('store-test-001', 'test-machine',
			 'test-project', 'claude-code',
			 'hello world',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz,
			 2, 1)
	`)
	require.NoError(t, err, "inserting test session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('store-test-001', 0, 'user',
			 'hello world',
			 '2026-03-12T10:00:00Z'::timestamptz, 11),
			('store-test-001', 1, 'assistant',
			 'hi there',
			 '2026-03-12T10:00:01Z'::timestamptz, 8)
	`)
	require.NoError(t, err, "inserting test messages")
}

func ensureAnalyticsTokenStoreSchema(
	t *testing.T, pgURL string,
) {
	t.Helper()
	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	_, err = pg.Exec(`
		DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE;
	`)
	require.NoError(t, err, "dropping schema")

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, testSchema), "creating schema")

	_, err = pg.Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens
		) VALUES
			('pg-token-001', 'test-machine', 'proj-a', 'claude',
			 'largest token session',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz,
			 12, 6, 900, TRUE),
			('pg-token-002', 'test-machine', 'proj-a', 'codex',
			 'second token session',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:15:00Z'::timestamptz,
			 8, 4, 600, TRUE),
			('pg-token-003', 'test-machine', 'proj-b', 'claude',
			 'third token session',
			 '2026-03-13T09:00:00Z'::timestamptz,
			 '2026-03-13T09:10:00Z'::timestamptz,
			 5, 3, 300, TRUE),
			('pg-token-missing', 'test-machine', 'proj-c', 'claude',
			 'missing token coverage',
			 '2026-03-13T11:00:00Z'::timestamptz,
			 '2026-03-13T11:20:00Z'::timestamptz,
			 9, 5, 0, FALSE)
	`)
	require.NoError(t, err, "inserting analytics token sessions")
}

func sessionIDs(sessions []db.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestNewStore(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	assert.True(t, store.ReadOnly())
	assert.True(t, store.HasFTS())
}

func TestDetectInsightGenerationAvailability(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	require.NoError(t, store.DetectInsightGenerationAvailability(
		context.Background(),
	), "DetectInsightGenerationAvailability")
	assert.True(t, store.InsightGenerationAvailable())
}

func TestProbeInsightGenerationAvailabilityTx_ReadOnly(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	tx, err := pg.BeginTx(
		context.Background(),
		&sql.TxOptions{ReadOnly: true},
	)
	require.NoError(t, err, "BeginTx")
	defer func() { _ = tx.Rollback() }()

	available, err := probeInsightGenerationAvailabilityTx(
		context.Background(), tx,
	)
	require.NoError(t, err, "probeInsightGenerationAvailabilityTx")
	assert.False(t, available)
}

func TestStoreListSessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.ListSessions(
		ctx, db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "ListSessions")
	assert.NotZero(t, page.Total, "expected at least 1 session")
	t.Logf("sessions: %d, total: %d",
		len(page.Sessions), page.Total)
}

func TestStoreListSessions_MachineMultiSelect(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('store-test-002', 'machine-b',
			 'test-project', 'codex',
			 'hello machine b',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz,
			 2, 1),
			('store-test-003', 'machine-c',
			 'test-project', 'gemini',
			 'hello machine c',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:30:00Z'::timestamptz,
			 2, 1)
	`)
	require.NoError(t, err, "inserting extra sessions")

	ctx := context.Background()
	page, err := store.ListSessions(
		ctx,
		db.SessionFilter{
			Machine: "test-machine,machine-c",
			Limit:   10,
		},
	)
	require.NoError(t, err, "ListSessions")
	require.Equal(t, 2, page.Total)
	got := []string{
		page.Sessions[0].Machine,
		page.Sessions[1].Machine,
	}
	assert.Contains(t, got, "test-machine")
	assert.Contains(t, got, "machine-c")
}

func ensureSidebarIndexStoreSchema(
	t *testing.T, pgURL string,
) *Store {
	t.Helper()
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	_, err = store.DB().Exec(`DELETE FROM messages`)
	require.NoError(t, err, "clearing seed messages")
	_, err = store.DB().Exec(`DELETE FROM sessions`)
	require.NoError(t, err, "clearing seed sessions")
	return store
}

func insertSidebarIndexSession(
	t *testing.T,
	store *Store,
	id string,
	opts ...func(*sidebarIndexSessionSeed),
) {
	t.Helper()
	row := sidebarIndexSessionSeed{
		id:               id,
		machine:          "test-machine",
		project:          "sidebar-project",
		agent:            "claude",
		firstMessage:     "sidebar session",
		startedAt:        "2026-03-12T10:00:00Z",
		endedAt:          "2026-03-12T10:30:00Z",
		messageCount:     3,
		userMessageCount: 2,
	}
	for _, opt := range opts {
		opt(&row)
	}

	_, err := store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			display_name, started_at, ended_at, message_count,
			user_message_count, parent_session_id,
			relationship_type, is_automated, deleted_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7::timestamptz, $8::timestamptz, $9,
			$10, $11, $12, $13, $14::timestamptz
		)
	`, row.id, row.machine, row.project, row.agent,
		row.firstMessage, row.displayName, row.startedAt,
		row.endedAt, row.messageCount, row.userMessageCount,
		row.parentSessionID, row.relationshipType,
		row.isAutomated, row.deletedAt)
	require.NoError(t, err, "inserting sidebar index session %s", id)
}

type sidebarIndexSessionSeed struct {
	id               string
	machine          string
	project          string
	agent            string
	firstMessage     string
	displayName      *string
	startedAt        string
	endedAt          string
	messageCount     int
	userMessageCount int
	parentSessionID  *string
	relationshipType string
	isAutomated      bool
	deletedAt        *string
}

func sidebarIndexRowsByID(
	sessions []db.SidebarSessionIndexRow,
) map[string]db.SidebarSessionIndexRow {
	rows := make(map[string]db.SidebarSessionIndexRow, len(sessions))
	for _, s := range sessions {
		rows[s.ID] = s
	}
	return rows
}

func requireSidebarIndexIDs(
	t *testing.T,
	sessions []db.SidebarSessionIndexRow,
	wantIDs []string,
) {
	t.Helper()
	rows := sidebarIndexRowsByID(sessions)
	require.Len(t, rows, len(wantIDs), "session count; rows=%v", rows)
	for _, id := range wantIDs {
		_, ok := rows[id]
		require.True(t, ok, "session %q missing from rows=%v", id, rows)
	}
}

func sidebarIndexIDs(
	sessions []db.SidebarSessionIndexRow,
) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestStoreGetSidebarSessionIndexComputesIsTeammate(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	insertSidebarIndexSession(t, store, "teammate", func(
		s *sidebarIndexSessionSeed,
	) {
		s.firstMessage = `<teammate-message from="reviewer">hi`
	})
	insertSidebarIndexSession(t, store, "normal")

	index, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")

	rows := sidebarIndexRowsByID(index.Sessions)
	assert.True(t, rows["teammate"].IsTeammate, "teammate IsTeammate")
	assert.False(t, rows["normal"].IsTeammate, "normal IsTeammate")
}

func TestStoreGetSidebarSessionIndexReturnsDisplayName(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	displayName := "Named sidebar session"
	insertSidebarIndexSession(t, store, "named", func(
		s *sidebarIndexSessionSeed,
	) {
		s.displayName = &displayName
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, index.Sessions, 1)
	got := index.Sessions[0].DisplayName
	require.NotNil(t, got)
	assert.Equal(t, displayName, *got)
}

func TestStoreGetSidebarSessionIndexExcludeAutomated(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	insertSidebarIndexSession(t, store, "normal")
	insertSidebarIndexSession(t, store, "review", func(
		s *sidebarIndexSessionSeed,
	) {
		s.firstMessage = "You are a code reviewer. Review the code."
		s.userMessageCount = 1
		s.isAutomated = true
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(),
		db.SessionFilter{ExcludeAutomated: true},
	)
	require.NoError(t, err, "GetSidebarSessionIndex exclude automated")
	requireSidebarIndexIDs(t, index.Sessions, []string{"normal"})
	assert.Equal(t, 1, index.Total)

	index, err = store.GetSidebarSessionIndex(
		context.Background(),
		db.SessionFilter{ExcludeAutomated: false},
	)
	require.NoError(t, err, "GetSidebarSessionIndex include automated")
	requireSidebarIndexIDs(
		t, index.Sessions, []string{"normal", "review"},
	)
}

func TestStoreGetSidebarSessionIndexExcludeOneShotKeepsAutomated(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	insertSidebarIndexSession(t, store, "multi", func(
		s *sidebarIndexSessionSeed,
	) {
		s.userMessageCount = 5
	})
	insertSidebarIndexSession(t, store, "oneshot", func(
		s *sidebarIndexSessionSeed,
	) {
		s.userMessageCount = 1
	})
	insertSidebarIndexSession(t, store, "review", func(
		s *sidebarIndexSessionSeed,
	) {
		s.firstMessage = "You are a code reviewer. Review the code."
		s.userMessageCount = 1
		s.isAutomated = true
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(),
		db.SessionFilter{
			ExcludeOneShot:   true,
			ExcludeAutomated: false,
		},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(t, index.Sessions, []string{"multi", "review"})
}

func TestStoreGetSidebarSessionIndexIncludesChildrenForMatchingRoot(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	rootID := "root"
	subID := "sub"
	insertSidebarIndexSession(t, store, rootID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.agent = "claude"
		s.userMessageCount = 5
	})
	insertSidebarIndexSession(t, store, subID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.agent = "codex"
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
	})
	insertSidebarIndexSession(t, store, "fork", func(
		s *sidebarIndexSessionSeed,
	) {
		s.agent = "codex"
		s.parentSessionID = &subID
		s.relationshipType = "fork"
	})
	insertSidebarIndexSession(t, store, "other", func(
		s *sidebarIndexSessionSeed,
	) {
		s.agent = "codex"
		s.userMessageCount = 5
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{Agent: "claude"},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")
	requireSidebarIndexIDs(
		t, index.Sessions, []string{"root", "sub", "fork"},
	)
}

func TestStoreGetSidebarSessionIndexPagedExcludesAutomatedDescendants(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	rootID := "root"
	insertSidebarIndexSession(t, store, rootID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = "2024-01-03T00:00:00Z"
		s.userMessageCount = 5
	})
	insertSidebarIndexSession(t, store, "human-child", func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = "2024-01-02T00:00:00Z"
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
		s.userMessageCount = 1
	})
	insertSidebarIndexSession(t, store, "automated-child", func(
		s *sidebarIndexSessionSeed,
	) {
		s.firstMessage = "You are a code reviewer. Review the code."
		s.endedAt = "2024-01-01T00:00:00Z"
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
		s.userMessageCount = 1
		s.isAutomated = true
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(),
		db.SessionFilter{ExcludeAutomated: true, Limit: 1},
	)
	require.NoError(t, err, "GetSidebarSessionIndex exclude automated")
	requireSidebarIndexIDs(t, index.Sessions, []string{"root", "human-child"})

	index, err = store.GetSidebarSessionIndex(
		context.Background(),
		db.SessionFilter{ExcludeAutomated: false, Limit: 1},
	)
	require.NoError(t, err, "GetSidebarSessionIndex include automated")
	requireSidebarIndexIDs(
		t, index.Sessions, []string{"root", "human-child", "automated-child"},
	)
}

func TestStoreGetSidebarSessionIndexPaginatesContinuationsAsDescendants(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	rootEnd := "2024-01-01T00:00:00Z"
	continuationEnd := "2024-01-10T00:00:00Z"
	otherEnd := "2024-01-05T00:00:00Z"
	rootID := "root"
	insertSidebarIndexSession(t, store, rootID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = rootEnd
	})
	insertSidebarIndexSession(t, store, "continuation", func(
		s *sidebarIndexSessionSeed,
	) {
		s.parentSessionID = &rootID
		s.relationshipType = "continuation"
		s.endedAt = continuationEnd
	})
	insertSidebarIndexSession(t, store, "other", func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = otherEnd
	})

	ctx := context.Background()
	first, err := store.GetSidebarSessionIndex(
		ctx, db.SessionFilter{Limit: 1},
	)
	require.NoError(t, err, "first page")
	assert.Equal(t, 2, first.Total)
	assert.NotEmpty(t, first.NextCursor)
	assert.ElementsMatch(t,
		[]string{"root", "continuation"},
		sidebarIndexIDs(first.Sessions),
	)

	second, err := store.GetSidebarSessionIndex(
		ctx, db.SessionFilter{Limit: 1, Cursor: first.NextCursor},
	)
	require.NoError(t, err, "second page")
	assert.Equal(t, 2, second.Total)
	assert.Empty(t, second.NextCursor)
	assert.Equal(t, []string{"other"}, sidebarIndexIDs(second.Sessions))
}

func TestStoreGetSidebarSessionIndexPaginatesByDescendantFreshness(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	rootEnd := "2024-01-01T00:00:00Z"
	childEnd := "2024-01-10T00:00:00Z"
	otherEnd := "2024-01-05T00:00:00Z"
	rootID := "root"
	insertSidebarIndexSession(t, store, rootID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = rootEnd
	})
	insertSidebarIndexSession(t, store, "child", func(
		s *sidebarIndexSessionSeed,
	) {
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
		s.endedAt = childEnd
	})
	insertSidebarIndexSession(t, store, "other", func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = otherEnd
	})

	ctx := context.Background()
	first, err := store.GetSidebarSessionIndex(
		ctx, db.SessionFilter{Limit: 1},
	)
	require.NoError(t, err, "first page")
	assert.Equal(t, 2, first.Total)
	assert.NotEmpty(t, first.NextCursor)
	assert.ElementsMatch(t,
		[]string{"root", "child"},
		sidebarIndexIDs(first.Sessions),
	)

	second, err := store.GetSidebarSessionIndex(
		ctx, db.SessionFilter{Limit: 1, Cursor: first.NextCursor},
	)
	require.NoError(t, err, "second page")
	assert.Equal(t, 2, second.Total)
	assert.Empty(t, second.NextCursor)
	assert.Equal(t, []string{"other"}, sidebarIndexIDs(second.Sessions))
}

func TestStoreGetSidebarSessionIndexStarredIncludesStarredDescendantRoot(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	insertSidebarIndexSession(t, store, "unstarred-newer", func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = "2024-01-20T00:00:00Z"
	})
	rootID := "root"
	insertSidebarIndexSession(t, store, rootID, func(
		s *sidebarIndexSessionSeed,
	) {
		s.endedAt = "2024-01-01T00:00:00Z"
	})
	insertSidebarIndexSession(t, store, "starred-child", func(
		s *sidebarIndexSessionSeed,
	) {
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
		s.endedAt = "2024-01-10T00:00:00Z"
	})
	ok, err := store.StarSession("starred-child")
	require.NoError(t, err, "StarSession")
	require.True(t, ok, "starred-child should exist")

	index, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{
			Starred: true,
			Limit:   1,
		},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")
	assert.Empty(t, index.NextCursor)
	assert.Equal(t, 1, index.Total)
	assert.ElementsMatch(t,
		[]string{"root", "starred-child"},
		sidebarIndexIDs(index.Sessions),
	)
}

func TestStoreGetSidebarSessionIndexPaginatesOrphanRoots(t *testing.T) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	insertSidebarIndexSession(t, store, "root", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-20T00:00:00Z"
		s.userMessageCount = 2
	})
	insertSidebarIndexSession(t, store, "orphan-sub", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-19T00:00:00Z"
		s.parentSessionID = strPtr("missing-parent")
		s.relationshipType = "subagent"
	})
	insertSidebarIndexSession(t, store, "orphan-fork", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-18T00:00:00Z"
		s.parentSessionID = strPtr("orphan-sub")
		s.relationshipType = "fork"
	})
	insertSidebarIndexSession(t, store, "continuation-orphan", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-17T00:00:00Z"
		s.parentSessionID = strPtr("missing-continuation-parent")
		s.relationshipType = "continuation"
	})

	first, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{Limit: 2},
	)
	require.NoError(t, err, "first page")
	assert.Equal(t, 3, first.Total)
	assert.NotEmpty(t, first.NextCursor)
	assert.ElementsMatch(t,
		[]string{"root", "orphan-sub", "orphan-fork"},
		sidebarIndexIDs(first.Sessions),
	)

	second, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{Limit: 2, Cursor: first.NextCursor},
	)
	require.NoError(t, err, "second page")
	assert.Equal(t, 3, second.Total)
	assert.Empty(t, second.NextCursor)
	assert.Equal(t, []string{"continuation-orphan"}, sidebarIndexIDs(second.Sessions))
}

func TestStoreGetSidebarSessionIndexDoesNotPromoteSoftDeletedParentChildren(t *testing.T) {
	pgURL := testPGURL(t)
	store := ensureSidebarIndexStoreSchema(t, pgURL)
	defer store.Close()

	rootID := "soft-deleted-root"
	insertSidebarIndexSession(t, store, rootID, func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-20T00:00:00Z"
		s.deletedAt = strPtr("2024-01-21T00:00:00Z")
	})
	insertSidebarIndexSession(t, store, "child-of-deleted-parent", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-19T00:00:00Z"
		s.parentSessionID = &rootID
		s.relationshipType = "subagent"
	})
	insertSidebarIndexSession(t, store, "other", func(s *sidebarIndexSessionSeed) {
		s.endedAt = "2024-01-18T00:00:00Z"
	})

	index, err := store.GetSidebarSessionIndex(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err, "GetSidebarSessionIndex")
	assert.ElementsMatch(t,
		[]string{"other"},
		sidebarIndexIDs(index.Sessions),
	)
}

func TestStoreGetSession(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetSession(ctx, "store-test-001")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess, "expected session, got nil")
	assert.Equal(t, "test-project", sess.Project)
}

func TestStoreGetMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	msgs, err := store.GetMessages(
		ctx, "store-test-001", 0, 100, true,
	)
	require.NoError(t, err, "GetMessages")
	assert.Len(t, msgs, 2)
}

func TestStoreGetStats(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err, "GetStats")
	assert.NotZero(t, stats.SessionCount, "expected at least 1 session in stats")
	t.Logf("stats: %+v", stats)
}

func TestStoreSearch(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 5,
	})
	require.NoError(t, err, "Search")
	assert.NotEmpty(t, page.Results, "expected at least 1 search result")
	t.Logf("search results: %d", len(page.Results))
}

func TestStoreAnalyticsSummary(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	summary, err := store.GetAnalyticsSummary(
		ctx, db.AnalyticsFilter{
			From: "2026-01-01",
			To:   "2026-12-31",
		},
	)
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.NotZero(t, summary.TotalSessions, "expected at least 1 session in summary")
	t.Logf("summary: %+v", summary)
}

func seedActivitySession(
	t *testing.T, store *Store, sid string, msgs []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	},
) {
	t.Helper()
	pg := store.DB()

	// PG doesn't allow multi-statement prepared queries,
	// so run each statement separately.
	_, err := pg.Exec(
		`DELETE FROM messages WHERE session_id = $1`, sid,
	)
	require.NoError(t, err, "deleting messages")
	_, err = pg.Exec(
		`DELETE FROM sessions WHERE id = $1`, sid,
	)
	require.NoError(t, err, "deleting session")
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			($1, 'test-machine', 'test-project',
			 'claude', 'activity test',
			 '2026-03-26T10:00:00Z'::timestamptz,
			 '2026-03-26T11:00:00Z'::timestamptz,
			 $2, 0)
	`, sid, len(msgs))
	require.NoError(t, err, "inserting session")

	for _, m := range msgs {
		var tsVal interface{} = nil
		if m.ts != "" {
			tsVal = m.ts
		}
		_, err := pg.Exec(`
			INSERT INTO messages
				(session_id, ordinal, role, content,
				 timestamp, content_length, is_system)
			VALUES ($1, $2, $3, $4,
				$5::timestamptz, $6, $7)
		`, sid, m.ordinal, m.role, m.content,
			tsVal, len(m.content), m.system)
		require.NoError(t, err, "inserting message ord=%d", m.ordinal)
	}
}

func TestStoreGetSessionActivity(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-activity"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hello", "2026-03-26T10:00:00Z", false},
		{1, "assistant", "hi", "2026-03-26T10:00:30Z", false},
		{2, "user", "next", "2026-03-26T10:01:30Z", false},
		{3, "assistant", "resp", "2026-03-26T10:02:00Z", false},
		{4, "user", "back", "2026-03-26T10:28:00Z", false},
		{5, "assistant", "wb", "2026-03-26T10:29:00Z", false},
		// System message — excluded from buckets.
		{6, "user", "This session is being continued from a previous conversation.", "2026-03-26T10:29:30Z", true},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	require.NoError(t, err, "GetSessionActivity")

	assert.Equal(t, int64(60), resp.IntervalSeconds)
	assert.Equal(t, 7, resp.TotalMessages)
	assert.GreaterOrEqual(t, len(resp.Buckets), 28, "bucket count")

	first := resp.Buckets[0]
	assert.Equal(t, 1, first.UserCount)
	assert.Equal(t, 1, first.AssistantCount)
	require.NotNil(t, first.FirstOrdinal)
	assert.Equal(t, 0, *first.FirstOrdinal)

	mid := resp.Buckets[15]
	assert.Equal(t, 0, mid.UserCount)
	assert.Equal(t, 0, mid.AssistantCount)
	assert.Nil(t, mid.FirstOrdinal)
}

func TestStoreGetSessionActivity_NoMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-activity-empty"
	seedActivitySession(t, store, sid, nil)

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	require.NoError(t, err, "GetSessionActivity")
	assert.Empty(t, resp.Buckets)
}

func TestStoreGetSessionActivity_NullTimestamps(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-activity-nullts"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hi", "", false},
		{1, "assistant", "hello", "", false},
	})

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	require.NoError(t, err, "GetSessionActivity")
	assert.Empty(t, resp.Buckets)
	assert.Equal(t, 2, resp.TotalMessages)
}

func TestStoreGetSessionActivity_SingleMessage(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-activity-single"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hi", "2026-03-26T10:00:00Z", false},
	})

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	require.NoError(t, err, "GetSessionActivity")
	require.Len(t, resp.Buckets, 1)
	assert.Equal(t, 1, resp.Buckets[0].UserCount)
}

func TestStoreGetSessionActivity_PrefixInjectedExcluded(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-activity-prefix"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hello", "2026-03-26T10:00:00Z", false},
		{1, "assistant", "hi", "2026-03-26T10:00:30Z", false},
		// Prefix-detected injected message: is_system=false but
		// content starts with a system prefix.
		{2, "user", "This session is being continued from a previous conversation.", "2026-03-26T10:01:00Z", false},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	require.NoError(t, err, "GetSessionActivity")

	// The prefix-detected message should be excluded from
	// buckets but still count toward TotalMessages.
	assert.Equal(t, 3, resp.TotalMessages)

	// Only ordinals 0 and 1 should appear in buckets.
	totalBucketed := 0
	for _, b := range resp.Buckets {
		totalBucketed += b.UserCount + b.AssistantCount
	}
	assert.Equal(t, 2, totalBucketed)

	// The excluded message at 10:01:00 must not extend the
	// timestamp range. With only 10:00:00-10:00:30 visible,
	// a single bucket should cover the entire span.
	assert.Len(t, resp.Buckets, 1)
}

func TestStoreGetSessionActivity_FractionalTimestamps(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	sid := "store-test-frac-ts"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "a", "2026-03-26T10:00:00.900Z", false},
		{1, "assistant", "b", "2026-03-26T10:00:59.100Z", false},
		{2, "user", "c", "2026-03-26T10:01:01.000Z", false},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	require.NoError(t, err, "GetSessionActivity")

	require.Equal(t, int64(60), resp.IntervalSeconds)
	require.GreaterOrEqual(t, len(resp.Buckets), 2)

	// First bucket should have both sub-second messages.
	first := resp.Buckets[0]
	assert.Equal(t, 1, first.UserCount)
	assert.Equal(t, 1, first.AssistantCount)

	// Second bucket should have the third message.
	second := resp.Buckets[1]
	assert.Equal(t, 1, second.UserCount)
}

func TestStoreAnalyticsSummaryOutputTokenCoverage(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	summary, err := store.GetAnalyticsSummary(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
	)
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1800, summary.TotalOutputTokens)
	assert.Equal(t, 3, summary.TokenReportingSessions)
}

func TestStoreAnalyticsHeatmapOutputTokens(t *testing.T) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	heatmap, err := store.GetAnalyticsHeatmap(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
		"output_tokens",
	)
	require.NoError(t, err, "GetAnalyticsHeatmap")

	assert.Equal(t, "output_tokens", heatmap.Metric)
	require.Len(t, heatmap.Entries, 2)
	assert.Equal(t, "2026-03-12", heatmap.Entries[0].Date)
	assert.Equal(t, 1500, heatmap.Entries[0].Value)
	assert.Equal(t, "2026-03-13", heatmap.Entries[1].Date)
	assert.Equal(t, 300, heatmap.Entries[1].Value)
}

func TestStoreAnalyticsTopSessionsOutputTokens(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
		"output_tokens",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")

	assert.Equal(t, "output_tokens", top.Metric)
	require.Len(t, top.Sessions, 3)
	assert.Equal(t, "pg-token-001", top.Sessions[0].ID)
	assert.Equal(t, 900, top.Sessions[0].OutputTokens)
	for _, session := range top.Sessions {
		assert.NotEqual(t, "pg-token-missing", session.ID,
			"session without token coverage was included: %+v", session)
	}
}

func TestStoreAnalyticsTopSessionsDisplayName(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			display_name, session_name,
			started_at, ended_at, message_count,
			user_message_count
		) VALUES
			('pg-session-name', 'test-machine', 'test-project',
			 'claude', 'raw first user message', NULL,
			 'Agent generated title',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz,
			 10, 2),
			('pg-custom-name', 'test-machine', 'test-project',
			 'claude', 'raw first user message',
			 'User renamed title', 'Generated title hidden by rename',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:30:00Z'::timestamptz,
			 9, 2)
	`)
	require.NoError(t, err, "inserting top session names")

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-12",
		},
		"messages",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")

	byID := map[string]db.TopSession{}
	for _, session := range top.Sessions {
		byID[session.ID] = session
	}

	named, ok := byID["pg-session-name"]
	require.True(t, ok, "pg-session-name missing from top sessions")
	require.NotNil(t, named.DisplayName,
		"session_name should be exposed as display_name")
	assert.Equal(t, "Agent generated title", *named.DisplayName)

	custom, ok := byID["pg-custom-name"]
	require.True(t, ok, "pg-custom-name missing from top sessions")
	require.NotNil(t, custom.DisplayName,
		"custom display_name should be exposed")
	assert.Equal(t, "User renamed title", *custom.DisplayName)
}

func TestStoreAnalyticsTopSessionsMessagesAllowRunningSessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count
		) VALUES
			('pg-running-session', 'test-machine', 'test-project',
			 'claude', 'still running',
			 '2026-03-12T13:00:00Z'::timestamptz,
			 NULL,
			 12, 3),
			('pg-finished-session', 'test-machine', 'test-project',
			 'claude', 'finished',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz,
			 10, 2)
	`)
	require.NoError(t, err, "inserting top sessions")

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-12",
		},
		"messages",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")

	byID := map[string]db.TopSession{}
	for _, session := range top.Sessions {
		byID[session.ID] = session
	}

	running, ok := byID["pg-running-session"]
	require.True(t, ok, "running session missing from top sessions")
	assert.Equal(t, 0.0, running.DurationMin)
}

func TestStoreAnalyticsTopSessionsDurationUsesClampedActiveDuration(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	// Unique date so only these two sessions match the filter,
	// regardless of what other tests leave behind in the schema.
	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count, user_message_count
		) VALUES
			('pg-clamp-wall', 'test-machine', 'clamp-parity',
			 'claude', 'wall start',
			 '2027-07-15T09:00:00Z'::timestamptz,
			 '2027-07-15T11:00:00Z'::timestamptz,
			 3, 2),
			('pg-clamp-active', 'test-machine', 'clamp-parity',
			 'claude', 'active start',
			 '2027-07-15T09:30:00Z'::timestamptz,
			 '2027-07-15T09:50:00Z'::timestamptz,
			 3, 2)
	`)
	require.NoError(t, err, "inserting sessions")

	_, err = store.DB().Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp) VALUES
			('pg-clamp-wall', 0, 'user', 'noop',
			 '2027-07-15T09:00:00Z'::timestamptz),
			('pg-clamp-wall', 1, 'assistant', 'idle wait',
			 '2027-07-15T10:59:00Z'::timestamptz),
			('pg-clamp-wall', 2, 'user', 'done',
			 '2027-07-15T11:00:00Z'::timestamptz),
			('pg-clamp-active', 0, 'user', 'start',
			 '2027-07-15T09:30:00Z'::timestamptz),
			('pg-clamp-active', 1, 'assistant', 'tooling',
			 '2027-07-15T09:35:00Z'::timestamptz),
			('pg-clamp-active', 2, 'user', 'finish',
			 '2027-07-15T09:50:00Z'::timestamptz)
	`)
	require.NoError(t, err, "inserting messages")

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{From: "2027-07-15", To: "2027-07-15"},
		"duration",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, top.Sessions, 2)

	// Active duration ranks ahead of wall: the engaged 20-min session
	// (5 min gap + a 15 min gap capped at the 5 min idle cap = 10)
	// beats the mostly-idle 2-hour session (119 min capped to 5 + a
	// 1 min gap = 6). Generation gaps count even with no tool calls.
	assert.Equal(t, "pg-clamp-active", top.Sessions[0].ID)
	assert.Equal(t, 20.0, top.Sessions[0].DurationMin)
	assert.Equal(t, 10.0, top.Sessions[0].ActiveDurationMin)
	assert.Equal(t, "pg-clamp-wall", top.Sessions[1].ID)
	assert.Equal(t, 120.0, top.Sessions[1].DurationMin)
	assert.Equal(t, 6.0, top.Sessions[1].ActiveDurationMin)
}

func TestStoreAnalyticsTopSessionsDurationExcludesReversedTimestamps(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	// A reversed session (ended_at < started_at) still accumulates
	// positive message-gap active duration, so without an eligibility
	// guard it would rank into the duration list ordered by active
	// duration. PostgreSQL must reject it, matching SQLite and DuckDB.
	// (Empty-string timestamps are not representable in timestamptz
	// columns, so only the reversed case applies on this backend.)
	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count, user_message_count
		) VALUES
			('pg-elig-valid', 'test-machine', 'elig-parity',
			 'claude', 'valid start',
			 '2027-08-20T09:00:00Z'::timestamptz,
			 '2027-08-20T09:30:00Z'::timestamptz,
			 2, 1),
			('pg-elig-reversed', 'test-machine', 'elig-parity',
			 'claude', 'reversed start',
			 '2027-08-20T10:00:00Z'::timestamptz,
			 '2027-08-20T09:00:00Z'::timestamptz,
			 2, 1)
	`)
	require.NoError(t, err, "inserting sessions")

	_, err = store.DB().Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp) VALUES
			('pg-elig-valid', 0, 'user', 'start',
			 '2027-08-20T09:00:00Z'::timestamptz),
			('pg-elig-valid', 1, 'assistant', 'work',
			 '2027-08-20T09:03:00Z'::timestamptz),
			('pg-elig-reversed', 0, 'user', 'start',
			 '2027-08-20T09:00:00Z'::timestamptz),
			('pg-elig-reversed', 1, 'assistant', 'work',
			 '2027-08-20T09:04:00Z'::timestamptz)
	`)
	require.NoError(t, err, "inserting messages")

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{From: "2027-08-20", To: "2027-08-20"},
		"duration",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")

	ids := []string{}
	for _, session := range top.Sessions {
		ids = append(ids, session.ID)
	}
	assert.Equal(t, []string{"pg-elig-valid"}, ids,
		"reversed duration row must be excluded")
}

func TestStoreWriteSurfaceSplitByCapability(t *testing.T) {
	pgURL := testPGURL(t)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	assert.True(t, store.ReadOnly())

	ctx := context.Background()
	project := "store-capability"
	sessionID := "store-capability-001"
	trashedID := "store-capability-002"
	emptyTrashID := "store-capability-003"
	batchTrashID := "store-capability-004"

	insightID, err := store.InsertInsight(db.Insight{
		Type:     "dashboard",
		DateFrom: "2026-03-12",
		DateTo:   "2026-03-12",
		Project:  &project,
		Agent:    "claude",
		Content:  "insight content",
		CacheKey: "capability-cache",
	})
	require.NoError(t, err, "InsertInsight")
	require.NotZero(t, insightID)

	insight, err := store.GetInsight(ctx, insightID)
	require.NoError(t, err, "GetInsight")
	require.NotNil(t, insight)
	assert.Equal(t, project, *insight.Project)

	cached, err := store.GetCachedInsight(ctx, "capability-cache")
	require.NoError(t, err, "GetCachedInsight")
	require.NotNil(t, cached)
	assert.Equal(t, insightID, cached.ID)

	listed, err := store.ListInsights(ctx, db.InsightFilter{
		Type: "dashboard",
	})
	require.NoError(t, err, "ListInsights")
	require.NotEmpty(t, listed)
	assert.Equal(t, insightID, listed[0].ID)

	require.NoError(t, store.DeleteInsight(insightID), "DeleteInsight")
	insight, err = store.GetInsight(ctx, insightID)
	require.NoError(t, err, "GetInsight after delete")
	assert.Nil(t, insight)

	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			display_name, started_at, ended_at, message_count,
			user_message_count
		) VALUES
			($1, 'machine', $2, 'claude', 'hello',
			 NULL, '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz, 2, 1),
			($3, 'machine', $2, 'claude', 'trash me',
			 NULL, '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz, 2, 1),
			($4, 'machine', $2, 'claude', 'empty trash me',
			 NULL, '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:30:00Z'::timestamptz, 2, 1),
			($5, 'machine', $2, 'claude', 'batch trash me',
			 NULL, '2026-03-12T13:00:00Z'::timestamptz,
			 '2026-03-12T13:30:00Z'::timestamptz, 2, 1)
	`, sessionID, project, trashedID, emptyTrashID, batchTrashID)
	require.NoError(t, err, "inserting session rows")

	renamed := "Capability renamed session"
	require.NoError(t, store.RenameSession(sessionID, &renamed),
		"RenameSession")
	sess, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err, "GetSession after rename")
	require.NotNil(t, sess)
	require.NotNil(t, sess.DisplayName)
	assert.Equal(t, renamed, *sess.DisplayName)

	require.NoError(t, store.SoftDeleteSession(sessionID),
		"SoftDeleteSession")
	sess, err = store.GetSession(ctx, sessionID)
	require.NoError(t, err, "GetSession after soft delete")
	assert.Nil(t, sess)
	trashed, err := store.ListTrashedSessions(ctx)
	require.NoError(t, err, "ListTrashedSessions")
	assert.Contains(t, sessionIDs(trashed), sessionID)

	restored, err := store.RestoreSession(sessionID)
	require.NoError(t, err, "RestoreSession")
	assert.EqualValues(t, 1, restored)

	require.NoError(t, store.SoftDeleteSession(trashedID),
		"SoftDeleteSession trashedID")
	deleted, err := store.DeleteSessionIfTrashed(trashedID)
	require.NoError(t, err, "DeleteSessionIfTrashed")
	assert.EqualValues(t, 1, deleted)
	sess, err = store.GetSessionFull(ctx, trashedID)
	require.NoError(t, err, "GetSessionFull after permanent delete")
	assert.Nil(t, sess)

	deletedCount, err := store.SoftDeleteSessions([]string{
		emptyTrashID, batchTrashID,
	})
	require.NoError(t, err, "SoftDeleteSessions")
	assert.Equal(t, 2, deletedCount)
	count, err := store.EmptyTrash()
	require.NoError(t, err, "EmptyTrash")
	assert.Equal(t, 2, count)
	trashed, err = store.ListTrashedSessions(ctx)
	require.NoError(t, err, "ListTrashedSessions after empty trash")
	assert.NotContains(t, sessionIDs(trashed), emptyTrashID)
	assert.NotContains(t, sessionIDs(trashed), batchTrashID)

	assert.Equal(t, db.ErrReadOnly, store.UpsertSession(db.Session{}))
	assert.Equal(t, db.ErrReadOnly,
		store.ReplaceSessionMessages("x", nil))
	_, err = store.WriteSessionBatchAtomic(nil)
	assert.ErrorIs(t, err, db.ErrReadOnly)
	_, err = store.RecordRecallQueryEvent(ctx, db.RecallQueryEvent{
		Surface: db.RecallQuerySurfaceQuery,
	})
	assert.ErrorIs(t, err, db.ErrReadOnly)
}
