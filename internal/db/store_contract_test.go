package db

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type storeContractBackend struct {
	name                string
	open                func(t *testing.T) Store
	seed                func(t *testing.T, store Store) storeContractFixture
	supportsLocalWrites bool
}

type storeContractFixture struct {
	alphaID     string
	betaID      string
	gammaID     string
	oldID       string
	automatedID string
	childID     string
	deletedID   string
}

func storeContractBackends() []storeContractBackend {
	// The db package can only register SQLite without importing remote
	// providers back into their dependency root. Read-only backends run
	// equivalent store-contract suites in their own packages.
	return []storeContractBackend{
		{
			name: "sqlite",
			open: func(t *testing.T) Store {
				t.Helper()
				return testDB(t)
			},
			seed: func(t *testing.T, store Store) storeContractFixture {
				t.Helper()
				return seedStoreContractSQLite(t, store)
			},
			supportsLocalWrites: true,
		},
	}
}

func TestStoreContract(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, store Store, fixture storeContractFixture, backend storeContractBackend)
	}{
		{"sessions_cursor_filters_and_dates", contractSessionsCursorFiltersAndDates},
		{"messages_ordering_and_tool_results", contractMessagesOrderingAndToolResults},
		{"search_modes_and_secret_findings", contractSearchModesAndSecretFindings},
		{"stars_and_pins", contractStarsAndPins},
		{"analytics_trends_and_usage", contractAnalyticsTrendsAndUsage},
		{"local_only_methods", contractLocalOnlyMethods},
	}

	for _, backend := range storeContractBackends() {
		t.Run(backend.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					store := backend.open(t)
					fixture := backend.seed(t, store)
					tt.run(t, store, fixture, backend)
				})
			}
		})
	}
}

func contractSessionsCursorFiltersAndDates(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	_ storeContractBackend,
) {
	t.Helper()
	ctx := context.Background()

	page, err := store.ListSessions(ctx, SessionFilter{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, 5, page.Total)
	require.Len(t, page.Sessions, 2)
	require.Equal(t, []string{fixture.gammaID, fixture.automatedID}, sessionIDs(page.Sessions))
	require.NotEmpty(t, page.NextCursor)

	cur, err := store.DecodeCursor(page.NextCursor)
	require.NoError(t, err)
	require.Equal(t, fixture.automatedID, cur.ID)
	require.Equal(t, 5, cur.Total)

	next, err := store.ListSessions(ctx, SessionFilter{
		Limit:  2,
		Cursor: page.NextCursor,
	})
	require.NoError(t, err)
	require.Equal(t, 5, next.Total)
	require.Equal(t, []string{fixture.betaID, fixture.alphaID}, sessionIDs(next.Sessions))

	alphaOnly, err := store.ListSessions(ctx, SessionFilter{
		Project:     "alpha",
		DateFrom:    "2026-01-10",
		DateTo:      "2026-01-12",
		MinMessages: 3,
	})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.gammaID, fixture.alphaID}, sessionIDs(alphaOnly.Sessions))

	noAutomation, err := store.ListSessions(ctx, SessionFilter{
		ExcludeAutomated: true,
		Limit:            10,
	})
	require.NoError(t, err)
	require.NotContains(t, sessionIDs(noAutomation.Sessions), fixture.automatedID)

	withChildren, err := store.ListSessions(ctx, SessionFilter{
		Project:         "alpha",
		IncludeChildren: true,
		Limit:           10,
	})
	require.NoError(t, err)
	require.Contains(t, sessionIDs(withChildren.Sessions), fixture.childID)

	children, err := store.GetChildSessions(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Equal(t, []string{fixture.childID}, sessionIDs(children))

	trashed, err := store.GetSession(ctx, fixture.deletedID)
	require.NoError(t, err)
	require.Nil(t, trashed)

	full, err := store.GetSessionFull(ctx, fixture.deletedID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.DeletedAt)

	// GetSessionFull returns the visible name on every backend:
	// the user rename when set, else the agent-provided session name.
	named, err := store.GetSessionFull(ctx, fixture.oldID)
	require.NoError(t, err)
	require.NotNil(t, named)
	require.NotNil(t, named.DisplayName)
	require.Equal(t, "Old Contract Name", *named.DisplayName)

	index, err := store.GetSidebarSessionIndex(ctx, SessionFilter{
		Project: "alpha",
	})
	require.NoError(t, err)
	require.Contains(t, sidebarSessionIDs(index.Sessions), fixture.childID)

	stats, err := store.GetStats(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, 5, stats.SessionCount)
	require.Equal(t, 13, stats.MessageCount)
	require.Equal(t, 3, stats.ProjectCount)

	projects, err := store.GetProjects(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta", "review"}, projectNames(projects))

	agents, err := store.GetAgents(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"claude", "codex", "roborev"}, agentNames(agents))

	machines, err := store.GetMachines(ctx, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"linux", "mac"}, machines)
}

