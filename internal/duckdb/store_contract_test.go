//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"errors"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuckDBStoreContract(t *testing.T) {
	store, fixture := newSyncedStore(t)
	tests := []struct {
		name string
		run  func(t *testing.T, store *Store, fixture syncFixture)
	}{
		{"sessions_cursors_and_metadata", duckContractSessionsCursorsAndMetadata},
		{"messages_search_and_secrets", duckContractMessagesSearchAndSecrets},
		{"read_only_curation", duckContractReadOnlyCuration},
		{"analytics_trends_and_usage", duckContractAnalyticsTrendsAndUsage},
		{"local_only_methods_read_only", duckContractLocalOnlyMethodsReadOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, store, fixture)
		})
	}
}

// TestDuckDBStoreHasSemanticFalse pins that the DuckDB store reports no
// semantic search capability until it gets its own VectorSearcher seam.
func TestDuckDBStoreHasSemanticFalse(t *testing.T) {
	s := &Store{}
	assert.False(t, s.HasSemantic(), "DuckDB HasSemantic")
}

// TestDuckDBSearchContentSemanticModesUnavailable pins that "semantic" and
// "hybrid" are rejected with db.ErrSemanticUnavailable before any query runs
// -- a zero-value Store (no live *sql.DB) is enough to prove that.
func TestDuckDBSearchContentSemanticModesUnavailable(t *testing.T) {
	s := &Store{}
	for _, mode := range []string{"semantic", "hybrid"} {
		_, err := s.SearchContent(context.Background(),
			db.ContentSearchFilter{Pattern: "x", Mode: mode})
		require.Error(t, err, "mode %q", mode)
		assert.True(t, errors.Is(err, db.ErrSemanticUnavailable),
			"mode %q: want ErrSemanticUnavailable, got %v", mode, err)
	}
}

func TestDuckDBFindSessionIDsByPartialLiteralCaseSensitive(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	for _, id := range []string{"abc_def", "abcXdef", "abc%def", "ABCdef"} {
		require.NoError(t, local.UpsertSession(db.Session{
			ID: id, Project: "proj", Machine: "test",
			Agent: "claude", MessageCount: 1,
		}), "upsert %q", id)
	}
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.FindSessionIDsByPartial(ctx, "c_d", 10)
	require.NoError(t, err, "underscore lookup")
	assert.Equal(t, []string{"abc_def"}, got)

	got, err = store.FindSessionIDsByPartial(ctx, "c%d", 10)
	require.NoError(t, err, "percent lookup")
	assert.Equal(t, []string{"abc%def"}, got)

	got, err = store.FindSessionIDsByPartial(ctx, "abc", 10)
	require.NoError(t, err, "case-sensitive lookup")
	assert.ElementsMatch(t, []string{"abc_def", "abcXdef", "abc%def"}, got)
	assert.NotContains(t, got, "ABCdef")
}

func duckContractSessionsCursorsAndMetadata(
	t *testing.T, store *Store, fixture syncFixture,
) {
	t.Helper()
	ctx := context.Background()

	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, fixture.betaID, page.Sessions[0].ID)
	require.NotEmpty(t, page.NextCursor)

	cur, err := store.DecodeCursor(page.NextCursor)
	require.NoError(t, err)
	require.Equal(t, fixture.betaID, cur.ID)
	require.Equal(t, 2, cur.Total)

	next, err := store.ListSessions(ctx, db.SessionFilter{
		Limit:  1,
		Cursor: page.NextCursor,
	})
	require.NoError(t, err)
	require.Equal(t, 2, next.Total)
	require.Equal(t, []string{fixture.alphaID}, duckSessionIDs(next.Sessions))

	// Sorting by a non-default column (messages, ascending by default) and its
	// keyset cursor must render on DuckDB: beta has 1 message, alpha has 2.
	byMsgs, err := store.ListSessions(ctx, db.SessionFilter{OrderBy: "messages", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.betaID, fixture.alphaID}, duckSessionIDs(byMsgs.Sessions))

	msgPage1, err := store.ListSessions(ctx, db.SessionFilter{OrderBy: "messages", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.betaID}, duckSessionIDs(msgPage1.Sessions))
	require.NotEmpty(t, msgPage1.NextCursor)
	msgPage2, err := store.ListSessions(ctx, db.SessionFilter{
		OrderBy: "messages", Limit: 1, Cursor: msgPage1.NextCursor,
	})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.alphaID}, duckSessionIDs(msgPage2.Sessions))

	alpha, err := store.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, alpha)
	require.Equal(t, "alpha", alpha.Project)

	full, err := store.GetSessionFull(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.Equal(t, fixture.alphaID, full.ID)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
	require.NoError(t, err)
	require.Contains(t, duckSidebarSessionIDs(index.Sessions), fixture.alphaID)

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, 2, stats.SessionCount)
	require.Equal(t, 3, stats.MessageCount)

	projects, err := store.GetProjects(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, duckProjectNames(projects))

	agents, err := store.GetAgents(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"claude"}, duckAgentNames(agents))

	machines, err := store.GetMachines(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"test-machine"}, machines)
}

