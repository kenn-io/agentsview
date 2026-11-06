//go:build fts5

package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageTopSessionsConcurrentRefreshLoadsOnlyReturnedMetadata(t *testing.T) {
	database := testDB(t)
	for i := range 11 {
		id := fmt.Sprintf("session-%02d", i)
		seedJournaledUsage(t, database, id)
		_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET display_name=? WHERE id=?`, fmt.Sprintf("Session %02d", i), id)
		require.NoError(t, err)
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", TopSessionsSort: "tokens"}
	warmJournaledUsage(t, database, filter)
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })

	// Count display records read by the foreground request, including retries.
	database.rawReader().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	var loaded []string
	require.NoError(t, conn.Raw(func(driverConn any) error {
		return driverConn.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("nullif", func(value, other any) any {
			if name, ok := value.(string); ok && strings.HasPrefix(name, "Session ") {
				loaded = append(loaded, name)
			}
			if value == other {
				return nil
			}
			return value
		}, true)
	}))
	require.NoError(t, conn.Close())
	refreshed := false
	filter.Progress = func(phase string) {
		if phase != "Reading cached daily totals" || refreshed {
			return
		}
		refreshed = true
		// Sync appends usage after Top sessions prepares. A peer Usage request
		// refreshes the shared cache before Top sessions reads its snapshot.
		require.NoError(t, other.InsertMessages(t.Context(), []Message{{SessionID: "session-10", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":1}`)}}))
		_, err := other.GetDailyUsage(t.Context(), UsageFilter{Timezone: "UTC", SkipSessionCounts: true})
		require.NoError(t, err)
	}
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.True(t, refreshed)
	require.Len(t, got, 1)
	assert.Equal(t, "session-10", got[0].SessionID)
	assert.Equal(t, 101, got[0].InputTokens)
	assert.Equal(t, []string{"Session 10"}, loaded)
}