func contractMessagesOrderingAndToolResults(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	_ storeContractBackend,
) {
	t.Helper()
	ctx := context.Background()

	asc, err := store.GetMessages(ctx, fixture.alphaID, 1, 3, true)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3}, messageOrdinals(asc))
	require.Len(t, asc[0].ToolCalls, 1)
	require.Equal(t, "search", asc[0].ToolCalls[0].ToolName)
	require.Len(t, asc[0].ToolCalls[0].ResultEvents, 1)
	require.Equal(t, "tool stream found contract parity event", asc[0].ToolCalls[0].ResultEvents[0].Content)

	desc, err := store.GetMessages(ctx, fixture.alphaID, 3, 2, false)
	require.NoError(t, err)
	require.Equal(t, []int{3, 2}, messageOrdinals(desc))

	all, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3, 4}, messageOrdinals(all))

	activity, err := store.GetSessionActivity(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, activity)
	require.Equal(t, 5, activity.TotalMessages)
	require.NotEmpty(t, activity.Buckets)

	timing, err := store.GetSessionTiming(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, timing)
}

func contractSearchModesAndSecretFindings(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	_ storeContractBackend,
) {
	t.Helper()
	ctx := context.Background()

	if store.HasFTS() {
		search, err := store.Search(ctx, SearchFilter{
			Query: "duckdb",
			Limit: 5,
		})
		require.NoError(t, err)
		require.NotEmpty(t, search.Results)
		require.Equal(t, fixture.alphaID, search.Results[0].SessionID)

		nameSearch, err := store.Search(ctx, SearchFilter{
			Query: "Old Contract Name",
			Limit: 5,
		})
		require.NoError(t, err)
		require.Equal(t, []string{fixture.oldID}, searchResultIDs(nameSearch.Results))
	}

	substring, err := store.SearchContent(ctx, ContentSearchFilter{
		Pattern:        "contract-input",
		Mode:           "substring",
		Sources:        []string{"tool_input"},
		IncludeOneShot: true,
		Limit:          5,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"tool_input"}, contentLocations(substring.Matches))

	regex, err := store.SearchContent(ctx, ContentSearchFilter{
		Pattern:        `trend\s+trend`,
		Mode:           "regex",
		Sources:        []string{"messages"},
		IncludeOneShot: true,
		Limit:          5,
	})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.gammaID}, contentSessionIDs(regex.Matches))

	if store.HasFTS() {
		fts, err := store.SearchContent(ctx, ContentSearchFilter{
			Pattern:        "analytics",
			Mode:           "fts",
			Sources:        []string{"messages"},
			IncludeOneShot: true,
			Limit:          5,
		})
		require.NoError(t, err)
		require.Contains(t, contentSessionIDs(fts.Matches), fixture.alphaID)
	}

	findings, err := store.ListSecretFindings(ctx, SecretFindingFilter{
		Project:       "alpha",
		RulesVersions: []string{"contract-rules-v1"},
		Limit:         10,
	})
	require.NoError(t, err)
	require.Len(t, findings.Findings, 2)
	require.Equal(t, fixture.alphaID, findings.Findings[0].SessionID)

	var messageSecretSource string
	for _, finding := range findings.Findings {
		source, ok, err := store.SecretFindingSource(ctx, finding.SecretFinding)
		require.NoError(t, err)
		require.True(t, ok)
		if finding.LocationKind == "message" {
			messageSecretSource = source
		}
	}
	require.Contains(t, messageSecretSource, "contract secret")

	secretSessions, err := store.ListSessions(ctx, SessionFilter{
		HasSecret:            true,
		SecretsRulesVersions: []string{"contract-rules-v1"},
		Limit:                10,
	})
	require.NoError(t, err)
	require.Equal(t, []string{fixture.alphaID}, sessionIDs(secretSessions.Sessions))
}