func duckContractMessagesSearchAndSecrets(
	t *testing.T, store *Store, fixture syncFixture,
) {
	t.Helper()
	ctx := context.Background()

	msgs, err := store.GetMessages(ctx, fixture.alphaID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	require.Equal(t, "duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)

	all, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, duckMessageOrdinals(all))

	activity, err := store.GetSessionActivity(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, activity)
	require.Equal(t, 2, activity.TotalMessages)

	timing, err := store.GetSessionTiming(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, timing)

	search, err := store.Search(ctx, db.SearchFilter{Query: "secret token", Limit: 5})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, fixture.alphaID, search.Results[0].SessionID)

	content, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck result",
		Sources:        []string{"tool_result"},
		IncludeOneShot: true,
		Limit:          5,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"tool_result"}, duckContentLocations(content.Matches))

	regex, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `secret\s+token`,
		Mode:           "regex",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          5,
	})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.alphaID}, duckContentSessionIDs(regex.Matches))

	findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Project: "alpha",
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, findings.Findings, 1)
	source, ok, err := store.SecretFindingSource(ctx, findings.Findings[0].SecretFinding)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, source, "secret token")
}

func duckContractReadOnlyCuration(
	t *testing.T, store *Store, fixture syncFixture,
) {
	t.Helper()
	ctx := context.Background()

	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{fixture.alphaID}, stars)

	ok, err := store.StarSession(fixture.betaID)
	require.ErrorIs(t, err, db.ErrReadOnly)
	require.False(t, ok)
	require.ErrorIs(t, store.UnstarSession(fixture.alphaID), db.ErrReadOnly)
	require.ErrorIs(t, store.BulkStarSessions([]string{fixture.betaID}), db.ErrReadOnly)

	pinID, err := store.PinMessage(fixture.alphaID, 1, nil)
	require.ErrorIs(t, err, db.ErrReadOnly)
	require.Zero(t, pinID)
	require.ErrorIs(t, store.UnpinMessage(fixture.alphaID, 1), db.ErrReadOnly)

	pins, err := store.ListPinnedMessages(ctx, fixture.alphaID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.Equal(t, "pin alpha", *pins[0].Note)
}

func duckContractAnalyticsTrendsAndUsage(
	t *testing.T, store *Store, fixture syncFixture,
) {
	t.Helper()
	ctx := context.Background()
	filter := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}

	summary, err := store.GetAnalyticsSummary(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 2, summary.TotalSessions)
	require.Equal(t, 3, summary.TotalMessages)

	activity, err := store.GetAnalyticsActivity(ctx, filter, "day")
	require.NoError(t, err)
	require.NotEmpty(t, activity.Series)

	tools, err := store.GetAnalyticsTools(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 1, tools.TotalCalls)

	skills, err := store.GetAnalyticsSkills(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 1, skills.TotalSkillCalls)
	require.Equal(t, 1, skills.DistinctSkills)
	require.NotEmpty(t, skills.BySkill)
	require.Equal(t, "duck-search", skills.BySkill[0].SkillName)

	trendTerms, err := db.ParseTrendTerms([]string{"alpha"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(ctx, filter, trendTerms, "week")
	require.NoError(t, err)
	require.Equal(t, 1, trends.Series[0].Total)

	usageFilter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}
	daily, err := store.GetDailyUsage(ctx, usageFilter)
	require.NoError(t, err)
	require.Equal(t, 13, daily.Totals.InputTokens)
	require.Equal(t, 11, daily.Totals.OutputTokens)
	require.Equal(t, 2, daily.SessionCounts.Total)
	require.Equal(t, 1, daily.SessionCounts.ByProject["alpha"])
	require.Equal(t, 1, daily.SessionCounts.ByProject["beta"])

	top, err := store.GetTopSessionsByCost(ctx, usageFilter, 10)
	require.NoError(t, err)
	require.NotEmpty(t, top)
	require.Equal(t, fixture.alphaID, top[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, usageFilter)
	require.NoError(t, err)
	require.Equal(t, 2, counts.Total)

	sessionUsage, err := store.GetSessionUsage(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, sessionUsage)
	require.True(t, sessionUsage.HasCost)
	require.Equal(t, []string{"claude-test"}, sessionUsage.Models)
}

func duckContractLocalOnlyMethodsReadOnly(
	t *testing.T, store *Store, fixture syncFixture,
) {
	t.Helper()
	require.True(t, store.ReadOnly())
	name := "ignored"
	requireReadOnlyDuck(t, store.RenameSession(fixture.alphaID, &name))
	requireReadOnlyDuck(t, store.SoftDeleteSession(fixture.alphaID))
	_, err := store.RestoreSession(fixture.alphaID)
	requireReadOnlyDuck(t, err)
	_, err = store.DeleteSessionIfTrashed(fixture.alphaID)
	requireReadOnlyDuck(t, err)
	_, err = store.EmptyTrash()
	requireReadOnlyDuck(t, err)
	_, err = store.InsertInsight(db.Insight{})
	requireReadOnlyDuck(t, err)
	requireReadOnlyDuck(t, store.DeleteInsight(1))
}

func requireReadOnlyDuck(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly), "expected ErrReadOnly, got %v", err)
}

