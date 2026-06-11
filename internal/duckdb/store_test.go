package duckdb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeCursorClearsLegacyTotal(t *testing.T) {
	store := &Store{}
	data, err := json.Marshal(db.SessionCursor{
		EndedAt: "2026-01-10T00:00:00.000Z",
		ID:      "legacy-cursor",
		Total:   42,
	})
	require.NoError(t, err)
	raw := base64.RawURLEncoding.EncodeToString(data)

	got, err := store.DecodeCursor(raw)
	require.NoError(t, err)

	assert.Equal(t, "legacy-cursor", got.ID)
	assert.Equal(t, 0, got.Total)
}

func TestStoreReadsSessionsMessagesAndMetadata(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)

	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	assert.Equal(t, 2, page.Total)
	assert.Equal(t, fixture.betaID, page.Sessions[0].ID)
	assert.Equal(t, fixture.alphaID, page.Sessions[1].ID)

	sess, err := store.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "alpha", sess.Project)
	assert.Equal(t, 2, sess.MessageCount)

	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "alpha first", msgs[0].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "search", msgs[1].ToolCalls[0].ToolName)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.SessionCount)
	assert.Equal(t, 3, stats.MessageCount)
	assert.Equal(t, 2, stats.ProjectCount)
	assert.Equal(t, 1, stats.MachineCount)
	require.NotNil(t, stats.EarliestSession)

	projects, err := store.GetProjects(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []db.ProjectInfo{
		{Name: "alpha", SessionCount: 1},
		{Name: "beta", SessionCount: 1},
	}, projects)

	agents, err := store.GetAgents(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []db.AgentInfo{{Name: "claude", SessionCount: 2}}, agents)

	machines, err := store.GetMachines(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"test-machine"}, machines)
}

func TestStoreMessageIDJoinsAreSessionScoped(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)
	insertOtherMachineDuckSession(t, store.duck)

	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[0].ToolCalls)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "search", msgs[1].ToolCalls[0].ToolName)

	content, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "wrong-session-tool",
		Sources:        []string{"tool_input"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, content.Matches, 1)
	assert.Equal(t, "other-session", content.Matches[0].SessionID)
	assert.Equal(t, 0, content.Matches[0].Ordinal)

	pins, err := store.ListPinnedMessages(ctx, "", "alpha")
	require.NoError(t, err)
	foundOtherPin := false
	for _, pin := range pins {
		if pin.SessionID == "other-session" {
			foundOtherPin = true
			require.NotNil(t, pin.Content)
			assert.Equal(t, "from other machine", *pin.Content)
		}
	}
	assert.True(t, foundOtherPin)
}

func TestStoreSearchesMessagesContentAndSecrets(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)

	search, err := store.Search(ctx, db.SearchFilter{Query: "secret token", Limit: 10})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	assert.Equal(t, fixture.alphaID, search.Results[0].SessionID)
	assert.Equal(t, 1, search.Results[0].Ordinal)

	ordinals, err := store.SearchSession(ctx, fixture.alphaID, "duck result")
	require.NoError(t, err)
	assert.Equal(t, []int{1}, ordinals)

	content, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck result",
		Sources:        []string{"tool_result"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, content.Matches, 1)
	assert.Equal(t, "tool_result", content.Matches[0].Location)
	assert.Equal(t, fixture.alphaID, content.Matches[0].SessionID)

	findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Project: "alpha",
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, findings.Findings, 1)
	finding := findings.Findings[0]
	assert.Equal(t, "test_secret", finding.RuleName)
	assert.Equal(t, "alpha", finding.Project)

	source, ok, err := store.SecretFindingSource(ctx, finding.SecretFinding)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "secret token sk-duckdb", source)
}

func TestSearchContentFTSFallsBackToSubstring(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "alpha",
		Mode:           "fts",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.Matches)
	assert.Equal(t, fixture.alphaID, got.Matches[0].SessionID)
	assert.Equal(t, "message", got.Matches[0].Location)
}

