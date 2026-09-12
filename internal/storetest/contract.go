package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// CoreStore is the Task 5 slice of db.Store. Keeping the contract narrow lets
// it exercise the embedded BunStore directly before later task groups move.
type CoreStore interface {
	SetCursorSecret([]byte)
	DecodeCursor(string) (db.SessionCursor, error)
	ListSessions(context.Context, db.SessionFilter) (db.SessionPage, error)
	GetSidebarSessionIndex(context.Context, db.SessionFilter) (db.SidebarSessionIndex, error)
	GetSession(context.Context, string) (*db.Session, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	FindSessionIDsByPartial(context.Context, string, int) ([]string, error)
	Search(context.Context, db.SearchFilter) (db.SearchPage, error)
	GetChildSessions(context.Context, string) ([]db.Session, error)
	GetSessionVersion(string) (int, int64, bool)
	GetMessages(context.Context, string, int, int, bool) ([]db.Message, error)
	GetMessagesWindow(context.Context, string, db.MessageWindow) ([]db.Message, error)
	GetAllMessages(context.Context, string) ([]db.Message, error)
	GetResumeModelCounts(context.Context, string) ([]db.ModelCount, error)
	GetSessionActivity(context.Context, string) (*db.SessionActivityResponse, error)
	GetSessionTiming(context.Context, string) (*db.SessionTiming, error)
	GetStats(context.Context, bool, bool) (db.Stats, error)
	GetProjects(context.Context, bool, bool) ([]db.ProjectInfo, error)
	GetAgents(context.Context, bool, bool) ([]db.AgentInfo, error)
	GetMachines(context.Context, bool, bool) ([]string, error)
	GetBranches(context.Context, bool, bool) ([]db.BranchInfo, error)
}

// Backend registers one embedded BunStore and its backend-owned fixture setup.
type Backend struct {
	Name string
	Open func(*testing.T) CoreStore
	Seed func(*testing.T, CoreStore) Fixture
}

// RunCoreContract verifies literal Task 5 behavior against one common store.
func RunCoreContract(t *testing.T, backend Backend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store := backend.Open(t)
		fixture := backend.Seed(t, store)
		store.SetCursorSecret([]byte("shared-core-contract-secret"))

		t.Run("sessions_cursors_and_metadata", func(t *testing.T) {
			runSessionContract(t, store, fixture)
		})
		t.Run("messages_activity_and_timing", func(t *testing.T) {
			runMessageContract(t, store, fixture)
		})
	})
}

