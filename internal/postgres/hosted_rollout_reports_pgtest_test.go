//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"hegel.dev/go/hegel"
)

type rolloutVectorHits []db.VectorHit

func (hits *rolloutVectorHits) PrepareSemanticSearch(context.Context, string) (db.VectorSearchPage, int, error) {
	return func(context.Context, int) ([]db.VectorHit, bool, error) { return *hits, true, nil }, 0, nil
}
func (*rolloutVectorHits) ResolveMessageUnits(_ context.Context, refs []db.MessageRef) ([]db.UnitRef, error) {
	return make([]db.UnitRef, len(refs)), nil
}

// Additional inventory: RecentEdits, findings/source, signals, trends and
// activity artifacts: TestHostedRolloutRecentEdits/ReportEvidence; version,
// watch and provider resume: TestHostedRolloutDirectIdentityReads; archive
// candidates and project metadata: TestHostedRolloutProjectMetadataAndCandidates.
// GetChildSessions and nested raw links: TestHostedCrossSourceTargetLifecycleAndReadRevision;
// legacy nested timing/sidebar roots: TestHostedRolloutLegacyTimingHidesNestedDuration.
// Pin creation/listing/physical references: this inventory plus the complete
// eligibility model below. UnpinMessage: TestHostedPinInventorySkipsFalseHistoricalPayloads
// and TestHostedLegacyWriteFencesFirstRawPublication; RemovePinReference: TestHostedPublicConflictCurationAndCursor.
// Identity/curation/purge: hosted_rollout_protection tests.
//
// Invoke public readers with their normal arguments. Comparing with an empty
// corpus catches leaks in totals and nested data as well as returned row IDs.
func TestHostedRolloutReadInventory(t *testing.T) {
	f := newProjectionFixture(t)
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	var hits rolloutVectorHits
	h.SetVectorSearcher(&hits)
	af := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	uf := db.UsageFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	insight, err := h.InsertInsight(t.Context(), db.Insight{Content: "historical summary", CacheKey: "saved"})
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	cases := []struct {
		name     string
		args     []any
		positive bool
	}{
		{"ListSessions", []any{db.SessionFilter{Limit: 100}}, true},
		{"GetSidebarSessionIndex", []any{db.SessionFilter{Limit: 100}}, true},
		{"FindSessionIDsByPartial", []any{"portable", 10}, true},
		{"FindSessionIDsByRawSuffix", []any{"portable", 10}, true},
		{"GetStats", []any{false, false}, true},
		{"ListStarredSessionIDs", nil, true},
		{"GetProjects", []any{false, false}, true},
		{"GetActiveProjectLabels", nil, true},
		{"GetAgents", []any{false, false}, true},
		{"GetMachines", []any{false, false}, true},
		{"GetBranches", []any{false, false}, true},
		{"GetMatchingMachineIDs", []any{[]string{"hosted"}}, true},
		{"GetProjectInventory", []any{db.ProjectDateFilter{}}, true},
		{"ListProjectRules", []any{""}, true},
		{"ActivityReportSourceProbe", nil, true},
		{"Search", []any{db.SearchFilter{Query: "needle"}}, true},
		{"SearchContent", []any{db.ContentSearchFilter{Pattern: "needle", IncludeOneShot: true}}, true},
		{"SearchContent", []any{db.ContentSearchFilter{Pattern: "needle", Mode: "semantic", Limit: 10, IncludeOneShot: true}}, true},
		{"SearchContent", []any{db.ContentSearchFilter{Pattern: "needle", Mode: "hybrid", Limit: 10, IncludeOneShot: true}}, true},
		{"GetAnalyticsSummary", []any{af}, true},
		{"GetAnalyticsActivity", []any{af, "day"}, true},
		{"GetAnalyticsHeatmap", []any{af, "sessions"}, true},
		{"GetAnalyticsProjects", []any{af}, true},
		{"GetAnalyticsHourOfWeek", []any{af}, true},
		{"GetAnalyticsSessionShape", []any{af}, true},
		{"GetAnalyticsTools", []any{af}, true},
		{"GetAnalyticsSkills", []any{af, "day"}, true},
		{"GetAnalyticsVelocity", []any{af}, true},
		{"GetAnalyticsTopSessions", []any{af, "messages"}, true},
		{"GetDailyUsage", []any{uf}, true},
		{"GetUsageSessionCounts", []any{uf}, true},
		{"GetUsageMatchingSessionCount", []any{uf}, true},
		{"GetPlanUsageSessionCounts", []any{uf}, true},
		{"GetTopSessionsByCost", []any{uf, 10}, true},
		{"GetActivityReport", []any{af, q}, true},
		{"GetSessionUsageRows", []any{[]string{"codex:portable"}}, true},
		{"ListPinnedMessages", []any{"", ""}, true},
		{"ListPinnedMessages", []any{"codex:portable", ""}, true},
		{"GetSession", []any{"codex:portable"}, true},
		{"GetSessionFull", []any{"codex:portable"}, true},
		{"GetMessages", []any{"codex:portable", 0, 10, true}, true},
		{"GetMessagesWindow", []any{"codex:portable", db.MessageWindow{}}, true},
		{"GetAllMessages", []any{"codex:portable"}, true},
		{"SearchSession", []any{"codex:portable", "needle"}, true},
		{"GetSessionActivity", []any{"codex:portable"}, true},
		{"GetSessionTiming", []any{"codex:portable"}, true},
		{"GetSessionUsage", []any{"codex:portable", true}, true},
		{"GetSessionPlanUsage", []any{"codex:portable"}, true},
		{"GetResumeModelCounts", []any{"codex:portable"}, true},
		{"ListInsights", []any{db.InsightFilter{}}, false},
		{"GetInsight", []any{insight}, false},
		{"GetCachedInsight", []any{"saved"}, false},
	}
	read := func(tc struct {
		name     string
		args     []any
		positive bool
	}) string {
		args := []reflect.Value{reflect.ValueOf(t.Context())}
		for _, arg := range tc.args {
			args = append(args, reflect.ValueOf(arg))
		}
		out := reflect.ValueOf(h).MethodByName(tc.name).Call(args)
		require.True(t, out[len(out)-1].IsNil(), "%s: %v", tc.name, out[len(out)-1])
		value := out[0].Interface()
		if probe, ok := value.(activity.SourceProbe); ok {
			probe.HostedIdentityRevision, probe.HostedSelectionRevision, probe.HostedCorpusRevision = 0, 0, 0
			value = probe
		}
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return string(data)
	}
	empty := make([]string, len(cases))
	for i, tc := range cases {
		empty[i] = read(tc)
	}
	m, _ := f.accept(t, "device-a", "inventory", "")
	outcome := projectionOutcome("needle")
	outcome.Outcome.Results[0].Result.Session.GitBranch = "main"
	outcome.Outcome.Results[0].Result.Messages[1].Model = "synthetic-model"
	outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].SkillName = "synthetic-skill"
	for i := range outcome.Outcome.Results[0].Result.Messages {
		outcome.Outcome.Results[0].Result.Messages[i].Timestamp = time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
	}
	outcome.Outcome.Results[0].Result.UsageEvents[0].OccurredAt = "2026-01-01T00:01:00Z"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	var physical string
	require.NoError(t, f.runtime.QueryRow(`SELECT id FROM sessions`).Scan(&physical))
	hits = rolloutVectorHits{{SessionID: physical, Score: 1}}
	_, err = f.runtime.Exec(`INSERT INTO session_plan_usage(session_id,has_plan_usage,uncaptured_turns) SELECT id,true,1 FROM sessions`)
	require.NoError(t, err)
	_, err = h.PinMessage(t.Context(), "codex:portable", 0, nil)
	require.NoError(t, err)
	_, err = h.StarSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	visible := make([]string, len(cases))
	for i, tc := range cases {
		visible[i] = read(tc)
		if tc.positive {
			assert.NotEqual(t, empty[i], visible[i], "positive control: %s", tc.name)
		}
	}
	gate, err := beginRawRollout(t.Context(), f.admin, f.schema, f.tenant, true)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.Exec(`LOCK source_worktree_project_mappings IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	var report db.ProjectInventory
	done := make(chan error, 1)
	go func() { var e error; report, e = h.GetProjectInventory(t.Context(), db.ProjectDateFilter{}); done <- e }()
	waitHostedLocks(t, f, t.Context(), 1)
	_, err = gate.Exec(`INSERT INTO raw_rollout_scopes(tenant_id,state) VALUES($1,'shadow')`, f.tenant)
	require.NoError(t, err)
	var group string
	require.NoError(t, gate.QueryRow(`SELECT group_id FROM raw_session_groups FOR UPDATE`).Scan(&group))
	require.NoError(t, recomputeRawRollout(t.Context(), gate, group))
	require.NoError(t, publishRawRevision(t.Context(), gate, true, nil))
	require.NoError(t, gate.Commit())
	require.NoError(t, <-done)
	assert.Zero(t, report.TotalSessions, "forwarded report must retry its complete snapshot")
	pins, err := h.core.pinReferencePage(t.Context(), physical, "")
	require.NoError(t, err)
	assert.Empty(t, pins, "direct physical pin lookup must enforce rollout eligibility")
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.JSONEq(t, empty[i], read(tc)) })
	}
	t.Run("eligibility_property", func(t *testing.T) {
		requireHegel(t)
		hegel.Test(t, func(ht *hegel.T) {
			i := hegel.Draw(ht, hegel.Integers(0, len(cases)-1))
			hidden := hegel.Draw(ht, hegel.Booleans())
			_, err := f.runtime.Exec(`UPDATE sessions SET rollout_hidden=$1`, hidden)
			require.NoError(ht, err)
			got := read(cases[i])
			want := empty[i]
			if !hidden {
				want = visible[i]
			}
			if hidden || !cases[i].positive {
				require.JSONEq(ht, want, got, cases[i].name)
			} else {
				require.NotEqual(ht, empty[i], got, cases[i].name)
			}
		}, hegel.WithTestCases(1000))
	})
}

// Findings, signals, trends and activity must retain a visible peer while
// hidden-only filters return no evidence or aggregate contribution.
func TestHostedRolloutReportEvidence(t *testing.T) {
	f := newHostedFixture(t, "tenant-evidence")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	for _, id := range []string{"visible", "shadow"} {
		_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,started_at,ended_at,message_count,user_message_count,health_score,health_grade,tool_failure_signal_count)
			VALUES($1,$1,'m','claude','2026-01-01T00:00:00Z','2026-01-01T00:01:00Z',2,2,90,'A',3)`, id)
		require.NoError(t, err)
		_, err = f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content,content_length,timestamp) VALUES
			($1,0,'user','needle evidence',15,'2026-01-01T00:00:00Z'),($1,1,'assistant','needle complete',15,'2026-01-01T00:01:00Z')`, id)
		require.NoError(t, err)
		_, err = f.runtime.Exec(`INSERT INTO secret_findings(session_id,rule_name,confidence,location_kind,message_ordinal,match_start,match_end,match_index,redacted_match,rules_version)
			VALUES($1,'synthetic','high','message',0,0,6,0,'redacted','test-rules')`, id)
		require.NoError(t, err)
	}
	af := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-02", Timezone: "UTC"}
	terms, err := db.ParseTrendTerms([]string{"needle"})
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	check := func(project string, ids []string) {
		t.Helper()
		filter := af
		filter.Project = project
		findings, err := h.ListSecretFindings(t.Context(), db.SecretFindingFilter{Project: project})
		require.NoError(t, err)
		var found []string
		for _, finding := range findings.Findings {
			found = append(found, finding.SessionID)
			assert.Equal(t, 0, finding.MessageOrdinal)
			assert.Equal(t, 6, finding.MatchEnd)
			text, ok, err := h.SecretFindingSource(t.Context(), finding.SecretFinding)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, "needle evidence", text)
		}
		assert.ElementsMatch(t, ids, found)
		assert.Zero(t, findings.NextCursor)
		signals, err := h.GetAnalyticsSignals(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, len(ids), signals.ScoredSessions)
		assert.Equal(t, 3*len(ids), signals.ToolHealth.TotalFailureSignals)
		examples, err := h.GetAnalyticsSignalSessions(t.Context(), filter, "tool_failure_signals", 10)
		require.NoError(t, err)
		found = nil
		for _, example := range examples.Sessions {
			found = append(found, example.SessionID)
			assert.Equal(t, 3, example.SignalTotal)
		}
		assert.ElementsMatch(t, ids, found)
		trends, err := h.GetTrendsTerms(t.Context(), filter, terms, "day")
		require.NoError(t, err)
		assert.Equal(t, 2*len(ids), trends.MessageCount)
		require.Len(t, trends.Series, 1)
		assert.Equal(t, 2*len(ids), trends.Series[0].Total)
		artifacts, err := h.BuildActivityReportArtifacts(t.Context(), filter, q, nil)
		require.NoError(t, err)
		assert.Equal(t, len(ids), artifacts.Report.Totals.Sessions)
		assert.Empty(t, artifacts.Report.BySession, "artifact reports keep drill-down records separately")
		assert.Zero(t, artifacts.Report.SessionsTotal)
		assert.Len(t, artifacts.Report.Projects, len(ids))
		report, err := h.GetActivityReport(t.Context(), filter, q)
		require.NoError(t, err)
		assert.Equal(t, len(ids), report.SessionsTotal)
		for _, rows := range [][]activity.SessionRow{report.BySession, artifacts.Sessions} {
			found = nil
			for _, row := range rows {
				found = append(found, row.SessionID)
			}
			assert.ElementsMatch(t, ids, found)
		}
		found = nil
		for id, membership := range artifacts.Membership {
			found = append(found, id)
			assert.NotEmpty(t, membership)
		}
		assert.ElementsMatch(t, ids, found)
	}
	check("", []string{"visible", "shadow"})
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=true WHERE id='shadow'`)
	require.NoError(t, err)
	check("", []string{"visible"})
	check("shadow", nil)
	text, ok, err := h.SecretFindingSource(t.Context(), db.SecretFinding{SessionID: "shadow", LocationKind: "message", MessageOrdinal: 0})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, text)
}