func TestUsageTopSessionsLoadsOnlyReturnedMetadata(t *testing.T) {
	database := testDB(t)
	for i := range 128 {
		id := fmt.Sprintf("owner-%03d", i)
		seedJournaledUsage(t, database, id)
		groupKey := ""
		if i >= 126 {
			groupKey = "daily:job"
		}
		_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET display_name=?, session_name=?, group_key=? WHERE id=?`, fmt.Sprintf("Owner %03d", i), fmt.Sprintf("Report %03d · run", i), groupKey, id)
		require.NoError(t, err)
		cost := money.Money{Microdollars: int64(i+1) * 1000}
		require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), id, []UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: CopilotReportedCostSource, OccurredAt: "2026-08-10T23:30:00Z", DedupKey: id}}))
	}
	seedJournaledUsage(t, database, "ordinary")
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)

	// Observe display-name evaluations at the archive query boundary, preserving
	// NULLIF semantics. Groups use installed labels and load no names.
	database.rawReader().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	var loaded []string
	require.NoError(t, conn.Raw(func(driverConn any) error {
		return driverConn.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("nullif", func(value, other any) any {
			if name, ok := value.(string); ok && strings.HasPrefix(name, "Owner ") {
				loaded = append(loaded, name)
			}
			if value == other {
				return nil
			}
			return value
		}, false)
	}))
	require.NoError(t, conn.Close())
	for _, tc := range []struct {
		name    string
		grouped bool
		limit   int
		loaded  []string
	}{
		{"individual", false, 1, []string{"Owner 127"}},
		{"group", true, 1, nil},
		{"group and individual", true, 2, []string{"Owner 125"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded = nil
			filter.TopSessionsByGroup = tc.grouped
			got, err := database.GetTopSessionsByCost(t.Context(), filter, tc.limit)
			require.NoError(t, err)
			assert.Equal(t, tc.loaded, loaded)
			if tc.grouped {
				require.Len(t, got, tc.limit+1)
				assert.Equal(t, "Report 127", got[0].GroupLabel)
				assert.Equal(t, "Report 127", got[0].DisplayName)
				assert.Equal(t, int64(255000), got[0].Cost.Microdollars)
				if tc.limit == 2 {
					assert.Equal(t, "owner-125", got[1].SessionID)
					assert.Equal(t, "Owner 125", got[1].DisplayName)
					assert.Equal(t, "Report 125 · run", got[1].SessionName)
					assert.Empty(t, got[1].GroupLabel)
				}
			} else {
				require.Len(t, got, 1)
				assert.Equal(t, "owner-127", got[0].SessionID)
				assert.Equal(t, "Owner 127", got[0].DisplayName)
				assert.Equal(t, "Report 127 · run", got[0].SessionName)
				assert.Empty(t, got[0].GroupLabel)
			}
		})
	}
}

// CheckUsageTopSessionsMatchLegacy checks cached reads against the archive for the external fixture test.
func CheckUsageTopSessionsMatchLegacy(t *testing.T, database *DB) {
	t.Helper()
	// Owners share units with ordinary runs. Labels come from the latest named
	// member, including members below the SQL cut before the owner joins.
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='cron:'||agent, session_name='Daily report · run' WHERE id LIKE 'journal-%' AND id NOT IN ('journal-00000','journal-00020')`)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='owner', machine='machine-0', agent='claude', session_name='Owner report · run' WHERE id IN ('duplicate-a','journal-00000','journal-00020')`)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='reported', agent='copilot', machine='machine-0' WHERE id IN ('reported','journal-00004')`)
	require.NoError(t, err)
	for _, timezone := range []string{"UTC", "America/New_York"} {
		filter := UsageFilter{From: "2026-08-03", To: "2026-08-10", Timezone: timezone}
		warmJournaledUsage(t, database, filter)
		for _, tc := range usageDayTotalsFilterCases() {
			if tc.name == "breakdowns" || tc.name == "skip counts" || timezone != "UTC" && tc.name != "unfiltered" {
				continue
			}
			f := filter
			tc.change(&f)
			t.Run(timezone+"/"+tc.name, func(t *testing.T) {
				if f.ActiveSince == "" && f.Termination == "" {
					prepared, err := database.prepareUsageWindow(t.Context(), f, usageQueryKindTopSessions, false, true, nil)
					require.NoError(t, err)
					require.True(t, prepared.journaled)
					assert.Empty(t, prepared.snapshot.Sessions)
					assert.Empty(t, prepared.snapshot.Versions)
				}
				for _, sort := range []string{"cost", "tokens", "input+output"} {
					if tc.name != "unfiltered" && sort != "cost" {
						continue
					}
					for _, limit := range []int{1, 3, 100} {
						if tc.name != "unfiltered" && limit != 3 || timezone != "UTC" && limit != 3 {
							continue
						}
						for _, grouped := range []bool{false, true} {
							if f.ActiveSince != "" && grouped {
								continue
							}
							t.Run(fmt.Sprintf("%s/%d/group=%t", sort, limit, grouped), func(t *testing.T) {
								f := f
								f.TopSessionsSort = sort
								if sort == "input+output" {
									f.TopSessionsSort = "tokens"
									f.TopSessionsTokenTypes = UsageTokenTypeInput | UsageTokenTypeOutput
								}
								f.TopSessionsByGroup = grouped
								want, err := database.getTopSessionsByCostLegacy(t.Context(), f, limit)
								require.NoError(t, err)
								if grouped {
									all, err := database.getTopSessionsByCostLegacy(t.Context(), f, 100)
									require.NoError(t, err)
									want, err = GroupTopSessions(all, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
									require.NoError(t, err)
								}
								got, err := database.GetTopSessionsByCost(t.Context(), f, limit)
								require.NoError(t, err)
								assert.Equal(t, want, got)
							})
						}
					}
				}
			})
		}
	}
}