func runSessionContract(t *testing.T, store CoreStore, fixture Fixture) {
	t.Helper()
	ctx := t.Context()
	first, err := store.ListSessions(ctx, db.SessionFilter{Project: "alpha", Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Sessions, 1)
	assert.Equal(t, fixture.RootNewID, first.Sessions[0].ID)
	assertSessionFileMetadataEmpty(t, first.Sessions[0])
	assert.Equal(t, 2, first.Total)
	assert.NotEmpty(t, first.NextCursor)
	decoded, err := store.DecodeCursor(first.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, fixture.RootNewID, decoded.ID)
	assert.Equal(t, 2, decoded.Total)

	second, err := store.ListSessions(ctx, db.SessionFilter{
		Project: "alpha", Limit: 1, Cursor: first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, fixture.RootOldID, second.Sessions[0].ID)
	assert.Equal(t, 2, second.Total)
	assert.Empty(t, second.NextCursor)

	active, err := store.ListSessions(ctx, db.SessionFilter{
		Project: "termination", Termination: "active", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, active.Sessions, 1)
	assert.Equal(t, fixture.ActiveID, active.Sessions[0].ID)

	visible, err := store.GetSession(ctx, fixture.RootNewID)
	require.NoError(t, err)
	require.NotNil(t, visible)
	assert.Equal(t, "alpha", visible.Project)
	assertSessionFileMetadataEmpty(t, *visible)
	trashed, err := store.GetSession(ctx, fixture.DeletedID)
	require.NoError(t, err)
	assert.Nil(t, trashed)
	full, err := store.GetSessionFull(ctx, fixture.DeletedID)
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.NotNil(t, full.DeletedAt)
	rootFull, err := store.GetSessionFull(ctx, fixture.RootNewID)
	require.NoError(t, err)
	require.NotNil(t, rootFull)
	require.NotNil(t, rootFull.FilePath)
	assert.Equal(t, "fixtures/root-new.jsonl", *rootFull.FilePath)
	require.NotNil(t, rootFull.FileSize)
	assert.Equal(t, int64(2048), *rootFull.FileSize)
	require.NotNil(t, rootFull.FileMtime)
	assert.Equal(t, int64(42), *rootFull.FileMtime)
	require.NotNil(t, rootFull.FileInode)
	assert.Equal(t, int64(73), *rootFull.FileInode)
	require.NotNil(t, rootFull.FileDevice)
	assert.Equal(t, int64(19), *rootFull.FileDevice)
	require.NotNil(t, rootFull.FileHash)
	assert.Equal(t, "hash", *rootFull.FileHash)
	require.NotNil(t, rootFull.LocalModifiedAt)
	assert.Equal(t, "2026-08-02T10:04:00Z", *rootFull.LocalModifiedAt)

	partial, err := store.FindSessionIDsByPartial(ctx, "root", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{fixture.RootNewID, fixture.RootOldID}, partial)
	search, err := store.Search(ctx, db.SearchFilter{
		Query: "visibilityneedle", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	assert.Equal(t, fixture.RootOldID, search.Results[0].SessionID,
		"search returns one hydrated result per visible session")
	children, err := store.GetChildSessions(ctx, fixture.RootNewID)
	require.NoError(t, err)
	require.Len(t, children, 2)
	assert.Equal(t,
		[]string{fixture.ChildID, fixture.ChildNoStartID},
		[]string{children[0].ID, children[1].ID},
	)
	assertSessionFileMetadataEmpty(t, children[0])

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, 2, index.Total)
	assert.ElementsMatch(t,
		[]string{fixture.RootNewID, fixture.ChildID, fixture.ChildNoStartID, fixture.RootOldID},
		sidebarIDs(index.Sessions),
	)
	firstRoot, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
		Project: "alpha", Limit: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, firstRoot.Total)
	assert.NotEmpty(t, firstRoot.NextCursor)
	assert.ElementsMatch(t,
		[]string{fixture.RootNewID, fixture.ChildID, fixture.ChildNoStartID},
		sidebarIDs(firstRoot.Sessions),
	)
	secondRoot, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
		Project: "alpha", Limit: 1, Cursor: firstRoot.NextCursor,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, secondRoot.Total)
	assert.Empty(t, secondRoot.NextCursor)
	assert.Equal(t, []string{fixture.RootOldID}, sidebarIDs(secondRoot.Sessions))

	count, version, ok := store.GetSessionVersion(fixture.RootNewID)
	assert.True(t, ok)
	assert.Equal(t, 3, count)
	assert.NotZero(t, version)

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.SessionCount)
	assert.Equal(t, 6, stats.MessageCount)
	assert.Equal(t, 2, stats.ProjectCount)
	assert.Equal(t, 2, stats.MachineCount)
	require.NotNil(t, stats.EarliestSession)
	assert.Equal(t, "2026-08-01T09:00:00Z", *stats.EarliestSession)

	projects, err := store.GetProjects(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []db.ProjectInfo{
		{Name: "alpha", SessionCount: 2},
		{Name: "termination", SessionCount: 1},
	}, projects)
	agents, err := store.GetAgents(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []db.AgentInfo{
		{Name: "claude", SessionCount: 1},
		{Name: "codex", SessionCount: 2},
	}, agents)
	machines, err := store.GetMachines(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"host-a", "host-b"}, machines)
	branches, err := store.GetBranches(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []db.BranchInfo{
		{Project: "alpha", Branch: "main", Token: db.EncodeBranchFilterToken("alpha", "main")},
		{Project: "alpha", Branch: "release", Token: db.EncodeBranchFilterToken("alpha", "release")},
		{Project: "termination", Token: db.EncodeBranchFilterToken("termination", "")},
	}, branches)
}