func TestSearchContentInvalidModeReturnsInputError(t *testing.T) {
	ctx := context.Background()
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
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

func TestSearchContentRedactsSecretsUnlessRevealed(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-secret-content"
	secretBody := "prefix AKIA" + "7QHWN2DKR4FYPLJM needle suffix"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "secret first", "2026-01-16T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", secretBody, "2026-01-16T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "secret.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	redacted, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "needle",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, redacted.Matches, 1)
	assert.NotContains(t, redacted.Matches[0].Snippet, "AKIA"+"7QHWN2DKR4FYPLJM")

	revealed, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "needle",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		RevealSecrets:  true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, revealed.Matches, 1)
	assert.Contains(t, revealed.Matches[0].Snippet, "AKIA"+"7QHWN2DKR4FYPLJM")
}

func TestSearchGroupsMessagesAndIncludesNameMatches(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "search.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.Search(ctx, db.SearchFilter{Query: "needle", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got.Results, 2)
	assert.Equal(t, "duck-search-content", got.Results[0].SessionID)
	assert.Equal(t, 1, got.Results[0].Ordinal)
	assert.Equal(t, "duck-search-name", got.Results[1].SessionID)
	assert.Equal(t, -1, got.Results[1].Ordinal)
	assert.Equal(t, "needle session name", got.Results[1].Snippet)

	quotedContent, err := store.Search(ctx, db.SearchFilter{Query: `"needle second"`, Limit: 10})
	require.NoError(t, err)
	require.Len(t, quotedContent.Results, 1)
	assert.Equal(t, "duck-search-content", quotedContent.Results[0].SessionID)
	assert.Equal(t, 1, quotedContent.Results[0].Ordinal)

	quotedName, err := store.Search(ctx, db.SearchFilter{Query: `"needle session"`, Limit: 10})
	require.NoError(t, err)
	require.Len(t, quotedName.Results, 1)
	assert.Equal(t, "duck-search-name", quotedName.Results[0].SessionID)
	assert.Equal(t, -1, quotedName.Results[0].Ordinal)

	renamed := "needle override rename"
	require.NoError(t, local.RenameSession("duck-search-name", &renamed))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	overridden, err := store.Search(ctx, db.SearchFilter{Query: "override", Limit: 10})
	require.NoError(t, err)
	require.Len(t, overridden.Results, 1)
	assert.Equal(t, "duck-search-name", overridden.Results[0].SessionID)
	assert.Equal(t, -1, overridden.Results[0].Ordinal)
	assert.Equal(t, "needle override rename", overridden.Results[0].Snippet)
}

func TestStoreCurationMethods(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)

	starred, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{fixture.alphaID}, starred)

	ok, err := store.StarSession(fixture.betaID)
	require.ErrorIs(t, err, db.ErrReadOnly)
	assert.False(t, ok)
	require.ErrorIs(t, store.BulkStarSessions([]string{fixture.betaID}), db.ErrReadOnly)
	require.ErrorIs(t, store.UnstarSession(fixture.alphaID), db.ErrReadOnly)
	starred, err = store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{fixture.alphaID}, starred)

	msgs, err := store.GetAllMessages(ctx, fixture.betaID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	note := "duck pin"
	pinID, err := store.PinMessage(fixture.betaID, msgs[0].ID, &note)
	require.ErrorIs(t, err, db.ErrReadOnly)
	assert.Zero(t, pinID)

	require.ErrorIs(t, store.UnpinMessage(fixture.alphaID, msgs[0].ID), db.ErrReadOnly)
	pins, err := store.ListPinnedMessages(ctx, fixture.alphaID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, "pin alpha", *pins[0].Note)
}