func TestDuckDBGetUsageMatchingSessionCountCountsCopilotSessionsWithoutUsageRows(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	ts := "2024-06-15T10:00:00Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: db.Session{
			ID:               "duck-copilot-empty",
			Project:          "alpha",
			Machine:          "test-machine",
			Agent:            "copilot",
			StartedAt:        &ts,
			EndedAt:          &ts,
			MessageCount:     1,
			UserMessageCount: 1,
		},
		Messages: []db.Message{{
			SessionID:  "duck-copilot-empty",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  ts,
			Model:      "gpt-5.3-codex",
			TokenUsage: nil,
		}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "seed copilot session")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2024-06-15",
		To:       "2024-06-15",
		Timezone: "UTC",
		Agent:    "copilot",
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestDuckDBGetUsageMatchingSessionCountCountsCopilotSessionByMessageTimestampOutsideSessionWindow(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	activityTS := "2026-02-08T10:00:00Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: db.Session{
				ID:               "duck-copilot-late-message",
				Project:          "alpha",
				Machine:          "test-machine",
				Agent:            "copilot",
				StartedAt:        &activityTS,
				EndedAt:          &activityTS,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []db.Message{{
				SessionID:  "duck-copilot-late-message",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2026-02-10T12:00:00Z",
				Model:      "gpt-5.3-codex",
				TokenUsage: nil,
			}},
			ReplaceMessages: true,
		},
		{
			Session: db.Session{
				ID:               "duck-copilot-out-of-range",
				Project:          "alpha",
				Machine:          "test-machine",
				Agent:            "copilot",
				StartedAt:        &activityTS,
				EndedAt:          &activityTS,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []db.Message{{
				SessionID:  "duck-copilot-out-of-range",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  activityTS,
				Model:      "gpt-5.3-codex",
				TokenUsage: nil,
			}},
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err, "seed copilot sessions")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From:     "2026-02-10",
		To:       "2026-02-10",
		Timezone: "UTC",
		Agent:    "copilot",
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestDuckDBGetUsageMatchingSessionCountModelFilterAppliesToBoundedRow
// guards against the model/exclude-model predicate matching session-wide
// instead of on the in-range message row: a session with an out-of-range
// message on the filtered model but an in-range message on a different
// model must not match a Model filter for the out-of-range model.
func TestDuckDBGetUsageMatchingSessionCountModelFilterAppliesToBoundedRow(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	startedAt := "2026-02-08T10:00:00Z"
	endedAt := "2026-02-10T12:00:00Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: db.Session{
			ID:               "duck-copilot-mixed-model",
			Project:          "alpha",
			Machine:          "test-machine",
			Agent:            "copilot",
			StartedAt:        &startedAt,
			EndedAt:          &endedAt,
			MessageCount:     2,
			UserMessageCount: 1,
		},
		Messages: []db.Message{
			{
				SessionID:  "duck-copilot-mixed-model",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2026-02-08T10:00:00Z",
				Model:      "gpt-5.3-codex",
				TokenUsage: nil,
			},
			{
				SessionID:  "duck-copilot-mixed-model",
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "2026-02-10T12:00:00Z",
				Model:      "claude-sonnet",
				TokenUsage: nil,
			},
		},
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "seed mixed-model copilot session")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
		Model: "gpt-5.3-codex",
	})
	require.NoError(t, err)
	require.Equal(t, 0, count,
		"out-of-range message's model must not match the bounded window")
}