// CheckUsageTopSessionsBoundedGroupMembers checks bounded candidates for the external fixture test.
func CheckUsageTopSessionsBoundedGroupMembers(t *testing.T, database *DB, filter UsageFilter) {
	t.Helper()
	database.StopUsageCacheBackfill()
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET project='project-0', machine='machine-0', group_key='daily:job', started_at='invalid', session_name=''`)
	require.NoError(t, err)
	for _, row := range []struct{ id, started, name string }{
		{"journal-00001", "2026-08-08T13:00:00.000000009+01:00", "Earlier report · run"},
		{"journal-00002", "2026-08-08T12:00:00.000000010Z", "Tied report · run"},
		{"journal-00003", "2026-08-08T08:00:00.000000010-04:00", "Latest report · run"},
		{"journal-00010", "2026-08-09T12:00:00Z", "daily · run"},
	} {
		_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET started_at=?, session_name=? WHERE id=?`, row.started, row.name, row.id)
		require.NoError(t, err)
	}
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='journal-00010'`)
	require.NoError(t, err)
	seedJournaledUsage(t, database, "individual")
	filter.TopSessionsByGroup = true
	warmJournaledUsage(t, database, filter)
	_, facts, _, err := database.queryUsageRollups(t.Context(), filter, usageQueryKindTopSessions, false, 1)
	require.NoError(t, err)
	require.NotNil(t, facts.TopSessions)
	assert.Len(t, facts.TopSessions, 2, "one group and its remainder")
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Empty(t, got[0].SessionID)
	assert.Equal(t, "claude", got[0].Agent, "the newest member's agent")
	assert.Equal(t, "2026-08-09T12:00:00Z", got[0].StartedAt)
	assert.Equal(t, "Latest report", got[0].GroupLabel)
	assert.Equal(t, "Latest report", got[0].DisplayName)
	assert.Equal(t, 360000, got[0].InputTokens)
	assert.Equal(t, 72000, got[0].OutputTokens)
	assert.Equal(t, 432000, got[0].TotalTokens)
	assert.Equal(t, int64(504000), got[0].Cost.Microdollars)

	assert.Equal(t, 100, got[1].InputTokens)
	assert.Equal(t, 20, got[1].OutputTokens)
	assert.Equal(t, int64(140), got[1].Cost.Microdollars)
	assert.Empty(t, got[1].SessionID)

	// A name-only edit must select the next currently named member.
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET session_name='daily · run' WHERE id='journal-00003'`)
	require.NoError(t, err)
	got, err = database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "Tied report", got[0].GroupLabel)
	assert.Equal(t, int64(504000), got[0].Cost.Microdollars)
}

func TestUsageTopSessionsPurgedBeforeHydration(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "purged")
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	_, facts, _, err := database.queryUsageRollups(t.Context(), filter, usageQueryKindTopSessions, false, 1)
	require.NoError(t, err)
	require.NoError(t, database.DeleteSession(t.Context(), "purged"))
	got, err := database.finishUsageTopSessions(t.Context(), facts)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "purged", got[0].DisplayName)
	assert.Equal(t, 120, got[0].TotalTokens)
}

func TestUsageTopSessionsOwnerCrossesCut(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 10_000_000}}}))
	for _, id := range []string{"owner", "ordinary"} {
		seedJournaledUsage(t, database, id)
	}
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='owner'`)
	require.NoError(t, err)
	insertSession(t, database, "duplicate", "project", func(s *Session) { s.Agent = "claude"; s.StartedAt = new("2026-08-10T23:32:00Z") })
	require.NoError(t, database.InsertMessages(t.Context(), []Message{
		{SessionID: "owner", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":500000}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "duplicate", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T23:32:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":500000}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		{SessionID: "owner", Ordinal: 2, Role: "assistant", Timestamp: "2026-08-10T23:32:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":599900}`)},
		{SessionID: "ordinary", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:32:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":999900}`)},
	}))
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	for _, sort := range []string{"cost", "tokens"} {
		filter.TopSessionsSort = sort
		got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "owner", got[0].SessionID)
		assert.Equal(t, int64(11000000), got[0].Cost.Microdollars)
		assert.Equal(t, 1100000, got[0].InputTokens)
	}
}

func TestUsageTopSessionsZeroCostTies(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"z", "a", "m"} {
		seedJournaledUsage(t, database, id)
	}
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	for _, sort := range []string{"cost", "tokens"} {
		filter.TopSessionsSort = sort
		got, err := database.GetTopSessionsByCost(t.Context(), filter, 2)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "a", got[0].SessionID)
		assert.Equal(t, "m", got[1].SessionID)
		assert.Zero(t, got[0].Cost.Microdollars)
	}
}