func TestStoreAnalyticsUsageAndTrends(t *testing.T) {
	ctx := context.Background()
	store, fixture := newSyncedStore(t)
	filter := db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}

	summary, err := store.GetAnalyticsSummary(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalSessions)
	assert.Equal(t, 3, summary.TotalMessages)
	assert.Equal(t, 2, summary.ActiveProjects)

	activity, err := store.GetAnalyticsActivity(ctx, filter, "day")
	require.NoError(t, err)
	assert.NotEmpty(t, activity.Series)

	heatmap, err := store.GetAnalyticsHeatmap(ctx, filter, "messages")
	require.NoError(t, err)
	require.Len(t, heatmap.Entries, 31)

	projects, err := store.GetAnalyticsProjects(ctx, filter)
	require.NoError(t, err)
	require.Len(t, projects.Projects, 2)

	hours, err := store.GetAnalyticsHourOfWeek(ctx, filter)
	require.NoError(t, err)
	assert.Len(t, hours.Cells, 168)

	shape, err := store.GetAnalyticsSessionShape(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 2, shape.Count)
	assert.Equal(t, 1, distributionCount(shape.AutonomyDistribution, "1-2"))
	assert.Equal(t, 1, distributionCount(shape.AutonomyDistribution, "<0.5"))

	tools, err := store.GetAnalyticsTools(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 1, tools.TotalCalls)

	velocity, err := store.GetAnalyticsVelocity(ctx, filter)
	require.NoError(t, err)
	assert.NotNil(t, velocity)

	top, err := store.GetAnalyticsTopSessions(ctx, filter, "messages")
	require.NoError(t, err)
	require.NotEmpty(t, top.Sessions)
	assert.Equal(t, fixture.alphaID, top.Sessions[0].ID)

	signals, err := store.GetAnalyticsSignals(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 2, signals.UnscoredSessions)

	trendTerms, err := db.ParseTrendTerms([]string{"alpha"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(ctx, filter, trendTerms, "week")
	require.NoError(t, err)
	assert.Equal(t, 1, trends.Series[0].Total)

	usageFilter := db.UsageFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	}
	usage, err := store.GetDailyUsage(ctx, usageFilter)
	require.NoError(t, err)
	assert.Equal(t, 13, usage.Totals.InputTokens)
	assert.Equal(t, 11, usage.Totals.OutputTokens)
	assert.InDelta(t, 0.000204, usage.Totals.TotalCost, 0.000001)

	topCost, err := store.GetTopSessionsByCost(ctx, usageFilter, 10)
	require.NoError(t, err)
	require.NotEmpty(t, topCost)
	assert.Equal(t, fixture.alphaID, topCost[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, usageFilter)
	require.NoError(t, err)
	assert.Equal(t, 2, counts.Total)
	assert.Equal(t, 1, counts.ByProject["alpha"])

	sessionUsage, err := store.GetSessionUsage(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, sessionUsage)
	assert.True(t, sessionUsage.HasCost)
	assert.Equal(t, []string{"claude-test"}, sessionUsage.Models)
}

func TestLoadPricingSeedsFallbackAndOverlaysOverrides(t *testing.T) {
	ctx := context.Background()
	conn := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	store.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-model": {
			Input: 9, Output: 10, CacheCreation: 11, CacheRead: 12,
		},
	})

	_, err := conn.ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		) VALUES
			('claude-sonnet-4-6', 30, 150, 37.5, 3.0),
			('_fallback_version', 999, 999, 999, 999)`)
	require.NoError(t, err)

	got, err := store.loadPricing(ctx)
	require.NoError(t, err)

	fallback := pricingByPattern(t, pricingpkg.FallbackPricing(), "gpt-5.5")
	require.Contains(t, got, "gpt-5.5")
	assert.Equal(t, fallback.InputPerMTok, got["gpt-5.5"].input)
	assert.Equal(t, fallback.OutputPerMTok, got["gpt-5.5"].output)
	assert.Equal(t, duckRates{
		input: 30, output: 150, cacheCreation: 37.5, cacheRead: 3.0,
	}, got["claude-sonnet-4-6"])
	assert.NotContains(t, got, "_fallback_version")
	assert.Equal(t, duckRates{
		input: 9, output: 10, cacheCreation: 11, cacheRead: 12,
	}, got["custom-model"])
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

func TestAnalyticsTopSessionsFiltersMetricEligibility(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "top-sessions.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET total_output_tokens = 25, has_total_output_tokens = TRUE
		WHERE id = 'duck-top-valid-output'`)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET total_output_tokens = 999, has_total_output_tokens = FALSE
		WHERE id = 'duck-top-untracked-output'`)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET started_at = '2026-01-20T02:00:00.000Z',
			ended_at = '2026-01-20T02:30:00.000Z'
		WHERE id = 'duck-top-valid-duration'`)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions
		SET started_at = NULL, ended_at = NULL
		WHERE id = 'duck-top-missing-duration'`)
	require.NoError(t, err)

	store := NewStoreFromDB(syncer.DB())
	filter := db.AnalyticsFilter{From: "2026-01-20", To: "2026-01-20"}
	output, err := store.GetAnalyticsTopSessions(ctx, filter, "output_tokens")
	require.NoError(t, err)
	assert.Equal(t, "output_tokens", output.Metric)
	require.NotEmpty(t, output.Sessions)
	assert.NotEqual(t, "duck-top-untracked-output", output.Sessions[0].ID)
	for _, session := range output.Sessions {
		assert.NotEqual(t, "duck-top-untracked-output", session.ID)
	}

	duration, err := store.GetAnalyticsTopSessions(ctx, filter, "duration")
	require.NoError(t, err)
	assert.Equal(t, "duration", duration.Metric)
	require.NotEmpty(t, duration.Sessions)
	seenValidDuration := false
	for _, session := range duration.Sessions {
		assert.NotEqual(t, "duck-top-missing-duration", session.ID)
		if session.ID == "duck-top-valid-duration" {
			seenValidDuration = true
			assert.Equal(t, 30.0, session.DurationMin)
		}
	}
	assert.True(t, seenValidDuration, "valid duration session was filtered out")

	unknown, err := store.GetAnalyticsTopSessions(ctx, filter, "not-a-metric")
	require.NoError(t, err)
	assert.Equal(t, "messages", unknown.Metric)
}

func TestAnalyticsProjectsPopulateDailyTrendAndSortByMessages(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "project-analytics.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsProjects(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(t, err)
	require.Len(t, got.Projects, 2)
	assert.Equal(t, "alpha", got.Projects[0].Name)
	assert.Equal(t, 5, got.Projects[0].Messages)
	assert.Equal(t, 5.0, got.Projects[0].DailyTrend)
	assert.Equal(t, "zeta", got.Projects[1].Name)
	assert.Equal(t, 2.0, got.Projects[1].DailyTrend)
}

func TestAnalyticsVelocityUsesMessageCyclesAndBreakdowns(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "velocity.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(t, err)
	assert.Equal(t, 30.0, got.Overall.TurnCycleSec.P50)
	assert.Equal(t, 30.0, got.Overall.FirstResponseSec.P50)
	assert.Equal(t, 2.0, got.Overall.MsgsPerActiveMin)
	assert.Equal(t, 13.0, got.Overall.CharsPerActiveMin)
	assert.Equal(t, 0.5, got.Overall.ToolCallsPerActiveMin)
	require.Len(t, got.ByAgent, 1)
	assert.Equal(t, "claude", got.ByAgent[0].Label)
	assert.Equal(t, 1, got.ByAgent[0].Sessions)
	require.Len(t, got.ByComplexity, 1)
	assert.Equal(t, "1-15", got.ByComplexity[0].Label)
}

func TestAnalyticsVelocitySingleMessageSessionsReturnArrays(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-velocity-single"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "single", "2026-01-22T01:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "single", "2026-01-22T01:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "velocity-single.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-01-31",
	})
	require.NoError(t, err)
	assert.NotNil(t, got.ByAgent)
	assert.Empty(t, got.ByAgent)
	assert.NotNil(t, got.ByComplexity)
	assert.Empty(t, got.ByComplexity)
}

func TestGetSessionTimingPopulatesSharedTimingPayload(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-timing"
	startedAt := "2026-01-20T00:00:00.000Z"
	endedAt := "2026-01-20T00:03:00.000Z"
	sess := syncSession(sessionID, "alpha", "timing first", startedAt, 2)
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
				}),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "timing.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	timing, err := store.GetSessionTiming(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, timing)
	assert.Equal(t, sessionID, timing.SessionID)
	assert.Equal(t, int64(180000), timing.TotalDurationMs)
	assert.Equal(t, 1, timing.TurnCount)
	assert.Equal(t, 1, timing.ToolCallCount)
	assert.False(t, timing.Running)
	require.Len(t, timing.Turns, 1)
	assert.Equal(t, 1, timing.Turns[0].Ordinal)
	require.NotNil(t, timing.Turns[0].DurationMs)
	assert.Equal(t, int64(120000), *timing.Turns[0].DurationMs)
	require.Len(t, timing.Turns[0].Calls, 1)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(120000), *timing.Turns[0].Calls[0].DurationMs)
}

func TestGetAllMessagesDoesNotTruncateAtDefaultLimit(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "large.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, got, messageCount)
	assert.Equal(t, "message-1004", got[messageCount-1].Content)
}

func TestSearchContentRegexDoesNotUseLiteralLikePrefilter(t *testing.T) {
	ctx := context.Background()
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
	ctx := context.Background()
	store, _ := newSyncedStore(t)

	first, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `alpha|duck\s+result`,
		Mode:           "regex",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
	})
	require.NoError(t, err)
	require.Len(t, first.Matches, 1)
	assert.Equal(t, "message", first.Matches[0].Location)
	assert.Equal(t, "alpha first", first.Matches[0].Snippet)
	assert.Equal(t, 1, first.NextCursor)

	second, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `alpha|duck\s+result`,
		Mode:           "regex",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
		Cursor:         first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Matches, 1)
	assert.Equal(t, "tool_result", second.Matches[0].Location)
	assert.Equal(t, "duck result", second.Matches[0].Snippet)
}

func TestSearchContentRegexOrdersBySessionRecency(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "regex-order.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        `target\s+word`,
		Mode:           "regex",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, got.Matches, 2)
	assert.Equal(t, "z-new-regex", got.Matches[0].SessionID)
	assert.Equal(t, "a-old-regex", got.Matches[1].SessionID)
}

func TestSearchContentSubstringPaginatesAfterGlobalOrdering(t *testing.T) {
	ctx := context.Background()
	store, _ := newSyncedStore(t)

	first, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
	})
	require.NoError(t, err)
	require.Len(t, first.Matches, 1)
	assert.Equal(t, "message", first.Matches[0].Location)
	assert.Equal(t, "secret token sk-duckdb", first.Matches[0].Snippet)
	assert.Equal(t, 1, first.NextCursor)

	second, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:        "duck",
		Sources:        []string{"tool_result", "messages"},
		IncludeOneShot: true,
		Limit:          1,
		Cursor:         first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Matches, 1)
	assert.Equal(t, "tool_result", second.Matches[0].Location)
	assert.Equal(t, "duck result", second.Matches[0].Snippet)
}

func TestSearchContentToolResultEmptyToolUseIDNotSuppressedByEvents(t *testing.T) {
	ctx := context.Background()
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
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "empty-tool-use.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
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
			require.NoError(t, err)
			require.Len(t, got.Matches, 1)
			assert.Equal(t, "tool_result", got.Matches[0].Location)
			assert.Contains(t, got.Matches[0].Snippet, "legacy needle")
		})
	}
}

func TestSearchContentLegacyToolResultsUseCallIndexTieBreaker(t *testing.T) {
	ctx := context.Background()
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
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "tool-order.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
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
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, first)
			require.NotZero(t, page.NextCursor)

			filter.Cursor = page.NextCursor
			page, err = store.SearchContent(ctx, filter)
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, second)
		})
	}
}

func TestSearchContentToolResultEventsUseCallIndexTieBreaker(t *testing.T) {
	ctx := context.Background()
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
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "tool-event-order.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
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
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, first)
			require.NotZero(t, page.NextCursor)

			filter.Cursor = page.NextCursor
			page, err = store.SearchContent(ctx, filter)
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, second)
		})
	}
}

func TestAnalyticsActivityMessageCountsRespectSessionFilter(t *testing.T) {
	ctx := context.Background()
	store, _ := newSyncedStore(t)

	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From:    "2026-01-01",
		To:      "2026-01-31",
		Project: "alpha",
	}, "day")
	require.NoError(t, err)
	require.Len(t, activity.Series, 1)
	assert.Equal(t, "2026-01-10", activity.Series[0].Date)
	assert.Equal(t, 1, activity.Series[0].UserMessages)
	assert.Equal(t, 1, activity.Series[0].AssistantMessages)
	assert.Equal(t, 2, activity.Series[0].ByAgent["claude"])
}

func TestAnalyticsActivityCountsToolCallRows(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "activity-tools.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx,
		`UPDATE messages SET has_tool_use = FALSE WHERE session_id = ?`,
		sessionID,
	)
	require.NoError(t, err)

	store := NewStoreFromDB(syncer.DB())
	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2026-01-23",
		To:   "2026-01-23",
	}, "day")
	require.NoError(t, err)
	require.Len(t, activity.Series, 1)
	assert.Equal(t, 2, activity.Series[0].ToolCalls)
}

func TestAnalyticsActivitySkipsSystemUserMessages(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "activity-system.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	activity, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2026-01-23",
		To:   "2026-01-23",
	}, "day")
	require.NoError(t, err)
	require.Len(t, activity.Series, 1)
	assert.Equal(t, 2, activity.Series[0].Messages)
	assert.Equal(t, 1, activity.Series[0].UserMessages)
	assert.Equal(t, 2, activity.Series[0].ByAgent["claude"])
}

func TestAnalyticsSessionFiltersUseMessageTimeForHourAndDay(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "time-filter.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
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
	require.NoError(t, err)
	assert.Equal(t, 1, summary.TotalSessions)
}

func TestAnalyticsTerminationFilterUsesSharedStateSemantics(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "termination.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From:        "2026-01-21",
		To:          "2026-01-21",
		Timezone:    "UTC",
		Termination: "unclean",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalSessions)
}

func TestAnalyticsActiveSinceParsesEquivalentOffsets(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "active-since.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		ActiveSince: "2026-01-21T11:00:00+02:00",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.TotalSessions)
}

func TestAnalyticsHourOfWeekRespectsSessionFilters(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "hour-of-week.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From:     "2026-01-21",
		To:       "2026-01-21",
		Timezone: "UTC",
		Project:  "alpha",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, hourOfWeekMessages(got.Cells, 2, 9))
}

func TestAnalyticsHourOfWeekIncludesOvernightMessages(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "hour-of-week-overnight.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From:     "2026-01-21",
		To:       "2026-01-21",
		Timezone: "UTC",
	})
	require.NoError(t, err)
	// 2026-01-21 is a Wednesday (ISO dow 2). The session falls inside the
	// date window, so all of its messages count, including the one whose
	// local date crosses past the To bound.
	assert.Equal(t, 1, hourOfWeekMessages(got.Cells, 2, 23))
	assert.Equal(t, 1, hourOfWeekMessages(got.Cells, 3, 0))
}

func TestTrendsTermsApplySessionFiltersAndSystemPrefixExclusion(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "trends.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	trendTerms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From:     "2026-01-22",
		To:       "2026-01-22",
		Timezone: "UTC",
		Project:  "alpha",
	}, trendTerms, "day")
	require.NoError(t, err)
	require.Len(t, trends.Series, 1)
	assert.Equal(t, 1, trends.Series[0].Total)
}

func TestDailyUsageDefaultsToLocalTimezone(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("DuckLocal", -5*60*60)
	t.Cleanup(func() { time.Local = oldLocal })

	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  3,
		OutputPerMTok: 15,
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "usage-local.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-01",
		To:   "2026-01-01",
	})
	require.NoError(t, err)
	require.Len(t, got.Daily, 1)
	assert.Equal(t, "2026-01-01", got.Daily[0].Date)
	assert.Equal(t, 1, got.Totals.InputTokens)
	assert.Equal(t, 2, got.Totals.OutputTokens)
}

func TestDailyUsageActiveSinceUsesSessionActivity(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "usage-active-since.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:        "2026-01-01",
		To:          "2026-01-01",
		Timezone:    "UTC",
		ActiveSince: "2026-01-02T00:00:00.000Z",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Totals.InputTokens)
	assert.Equal(t, 2, got.Totals.OutputTokens)
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
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  3,
		OutputPerMTok: 15,
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "usage.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())
	filter := db.UsageFilter{From: "2026-01-01", To: "2026-01-31"}

	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 1, daily.Totals.InputTokens)
	assert.Equal(t, 2, daily.Totals.OutputTokens)

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err)
	require.Len(t, top, 1)
	assert.Equal(t, "duck-usage-a", top[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 1, counts.Total)
	assert.Equal(t, 1, counts.ByProject["alpha"])
	assert.NotContains(t, counts.ByProject, "beta")

	sessionUsage, err := store.GetSessionUsage(ctx, "duck-usage-b")
	require.NoError(t, err)
	require.NotNil(t, sessionUsage)
	assert.True(t, sessionUsage.HasCost)
	assert.InDelta(t, 0.000033, sessionUsage.CostUSD, 0.000001)
	assert.Equal(t, []string{"claude-test"}, sessionUsage.Models)
}

func TestUsageDedupPrefersInRangeDuplicate(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "claude-test",
		InputPerMTok:  3,
		OutputPerMTok: 15,
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
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "usage-edge.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	// The duplicate before midnight is outside the window but inside
	// the padded UTC bounds and sorts first by timestamp. It must not
	// win the dedup and suppress the in-range duplicate.
	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From: "2026-01-13", To: "2026-01-13", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Totals.InputTokens)
	assert.Equal(t, 2, got.Totals.OutputTokens)
}

func TestTrendsTermsWordBoundaryAndOverlapParity(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "trends-parity.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	terms, err := db.ParseTrendTerms([]string{"seam", "test|testing"})
	require.NoError(t, err)
	filter := db.AnalyticsFilter{
		From: "2026-01-22", To: "2026-01-22", Timezone: "UTC",
	}

	got, err := store.GetTrendsTerms(ctx, filter, terms, "day")
	require.NoError(t, err)
	require.Len(t, got.Series, 2)
	// Word-bounded: "seamless" does not count for "seam", and
	// "testing" is not double-counted via its "test" substring.
	assert.Equal(t, 2, got.Series[0].Total)
	assert.Equal(t, 2, got.Series[1].Total)

	want, err := local.GetTrendsTerms(ctx, filter, terms, "day")
	require.NoError(t, err)
	require.Len(t, want.Series, 2)
	assert.Equal(t, want.Series[0].Total, got.Series[0].Total)
	assert.Equal(t, want.Series[1].Total, got.Series[1].Total)
}

func TestDailyUsageBreakdownsAndCacheSavings(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         3,
		OutputPerMTok:        15,
		CacheCreationPerMTok: 1,
		CacheReadPerMTok:     0.5,
	}}))
	sessionID := "duck-usage-breakdowns"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:  syncSession(sessionID, "alpha", "usage first", "2026-01-17T00:00:00.000Z", 1),
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
	}})
	require.NoError(t, err)

	syncer := newTestSync(t, filepath.Join(t.TempDir(), "usage-breakdowns.duckdb"), local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-01-01",
		To:         "2026-01-31",
		Breakdowns: true,
	})
	require.NoError(t, err)
	require.Len(t, got.Daily, 1)
	day := got.Daily[0]
	require.Len(t, day.ModelBreakdowns, 1)
	require.Len(t, day.ProjectBreakdowns, 1)
	require.Len(t, day.AgentBreakdowns, 1)
	assert.Equal(t, "alpha", day.ProjectBreakdowns[0].Project)
	assert.Equal(t, "claude", day.AgentBreakdowns[0].Agent)
	assert.InDelta(t, 0.00001, got.Totals.CacheSavings, 0.000001)
}

func TestGetChildSessionsOrderedByStartedAt(t *testing.T) {
	ctx := context.Background()
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
	require.NoError(t, err)
	require.NoError(t, local.SoftDeleteSession("duck-child-deleted"))

	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	children, err := store.GetChildSessions(ctx, "duck-parent")
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"duck-child-early", "duck-child-late"},
		duckSessionIDs(children))
}

func newSyncedStore(t *testing.T) (*Store, syncFixture) {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB()), fixture
}