// TestDuckDBGetUsageMatchingSessionCountCountsAssistantMessageWithNoModel
// guards against gating matching-session eligibility on m.model != ”:
// some Copilot assistant messages parse before a model name is known, so
// an assistant message with an empty Model must still count toward the
// matching-session total when no Model/ExcludeModel filter narrows it.
func TestDuckDBGetUsageMatchingSessionCountCountsAssistantMessageWithNoModel(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	ts := "2026-02-10T10:00:00Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: db.Session{
			ID:               "duck-copilot-no-model",
			Project:          "alpha",
			Machine:          "test-machine",
			Agent:            "copilot",
			StartedAt:        &ts,
			EndedAt:          &ts,
			MessageCount:     1,
			UserMessageCount: 1,
		},
		Messages: []db.Message{{
			SessionID:  "duck-copilot-no-model",
			Ordinal:    0,
			Role:       "assistant",
			Timestamp:  ts,
			Model:      "",
			TokenUsage: nil,
		}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "seed no-model copilot session")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	count, err := store.GetUsageMatchingSessionCount(ctx, db.UsageFilter{
		From: "2026-02-10", To: "2026-02-10",
		Timezone: "UTC", Agent: "copilot",
	})
	require.NoError(t, err)
	require.Equal(t, 1, count,
		"assistant message with no model must still count without a model filter")
}

// TestDuckDBGetUsageMatchingSessionCountUnboundedMatchesBoundedSemantics
// guards against the unbounded (no From/To) branch drifting from the
// bounded branch: soft-deleted sessions must be excluded (backend parity
// with SQLite/PG, which seed their unbounded WHERE with the deleted_at
// eligibility), sessions without assistant/event activity must not count,
// and empty-model assistant messages must survive an ExcludeModel filter.
func TestDuckDBGetUsageMatchingSessionCountUnboundedMatchesBoundedSemantics(
	t *testing.T,
) {
	ctx := context.Background()
	local := newLocalDB(t)
	ts := "2026-03-01T10:00:00Z"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: db.Session{
				ID:               "duck-copilot-live",
				Project:          "alpha",
				Machine:          "test-machine",
				Agent:            "copilot",
				StartedAt:        &ts,
				EndedAt:          &ts,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []db.Message{{
				SessionID:  "duck-copilot-live",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  ts,
				Model:      "",
				TokenUsage: nil,
			}},
			ReplaceMessages: true,
		},
		{
			Session: db.Session{
				ID:               "duck-copilot-trashed",
				Project:          "alpha",
				Machine:          "test-machine",
				Agent:            "copilot",
				StartedAt:        &ts,
				EndedAt:          &ts,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []db.Message{{
				SessionID:  "duck-copilot-trashed",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  ts,
				Model:      "gpt-5.3-codex",
				TokenUsage: nil,
			}},
			ReplaceMessages: true,
		},
		{
			Session: db.Session{
				ID:               "duck-copilot-user-only",
				Project:          "alpha",
				Machine:          "test-machine",
				Agent:            "copilot",
				StartedAt:        &ts,
				EndedAt:          &ts,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []db.Message{{
				SessionID: "duck-copilot-user-only",
				Ordinal:   0,
				Role:      "user",
				Timestamp: ts,
			}},
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err, "seed copilot sessions")
	require.NoError(t, local.SoftDeleteSession("duck-copilot-trashed"))

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	unbounded := db.UsageFilter{Timezone: "UTC", Agent: "copilot"}
	count, err := store.GetUsageMatchingSessionCount(ctx, unbounded)
	require.NoError(t, err)
	assert.Equal(t, 1, count,
		"soft-deleted and assistant-less sessions must not count unbounded")

	bounded := unbounded
	bounded.From = "2026-03-01"
	bounded.To = "2026-03-01"
	boundedCount, err := store.GetUsageMatchingSessionCount(ctx, bounded)
	require.NoError(t, err)
	assert.Equal(t, count, boundedCount,
		"bounded and unbounded requests must match the same sessions")

	excluded := unbounded
	excluded.ExcludeModel = "gpt-5.3-codex"
	excludedCount, err := store.GetUsageMatchingSessionCount(ctx, excluded)
	require.NoError(t, err)
	assert.Equal(t, 1, excludedCount,
		"empty-model assistant message must survive an ExcludeModel filter")
}

func duckSessionIDs(sessions []db.Session) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func duckSidebarSessionIDs(sessions []db.SidebarSessionIndexRow) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func duckMessageOrdinals(messages []db.Message) []int {
	ordinals := make([]int, len(messages))
	for i, msg := range messages {
		ordinals[i] = msg.Ordinal
	}
	return ordinals
}

func duckProjectNames(projects []db.ProjectInfo) []string {
	names := make([]string, len(projects))
	for i, project := range projects {
		names[i] = project.Name
	}
	return names
}

func duckAgentNames(agents []db.AgentInfo) []string {
	names := make([]string, len(agents))
	for i, agent := range agents {
		names[i] = agent.Name
	}
	return names
}

func duckContentLocations(matches []db.ContentMatch) []string {
	locations := make([]string, len(matches))
	for i, match := range matches {
		locations[i] = match.Location
	}
	return locations
}

func duckContentSessionIDs(matches []db.ContentMatch) []string {
	ids := make([]string, len(matches))
	for i, match := range matches {
		ids[i] = match.SessionID
	}
	return ids
}