// CheckUsageSessionCountsJournaled checks that direct count reads override skipped summary counts.
func CheckUsageSessionCountsJournaled(t *testing.T, database *DB) {
	t.Helper()
	filter := UsageFilter{From: "2026-08-03", To: "2026-08-10", Timezone: "UTC", SkipSessionCounts: true}
	warmJournaledUsage(t, database, filter)
	cases := usageDayTotalsFilterCases()
	want := make([]UsageSessionCounts, len(cases))
	for i, tc := range cases {
		f := filter
		tc.change(&f)
		var err error
		want[i], err = database.getUsageSessionCountsLegacy(t.Context(), f)
		require.NoError(t, err)
		if f.ActiveSince != "" {
			// Activity windows capture candidates from the archive.
			got, err := database.GetUsageSessionCounts(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, want[i], got, tc.name)
		}
	}
	// Day-total counts come from the cache, so archive names stay unread.
	type authorizer interface {
		RegisterAuthorizer(func(int, string, string, string) int)
	}
	database.rawReader().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		raw.(authorizer).RegisterAuthorizer(func(op int, table, column, _ string) int {
			if op == sqlite3.SQLITE_READ && table == "sessions" && (column == "display_name" || column == "session_name") {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	t.Cleanup(func() {
		conn, err := database.getReader().Conn(context.WithoutCancel(t.Context()))
		require.NoError(t, err)
		defer conn.Close()
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(authorizer).RegisterAuthorizer(nil)
			return nil
		}))
	})
	for i, tc := range cases {
		f := filter
		tc.change(&f)
		if f.ActiveSince != "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			got, err := database.GetUsageSessionCounts(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, want[i], got)
		})
	}
}

func TestUsageTopSessionsGroupRenameAfterWarmup(t *testing.T) {
	for _, authoritative := range []bool{false, true} {
		t.Run(strconv.FormatBool(authoritative), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "grouped")
			_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='daily:job', session_name='Original report · run' WHERE id='grouped'`)
			require.NoError(t, err)
			if authoritative {
				cost := money.Money{Microdollars: 1000}
				require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "grouped", []UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: CopilotReportedCostSource, OccurredAt: "2026-08-10T23:30:00Z"}}))
			}
			filter := UsageFilter{Timezone: "UTC", TopSessionsByGroup: true}
			warmJournaledUsage(t, database, filter)
			got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "Original report", got[0].GroupLabel)
			for _, tc := range []struct{ name, label, display string }{
				{"Renamed report · run", "Renamed report", "Renamed report"},
				{"daily · run", "", "daily"},
				{"Another report · run", "Another report", "Another report"},
			} {
				require.NoError(t, database.RefreshSessionName(t.Context(), "grouped", &tc.name))
				got, err = database.GetTopSessionsByCost(t.Context(), filter, 1)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, tc.label, got[0].GroupLabel)
				assert.Equal(t, tc.display, got[0].DisplayName)
			}

			// An older writer's open drops and recreates only the triggers it names.
			const name = "trg_sessions_usage_change_group_label"
			drops := strings.Replace(usageChangeJournalTriggerDropsSQL, "DROP TRIGGER IF EXISTS "+name+";\n", "", 1)
			creates := usageChangeJournalTriggerSQL
			start := strings.Index(creates, "CREATE TRIGGER "+name+" ")
			require.GreaterOrEqual(t, start, 0)
			end := start + strings.Index(creates[start:], "END;\n") + len("END;\n")
			_, err = database.getWriter().ExecContext(t.Context(), drops+creates[:start]+creates[end:])
			require.NoError(t, err)
			renamed := "Older writer report · run"
			require.NoError(t, database.RefreshSessionName(t.Context(), "grouped", &renamed))
			got, err = database.GetTopSessionsByCost(t.Context(), filter, 1)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "Older writer report", got[0].GroupLabel)
		})
	}
}