// Archive metadata and saved summaries are independent records. Session-derived
// candidates, examples and inventory counts still use only eligible sessions.
func TestHostedRolloutProjectMetadataAndCandidates(t *testing.T) {
	f := newHostedFixture(t, "tenant-projects")
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO source_archives(source_archive_id,source_archive_salt) VALUES('archive','synthetic-salt');
 INSERT INTO source_project_identity_observations(source_archive_id,source_archive_salt,project,machine,root_path,repository_path,observed_at) VALUES
 ('archive','synthetic-salt','p','m','/workspace/p','/workspace/p','2026-01-01'),('archive','synthetic-salt','hidden-only','m','/workspace/hidden','/workspace/hidden','2026-01-01');
 INSERT INTO sessions(id,source_archive_id,project,machine,agent,cwd,message_count,user_message_count) VALUES
 ('visible','archive','p','m','claude','/workspace/p',1,2),('shadow','archive','p','m','claude','/workspace/p',1,2),
 ('hidden-only','archive','hidden-only','m','claude','/workspace/hidden',1,2);
 INSERT INTO sync_metadata(key,value) VALUES('machine_label:m','Synthetic device'),('machine_alias:old','m');
 INSERT INTO source_worktree_project_mappings(source_archive_id,machine,path_prefix,layout,project,original_project,enabled,updated_at)
 VALUES('archive','configured','/workspace/configured','explicit','p','p',true,'2026-01-01')`)
	require.NoError(t, err)
	insight, err := h.InsertInsight(t.Context(), db.Insight{Content: "saved summary", CacheKey: "saved-project"})
	require.NoError(t, err)
	observations, err := h.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, observations, 2)
	assert.Equal(t, []string{"hidden-only", "p"}, []string{observations[0].Project, observations[1].Project})
	assert.Equal(t, []string{"/workspace/hidden", "/workspace/p"}, []string{observations[0].RootPath, observations[1].RootPath})
	projects, err := h.BuildProjectIdentityMap(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, projects, 2)
	require.NotEmpty(t, projects["p"].ProjectKey)
	require.NotNil(t, projects["p"].Identity)
	request := db.ArchiveWorktreeCandidateRequest{ProjectLabel: "p", ProjectKey: projects["p"].ProjectKey}
	check := func(wantIDs []string) {
		t.Helper()
		candidates, err := h.ListArchiveWorktreeCandidates(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, candidates, 1)
		assert.Equal(t, len(wantIDs), candidates[0].ContributingSessions)
		assert.Equal(t, 1, candidates[0].DistinctCwds)
		assert.Equal(t, "/workspace/p", candidates[0].EvidenceRoot)
		assert.Equal(t, "aggregate", candidates[0].EvidenceKind)
		var ids []string
		for _, example := range candidates[0].Examples {
			ids = append(ids, example.SessionID)
			assert.Equal(t, "/workspace/p", example.Cwd)
		}
		assert.ElementsMatch(t, wantIDs, ids)
	}
	check([]string{"visible", "shadow"})
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=(id<>'visible')`)
	require.NoError(t, err)
	check([]string{"visible"})
	inventory, err := h.GetProjectInventory(t.Context(), db.ProjectDateFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, inventory.TotalProjects)
	assert.Equal(t, 1, inventory.TotalSessions)
	hiddenCandidates, err := h.ListArchiveWorktreeCandidates(t.Context(), db.ArchiveWorktreeCandidateRequest{ProjectLabel: "hidden-only", ProjectKey: projects["hidden-only"].ProjectKey})
	require.NoError(t, err)
	assert.Empty(t, hiddenCandidates)
	storedObservations, err := h.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, observations, storedObservations, "archive metadata is not a session projection")
	storedProjects, err := h.BuildProjectIdentityMap(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, projects, storedProjects)
	selected, err := h.ListProjectIdentityObservations(t.Context(), []string{"p"})
	require.NoError(t, err)
	require.Len(t, selected, 1)
	assert.Equal(t, "p", selected[0].Project)
	selectedMap, err := h.BuildProjectIdentityMap(t.Context(), []string{"p"})
	require.NoError(t, err)
	assert.Equal(t, map[string]export.ProjectMapEntry{"p": projects["p"]}, selectedMap)
	labels, err := h.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"m": "Synthetic device"}, labels)
	aliases, err := h.GetMachineAliases(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old": "m"}, aliases)
	rules, err := h.ListProjectRules(t.Context(), "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"m", "configured"}, rules.Machines)
	saved, err := h.GetInsight(t.Context(), insight)
	require.NoError(t, err)
	require.NotNil(t, saved)
	assert.Equal(t, "saved summary", saved.Content)
	cached, err := h.GetCachedInsight(t.Context(), "saved-project")
	require.NoError(t, err)
	require.NotNil(t, cached)
	assert.Equal(t, "saved summary", cached.Content)
	insights, err := h.ListInsights(t.Context(), db.InsightFilter{})
	require.NoError(t, err)
	require.Len(t, insights, 1)
	assert.Equal(t, "saved summary", insights[0].Content)
}
