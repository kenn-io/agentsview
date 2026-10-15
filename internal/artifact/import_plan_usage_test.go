package artifact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestArtifactImportKeepsCodexCloudPlanUsage(t *testing.T) {
	source := testExportDB(t)
	seedCodexCloud := func(id string) {
		t.Helper()
		seedSession(t, source, id, "codex_cloud", func(s *db.Session) {
			s.Agent = "codex-cloud"
		})
		require.NoError(t, source.ReplaceSessionMessages(t.Context(), id, []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
			{
				SessionID: id, Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5,
				Model: "gpt-5-codex", Timestamp: "2026-06-14T01:02:30Z",
			},
		}))
	}
	seedCodexCloud("root")
	seedCodexCloud("child")
	seedSession(t, source, "plain", "project")

	weekly, fiveHour, credits := 12.5, 40.0, "3.25"
	groupWeekly, groupCredits := 12.5, "3.25"
	require.NoError(t, source.ReplaceSessionUsageEvents(t.Context(), "root", []db.UsageEvent{{
		SessionID: "root", Source: "codex-cloud", Model: "gpt-5-codex",
		InputTokens: 100, OutputTokens: 20,
		OccurredAt: "2026-06-14T01:02:30Z", DedupKey: "turn-1",
	}}))
	require.NoError(t, source.ReplaceSessionPlanUsage(t.Context(), "root", &db.SessionPlanUsage{
		HasPlanUsage: true, DataAsOf: "2026-06-14T01:05:00Z",
		DataStatus: "available", UsageSource: "wham",
		WeeklyPercent: &weekly, FiveHourPercent: &fiveHour, Credits: &credits,
		Groups: []db.SessionPlanUsageGroup{{
			Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard",
			WeeklyPercent: &groupWeekly, Credits: &groupCredits,
		}},
		Final:       true,
		HasCoverage: true, LastCaptureAt: "2026-06-14T01:02:30Z", UncapturedTurns: 2,
	}))
	// The child has no usage events: its usage counts under the root.
	require.NoError(t, source.ReplaceSessionPlanUsage(t.Context(), "child", &db.SessionPlanUsage{
		HasPlanUsage: true, DataStatus: "available", UsageSource: "wham",
		OutcomeKind: "counted_under", OutcomeAt: "2026-06-14T01:06:00Z",
		CountedUnder: "root",
	}))

	store := newTestArtifactStore(t)
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: contractOrigin, Full: true})
	require.NoError(t, err)
	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	plainRef, err := NewRef(contractOrigin, KindManifests, checkpoint.Sessions[contractOrigin+"~plain"]+".json")
	require.NoError(t, err)
	plainManifest := string(readContractArtifact(t, store, plainRef))
	assert.Contains(t, plainManifest, `"v":5}`, "a session without plan usage keeps its old manifest")
	assert.NotContains(t, plainManifest, "plan_usage")
	assert.NotContains(t, plainManifest, "capture_coverage")

	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(destination, store, importLocalOrigin)
	recordAllImportEntries(t, coordinator, store, contractOrigin)
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, result.Sessions)

	root, err := destination.GetSessionPlanUsage(t.Context(), contractOrigin+"~root")
	require.NoError(t, err)
	assert.Equal(t, &db.PlanUsageView{
		DataAsOf: "2026-06-14T01:05:00Z", DataStatus: "available", UsageSource: "wham",
		WeeklyPercent: &weekly, FiveHourPercent: &fiveHour, Credits: &credits,
		Groups: []db.SessionPlanUsageGroup{{
			Model: "gpt-5-codex", ReasoningEffort: "high", Speed: "standard",
			WeeklyPercent: &groupWeekly, Credits: &groupCredits,
		}},
		Final: true,
	}, root.PlanUsageView())
	assert.Equal(t, &db.CaptureCoverageView{
		LastCaptureAt: "2026-06-14T01:02:30Z", UncapturedTurns: 2,
	}, root.CaptureCoverageView())

	child, err := destination.GetSessionPlanUsage(t.Context(), contractOrigin+"~child")
	require.NoError(t, err)
	assert.Equal(t, &db.PlanUsageView{
		DataStatus: "available", UsageSource: "wham",
		Groups:      []db.SessionPlanUsageGroup{},
		OutcomeKind: "counted_under", OutcomeAt: "2026-06-14T01:06:00Z",
		CountedUnder: contractOrigin + "~root",
	}, child.PlanUsageView())
	assert.Nil(t, child.CaptureCoverageView())

	plain, err := destination.GetSessionPlanUsage(t.Context(), contractOrigin+"~plain")
	require.NoError(t, err)
	assert.Nil(t, plain)

	counts, err := destination.GetPlanUsageSessionCounts(t.Context(), db.UsageFilter{})
	require.NoError(t, err)
	assert.Equal(t, db.PlanUsageSessionCounts{
		PlanOnly:        1,
		PlanOnlyByModel: map[string]int{"gpt-5-codex": 1},
		PartlyCaptured:  1,
	}, counts)
}

func TestArtifactExportRepublishesSessionWhenPlanUsageArrives(t *testing.T) {
	source := testExportDB(t)
	seedSession(t, source, "task", "codex_cloud", func(s *db.Session) { s.Agent = "codex-cloud" })
	store := newTestArtifactStore(t)
	manifestFor := func() manifest {
		t.Helper()
		_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: contractOrigin})
		require.NoError(t, err)
		checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
		ref, err := NewRef(contractOrigin, KindManifests, checkpoint.Sessions[contractOrigin+"~task"]+".json")
		require.NoError(t, err)
		m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
		require.NoError(t, err)
		return m
	}
	before := manifestFor()
	assert.Equal(t, 5, before.Version)
	assert.Nil(t, before.CaptureCoverage)

	require.NoError(t, source.ReplaceSessionPlanUsage(t.Context(), "task", &db.SessionPlanUsage{
		HasCoverage: true, LastCaptureAt: "2026-06-14T01:02:30Z", UncapturedTurns: 1,
	}))
	after := manifestFor()
	assert.Equal(t, 7, after.Version)
	assert.Nil(t, after.PlanUsage)
	assert.Equal(t, &artifactCaptureCoverage{
		LastCaptureAt: "2026-06-14T01:02:30Z", UncapturedTurns: 1,
	}, after.CaptureCoverage)
}