func contractStarsAndPins(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	backend storeContractBackend,
) {
	t.Helper()
	if !backend.supportsLocalWrites {
		_, err := store.StarSession(fixture.alphaID)
		require.ErrorIs(t, err, ErrReadOnly)
		_, err = store.PinMessage(fixture.alphaID, 1, nil)
		require.ErrorIs(t, err, ErrReadOnly)
		return
	}

	ctx := context.Background()
	ok, err := store.StarSession(fixture.alphaID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.BulkStarSessions([]string{fixture.gammaID, "missing-session"}))

	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{fixture.alphaID, fixture.gammaID}, stars)

	require.NoError(t, store.UnstarSession(fixture.gammaID))
	stars, err = store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{fixture.alphaID}, stars)

	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Greater(t, len(msgs), 1)
	note := "contract pin"
	pinID, err := store.PinMessage(fixture.alphaID, msgs[1].ID, &note)
	require.NoError(t, err)
	require.Positive(t, pinID)

	sessionPins, err := store.ListPinnedMessages(ctx, fixture.alphaID, "")
	require.NoError(t, err)
	require.Len(t, sessionPins, 1)
	require.Equal(t, 1, sessionPins[0].Ordinal)
	require.Equal(t, note, *sessionPins[0].Note)

	allPins, err := store.ListPinnedMessages(ctx, "", "alpha")
	require.NoError(t, err)
	require.Len(t, allPins, 1)
	require.Equal(t, fixture.alphaID, allPins[0].SessionID)
	require.NotNil(t, allPins[0].Content)

	require.NoError(t, store.UnpinMessage(fixture.alphaID, msgs[1].ID))
	sessionPins, err = store.ListPinnedMessages(ctx, fixture.alphaID, "")
	require.NoError(t, err)
	require.Empty(t, sessionPins)
}