func assertSessionFileMetadataEmpty(t *testing.T, session db.Session) {
	t.Helper()
	assert.Nil(t, session.FilePath)
	assert.Nil(t, session.FileSize)
	assert.Nil(t, session.FileMtime)
	assert.Nil(t, session.FileInode)
	assert.Nil(t, session.FileDevice)
	assert.Nil(t, session.FileHash)
	assert.Nil(t, session.LocalModifiedAt)
}

func runMessageContract(t *testing.T, store CoreStore, fixture Fixture) {
	t.Helper()
	ctx := t.Context()
	messages, err := store.GetMessages(ctx, fixture.RootNewID, 0, 2, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, []int{0, 1}, []int{messages[0].Ordinal, messages[1].Ordinal})
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "Read", messages[1].ToolCalls[0].Category)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 2)
	assert.Equal(t, "completed", messages[1].ToolCalls[0].ResultEvents[1].Status)

	anchor := 1
	window, err := store.GetMessagesWindow(ctx, fixture.RootNewID, db.MessageWindow{
		Around: &anchor, Before: 1, After: 1, Roles: []string{"user"},
	})
	require.NoError(t, err)
	require.Len(t, window, 2)
	assert.Equal(t, []int{0, 1}, []int{window[0].Ordinal, window[1].Ordinal})

	zeroBefore, err := store.GetMessagesWindow(ctx, fixture.RootNewID, db.MessageWindow{
		Around: &anchor, Before: 0, After: 1,
	})
	require.NoError(t, err)
	require.Len(t, zeroBefore, 2)
	assert.Equal(t, []int{1, 2}, []int{zeroBefore[0].Ordinal, zeroBefore[1].Ordinal})

	zeroAfter, err := store.GetMessagesWindow(ctx, fixture.RootNewID, db.MessageWindow{
		Around: &anchor, Before: 1, After: 0,
	})
	require.NoError(t, err)
	require.Len(t, zeroAfter, 2)
	assert.Equal(t, []int{0, 1}, []int{zeroAfter[0].Ordinal, zeroAfter[1].Ordinal})

	all, err := store.GetAllMessages(ctx, fixture.RootNewID)
	require.NoError(t, err)
	require.Len(t, all, 3)
	counts, err := store.GetResumeModelCounts(ctx, fixture.RootNewID)
	require.NoError(t, err)
	assert.Equal(t, []db.ModelCount{{Model: "model-a", Count: 2}}, counts)

	activity, err := store.GetSessionActivity(ctx, fixture.RootNewID)
	require.NoError(t, err)
	assert.Equal(t, 3, activity.TotalMessages)
	assert.Equal(t, int64(60), activity.IntervalSeconds)
	require.Len(t, activity.Buckets, 3)
	assert.Equal(t, 1, activity.Buckets[0].UserCount)
	assert.Equal(t, 1, activity.Buckets[1].AssistantCount)

	timing, err := store.GetSessionTiming(ctx, fixture.RootNewID)
	require.NoError(t, err)
	require.NotNil(t, timing)
	assert.Equal(t, int64(180000), timing.TotalDurationMs)
	assert.Equal(t, int64(20000), timing.ToolDurationMs)
	assert.Equal(t, 1, timing.TurnCount)
	assert.Equal(t, 1, timing.ToolCallCount)
	require.NotNil(t, timing.SlowestCall)
	require.NotNil(t, timing.SlowestCall.DurationMs)
	assert.Equal(t, int64(15000), *timing.SlowestCall.DurationMs)
}

func sidebarIDs(rows []db.SidebarSessionIndexRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