func contractAnalyticsTrendsAndUsage(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	_ storeContractBackend,
) {
	t.Helper()
	ctx := context.Background()

	summary, err := store.GetAnalyticsSummary(ctx, AnalyticsFilter{
		From:     "2026-01-09",
		To:       "2026-01-12",
		Timezone: "UTC",
	})
	require.NoError(t, err)
	require.Equal(t, 5, summary.TotalSessions)
	require.Equal(t, 13, summary.TotalMessages)
	require.Equal(t, 440, summary.TotalOutputTokens)
	require.Equal(t, 4, summary.TokenReportingSessions)
	require.Equal(t, 3, summary.ActiveProjects)
	require.Equal(t, 4, summary.ActiveDays)
	require.Equal(t, 2, summary.Agents["claude"].Sessions)

	noAutomation, err := store.GetAnalyticsSummary(ctx, AnalyticsFilter{
		From:             "2026-01-09",
		To:               "2026-01-12",
		ExcludeAutomated: true,
	})
	require.NoError(t, err)
	require.Equal(t, 4, noAutomation.TotalSessions)

	activity, err := store.GetAnalyticsActivity(ctx, AnalyticsFilter{
		From:     "2026-01-09",
		To:       "2026-01-12",
		Timezone: "UTC",
	}, "day")
	require.NoError(t, err)
	require.Len(t, activity.Series, 4)

	tools, err := store.GetAnalyticsTools(ctx, AnalyticsFilter{
		From: "2026-01-10",
		To:   "2026-01-10",
	})
	require.NoError(t, err)
	require.Equal(t, 1, tools.TotalCalls)
	require.NotEmpty(t, tools.ByCategory)
	require.Equal(t, "search", tools.ByCategory[0].Category)

	trends, err := store.GetTrendsTerms(ctx, AnalyticsFilter{
		From:     "2026-01-09",
		To:       "2026-01-12",
		Timezone: "UTC",
	}, []TrendTermInput{
		{Term: "trend", Variants: []string{"trend"}, Matchers: []string{"trend"}},
	}, "day")
	require.NoError(t, err)
	require.Equal(t, "day", trends.Granularity)
	require.Equal(t, 2, trends.Series[0].Total)
	require.Equal(t, 13, trends.MessageCount)

	daily, err := store.GetDailyUsage(ctx, UsageFilter{
		From:       "2026-01-10",
		To:         "2026-01-12",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(daily.Daily), 2)
	require.Equal(t, 315, daily.Totals.InputTokens)
	require.Equal(t, 130, daily.Totals.OutputTokens)
	require.InDelta(t, 0.002435, daily.Totals.TotalCost, 0.00001)

	top, err := store.GetTopSessionsByCost(ctx, UsageFilter{
		From: "2026-01-10",
		To:   "2026-01-12",
	}, 3)
	require.NoError(t, err)
	require.NotEmpty(t, top)
	require.Equal(t, fixture.alphaID, top[0].SessionID)

	counts, err := store.GetUsageSessionCounts(ctx, UsageFilter{
		From: "2026-01-10",
		To:   "2026-01-12",
	})
	require.NoError(t, err)
	require.Equal(t, 3, counts.Total)
	require.Equal(t, 2, counts.ByProject["alpha"])

	usage, err := store.GetSessionUsage(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.True(t, usage.HasTokenData)
	require.True(t, usage.HasCost)
	require.Equal(t, []string{"claude-sonnet-contract"}, usage.Models)
	require.InDelta(t, 0.002355, usage.CostUSD, 0.00001)
}

func contractLocalOnlyMethods(
	t *testing.T,
	store Store,
	fixture storeContractFixture,
	backend storeContractBackend,
) {
	t.Helper()
	ctx := context.Background()

	if !backend.supportsLocalWrites {
		require.True(t, store.ReadOnly())
		ignoredName := "ignored"
		requireReadOnly(t, store.RenameSession(fixture.alphaID, &ignoredName))
		requireReadOnly(t, store.SoftDeleteSession(fixture.alphaID))
		_, err := store.RestoreSession(fixture.deletedID)
		requireReadOnly(t, err)
		_, err = store.DeleteSessionIfTrashed(fixture.deletedID)
		requireReadOnly(t, err)
		_, err = store.EmptyTrash()
		requireReadOnly(t, err)
		_, err = store.InsertInsight(Insight{})
		requireReadOnly(t, err)
		requireReadOnly(t, store.DeleteInsight(1))
		return
	}

	require.False(t, store.ReadOnly())
	renamed := "Renamed contract session"
	require.NoError(t, store.RenameSession(fixture.betaID, &renamed))
	beta, err := store.GetSession(ctx, fixture.betaID)
	require.NoError(t, err)
	require.NotNil(t, beta)
	require.Equal(t, renamed, *beta.DisplayName)

	require.NoError(t, store.SoftDeleteSession(fixture.betaID))
	beta, err = store.GetSession(ctx, fixture.betaID)
	require.NoError(t, err)
	require.Nil(t, beta)

	trashed, err := store.ListTrashedSessions(ctx)
	require.NoError(t, err)
	require.Contains(t, sessionIDs(trashed), fixture.betaID)

	restored, err := store.RestoreSession(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, restored)

	deleted, err := store.DeleteSessionIfTrashed(fixture.deletedID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	emptyCount, err := store.EmptyTrash()
	require.NoError(t, err)
	require.Zero(t, emptyCount)

	insightID, err := store.InsertInsight(Insight{
		Type:     "contract",
		DateFrom: "2026-01-12",
		DateTo:   "2026-01-12",
		Agent:    "claude",
		Content:  "Backend contract test insight",
	})
	require.NoError(t, err)
	require.Positive(t, insightID)
	insight, err := store.GetInsight(ctx, insightID)
	require.NoError(t, err)
	require.NotNil(t, insight)
	require.Equal(t, "Backend contract test insight", insight.Content)
	list, err := store.ListInsights(ctx, InsightFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, store.DeleteInsight(insightID))
}

func seedStoreContractSQLite(
	t *testing.T,
	store Store,
) storeContractFixture {
	t.Helper()

	pricingStore, ok := store.(interface {
		UpsertModelPricing([]ModelPricing) error
	})
	require.True(t, ok, "sqlite contract seed requires pricing writer")
	require.NoError(t, pricingStore.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "claude-sonnet-contract",
			InputPerMTok:         3,
			OutputPerMTok:        15,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.3,
		},
		{
			ModelPattern:         "codex-mini-contract",
			InputPerMTok:         1,
			OutputPerMTok:        5,
			CacheCreationPerMTok: 1,
			CacheReadPerMTok:     0.1,
		},
	}))

	fixture := storeContractFixture{
		alphaID:     "contract-alpha",
		betaID:      "contract-beta",
		gammaID:     "contract-gamma",
		oldID:       "contract-old",
		automatedID: "contract-automated",
		childID:     "contract-alpha-child",
		deletedID:   "contract-deleted",
	}

	alphaSecret := "The contract secret is sk-contract-alpha-123."
	alphaSecretStart := len("The contract secret is ")
	toolInput := `{"query":"contract-input duckdb parity secret"}`
	toolSecretStart := len(`{"query":"`)
	callIndex := 0
	alphaHealthScore := 88
	alphaHealthGrade := "A"
	gammaHealthScore := 55
	gammaHealthGrade := "C"

	writes := []SessionBatchWrite{
		contractSessionWrite(contractSessionSeed{
			id:           fixture.alphaID,
			project:      "alpha",
			machine:      "mac",
			agent:        "claude",
			firstMessage: "Alpha parity investigation starts with duckdb parity keyword.",
			sessionName:  "Alpha DuckDB parity",
			startedAt:    "2026-01-10T12:00:00Z",
			endedAt:      "2026-01-10T12:06:00Z",
			userMessages: 3,
			outputTokens: 320,
			peakTokens:   900,
			messages: []Message{
				contractMessage(fixture.alphaID, 0, "user", "Alpha parity investigation starts with duckdb parity keyword.", "2026-01-10T12:00:00Z"),
				contractMessage(fixture.alphaID, 1, "assistant", "Running contract search for backend parity.", "2026-01-10T12:01:00Z",
					withModel("claude-sonnet-contract"),
					withTokenUsage(`{"input_tokens":200,"output_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`),
					withToolCall(ToolCall{
						ToolName:      "search",
						Category:      "search",
						ToolUseID:     "tool-alpha-1",
						InputJSON:     toolInput,
						ResultContent: "legacy result mentions parity",
						ResultEvents: []ToolResultEvent{
							{
								Source:    "tool",
								Status:    "complete",
								Content:   "tool stream found contract parity event",
								Timestamp: "2026-01-10T12:01:30Z",
							},
						},
					})),
				contractMessage(fixture.alphaID, 2, "user", "Please add backend contract tests for cursor pagination and stars.", "2026-01-10T12:02:00Z"),
				contractMessage(fixture.alphaID, 3, "assistant", "Implemented analytics summary with usage rollup.", "2026-01-10T12:04:00Z",
					withModel("claude-sonnet-contract"),
					withTokenUsage(`{"input_tokens":100,"output_tokens":30,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`)),
				contractMessage(fixture.alphaID, 4, "user", alphaSecret, "2026-01-10T12:05:00Z"),
			},
			usageEvents: []UsageEvent{
				{
					Source:       "hermes",
					Model:        "claude-sonnet-contract",
					InputTokens:  10,
					OutputTokens: 5,
					OccurredAt:   "2026-01-10T12:05:30Z",
					DedupKey:     "alpha-hermes",
				},
			},
			signals: SessionSignalUpdate{
				ToolFailureSignalCount: 1,
				ToolRetryCount:         1,
				Outcome:                "success",
				OutcomeConfidence:      "high",
				EndedWithRole:          "user",
				HealthScore:            &alphaHealthScore,
				HealthGrade:            &alphaHealthGrade,
				HasToolCalls:           true,
				HasContextData:         true,
				SecretLeakCount:        2,
				SecretsRulesVersion:    "contract-rules-v1",
			},
			findings: []SecretFinding{
				{
					SessionID:      fixture.alphaID,
					RuleName:       "contract_secret",
					Confidence:     "definite",
					LocationKind:   "message",
					MessageOrdinal: 4,
					MatchStart:     alphaSecretStart,
					MatchEnd:       alphaSecretStart + len("sk-contract-alpha-123"),
					MatchIndex:     0,
					RedactedMatch:  "sk-contract-alpha-...",
					RulesVersion:   "contract-rules-v1",
				},
				{
					SessionID:      fixture.alphaID,
					RuleName:       "contract_secret",
					Confidence:     "candidate",
					LocationKind:   "tool_input",
					MessageOrdinal: 1,
					CallIndex:      &callIndex,
					MatchStart:     toolSecretStart,
					MatchEnd:       toolSecretStart + len("contract-input"),
					MatchIndex:     1,
					RedactedMatch:  "contract-input...",
					RulesVersion:   "contract-rules-v1",
				},
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.betaID,
			project:      "beta",
			machine:      "linux",
			agent:        "codex",
			firstMessage: "Beta recency search target.",
			startedAt:    "2026-01-11T09:00:00Z",
			endedAt:      "2026-01-11T09:05:00Z",
			userMessages: 1,
			outputTokens: 40,
			peakTokens:   120,
			messages: []Message{
				contractMessage(fixture.betaID, 0, "user", "Beta recency search target.", "2026-01-11T09:00:00Z"),
				contractMessage(fixture.betaID, 1, "assistant", "One-shot beta answer.", "2026-01-11T09:01:00Z",
					withModel("codex-mini-contract"),
					withTokenUsage(`{"input_tokens":5,"output_tokens":5}`)),
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.gammaID,
			project:      "alpha",
			machine:      "mac",
			agent:        "codex",
			firstMessage: "Gamma trend request.",
			startedAt:    "2026-01-12T10:00:00Z",
			endedAt:      "2026-01-12T10:10:00Z",
			userMessages: 2,
			outputTokens: 60,
			peakTokens:   300,
			messages: []Message{
				contractMessage(fixture.gammaID, 0, "user", "Gamma regex target mentions trend trend.", "2026-01-12T10:00:00Z"),
				contractMessage(fixture.gammaID, 1, "assistant", "Gamma answer uses codex usage.", "2026-01-12T10:02:00Z",
					withModel("codex-mini-contract"),
					withTokenUsage(`{"input_tokens":0,"output_tokens":0}`)),
				contractMessage(fixture.gammaID, 2, "user", "Another gamma request.", "2026-01-12T10:03:00Z"),
			},
			usageEvents: []UsageEvent{
				{
					Source:       "hermes",
					Model:        "codex-mini-contract",
					InputTokens:  0,
					OutputTokens: 30,
					CostUSD:      floatPtr(0.00005),
					CostStatus:   "reported",
					CostSource:   "fixture",
					OccurredAt:   "2026-01-12T10:04:00Z",
					DedupKey:     "gamma-hermes",
				},
			},
			signals: SessionSignalUpdate{
				Outcome:           "failed",
				OutcomeConfidence: "medium",
				EndedWithRole:     "user",
				HealthScore:       &gammaHealthScore,
				HealthGrade:       &gammaHealthGrade,
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.automatedID,
			project:      "review",
			machine:      "mac",
			agent:        "roborev",
			firstMessage: "You are a code reviewer. Review the code changes introduced by commit abc123.",
			startedAt:    "2026-01-11T13:00:00Z",
			endedAt:      "2026-01-11T13:01:00Z",
			userMessages: 1,
			messages: []Message{
				contractMessage(fixture.automatedID, 0, "user", "You are a code reviewer. Review the code changes introduced by commit abc123.", "2026-01-11T13:00:00Z"),
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.oldID,
			project:      "alpha",
			machine:      "linux",
			agent:        "claude",
			firstMessage: "Old contract first message.",
			sessionName:  "Old Contract Name",
			startedAt:    "2026-01-09T08:00:00Z",
			endedAt:      "2026-01-09T08:03:00Z",
			userMessages: 1,
			outputTokens: 20,
			peakTokens:   100,
			messages: []Message{
				contractMessage(fixture.oldID, 0, "user", "Old contract first message.", "2026-01-09T08:00:00Z"),
				contractMessage(fixture.oldID, 1, "assistant", "Old contract answer.", "2026-01-09T08:01:00Z",
					withModel("claude-sonnet-contract"),
					withTokenUsage(`{"input_tokens":0,"output_tokens":0}`)),
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.childID,
			project:      "alpha",
			machine:      "mac",
			agent:        "claude",
			firstMessage: "Child session spawned from alpha.",
			startedAt:    "2026-01-10T12:02:30Z",
			endedAt:      "2026-01-10T12:03:00Z",
			userMessages: 1,
			parentID:     fixture.alphaID,
			relationship: "subagent",
			messages: []Message{
				contractMessage(fixture.childID, 0, "assistant", "Child subagent result.", "2026-01-10T12:02:30Z"),
			},
		}),
		contractSessionWrite(contractSessionSeed{
			id:           fixture.deletedID,
			project:      "alpha",
			machine:      "mac",
			agent:        "claude",
			firstMessage: "Deleted session should stay hidden.",
			startedAt:    "2026-01-12T15:00:00Z",
			endedAt:      "2026-01-12T15:01:00Z",
			userMessages: 1,
			messages: []Message{
				contractMessage(fixture.deletedID, 0, "user", "Deleted session should stay hidden.", "2026-01-12T15:00:00Z"),
			},
		}),
	}

	result, err := store.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	require.Equal(t, len(writes), result.WrittenSessions)
	require.NoError(t, store.SoftDeleteSession(fixture.deletedID))
	return fixture
}

type contractSessionSeed struct {
	id           string
	project      string
	machine      string
	agent        string
	firstMessage string
	sessionName  string
	startedAt    string
	endedAt      string
	userMessages int
	outputTokens int
	peakTokens   int
	parentID     string
	relationship string
	messages     []Message
	usageEvents  []UsageEvent
	signals      SessionSignalUpdate
	findings     []SecretFinding
}

func contractSessionWrite(seed contractSessionSeed) SessionBatchWrite {
	relationship := seed.relationship
	if relationship == "" {
		relationship = "root"
	}
	session := Session{
		ID:                   seed.id,
		Project:              seed.project,
		Machine:              seed.machine,
		Agent:                seed.agent,
		FirstMessage:         &seed.firstMessage,
		StartedAt:            &seed.startedAt,
		EndedAt:              &seed.endedAt,
		MessageCount:         len(seed.messages),
		UserMessageCount:     seed.userMessages,
		RelationshipType:     relationship,
		TotalOutputTokens:    seed.outputTokens,
		PeakContextTokens:    seed.peakTokens,
		HasTotalOutputTokens: seed.outputTokens > 0,
		HasPeakContextTokens: seed.peakTokens > 0,
	}
	if seed.sessionName != "" {
		session.SessionName = &seed.sessionName
	}
	if seed.parentID != "" {
		session.ParentSessionID = &seed.parentID
	}
	return SessionBatchWrite{
		Session:         session,
		Messages:        seed.messages,
		UsageEvents:     seed.usageEvents,
		Signals:         seed.signals,
		Findings:        seed.findings,
		DataVersion:     1,
		ReplaceMessages: true,
	}
}

type messageOption func(*Message)

func contractMessage(
	sessionID string,
	ordinal int,
	role string,
	content string,
	timestamp string,
	opts ...messageOption,
) Message {
	msg := Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		Timestamp:     timestamp,
		ContentLength: len(content),
	}
	for _, opt := range opts {
		opt(&msg)
	}
	return msg
}

func withModel(model string) messageOption {
	return func(msg *Message) {
		msg.Model = model
	}
}

func withTokenUsage(raw string) messageOption {
	return func(msg *Message) {
		msg.TokenUsage = []byte(raw)
	}
}

func withToolCall(call ToolCall) messageOption {
	return func(msg *Message) {
		msg.HasToolUse = true
		msg.ToolCalls = append(msg.ToolCalls, call)
	}
}

func sessionIDs(sessions []Session) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func sidebarSessionIDs(sessions []SidebarSessionIndexRow) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func messageOrdinals(messages []Message) []int {
	ordinals := make([]int, len(messages))
	for i, msg := range messages {
		ordinals[i] = msg.Ordinal
	}
	return ordinals
}

func searchResultIDs(results []SearchResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.SessionID
	}
	return ids
}

func contentSessionIDs(matches []ContentMatch) []string {
	ids := make([]string, len(matches))
	for i, match := range matches {
		ids[i] = match.SessionID
	}
	return ids
}

func contentLocations(matches []ContentMatch) []string {
	locations := make([]string, len(matches))
	for i, match := range matches {
		locations[i] = match.Location
	}
	return locations
}

func projectNames(projects []ProjectInfo) []string {
	names := make([]string, len(projects))
	for i, project := range projects {
		names[i] = project.Name
	}
	return names
}

func agentNames(agents []AgentInfo) []string {
	names := make([]string, len(agents))
	for i, agent := range agents {
		names[i] = agent.Name
	}
	return names
}

func requireReadOnly(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrReadOnly), "expected ErrReadOnly, got %v", err)
}

func floatPtr(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

func TestStoreContractBackendsAreRegisteredExplicitly(t *testing.T) {
	backends := storeContractBackends()
	require.NotEmpty(t, backends)
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		names = append(names, backend.name)
		assert.NotNil(t, backend.open)
		assert.NotNil(t, backend.seed)
	}
	require.True(t, slices.Contains(names, "sqlite"))
}
