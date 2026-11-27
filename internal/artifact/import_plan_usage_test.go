package artifact

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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

// A computer that imports another computer's Codex cloud chats through the
// artifact folder reports them under the same account key as the computer
// that fetched them, so a consumer that keeps one copy per origin counts
// each chat once. Project keys for label-only projects are salted per
// archive, so the two parts' digests can differ.
func TestArtifactImportReportsCodexCloudChatUnderAccount(t *testing.T) {
	const origin = "codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	source := testExportDB(t)
	require.NoError(t, source.ConfigureArtifactLocalMachine(t.Context(), contractOrigin))
	seedSession(t, source, "task", "codex_cloud", func(s *db.Session) {
		s.Agent = "codex-cloud"
		s.FilePath = new("/data/sources/codex-cloud/0f1e2d3c4b5a69788796a5b4c3d2e1f0/task.jsonl.zstd")
		s.StartedAt = new("2026-06-14T01:00:00Z")
		s.EndedAt = new("2026-06-14T01:03:00Z")
	})
	require.NoError(t, source.ReplaceSessionMessages(t.Context(), "task", []db.Message{
		{SessionID: "task", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2026-06-14T01:00:00Z"},
		{SessionID: "task", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5, Timestamp: "2026-06-14T01:03:00Z", Model: "gpt-5-codex"},
	}))
	store := newTestArtifactStore(t)
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: contractOrigin, Full: true})
	require.NoError(t, err)

	destination := testDB(t)
	require.NoError(t, destination.ConfigureArtifactLocalMachine(t.Context(), importLocalOrigin))
	coordinator := NewStoreImportCoordinator(destination, store, importLocalOrigin)
	recordAllImportEntries(t, coordinator, store, contractOrigin)
	_, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)

	for _, archive := range []*db.DB{source, destination} {
		day, err := archive.ExportReportingDay(t.Context(), db.ReportingExportOptions{
			Date:          time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
			Now:           time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
			SchemaVersion: 7,
			ByMachine:     true,
		})
		require.NoError(t, err)
		hour := day.Hours[1]
		require.Contains(t, hour.Machines, origin)
		assert.InDelta(t, 3, hour.Machines[origin].Activity.Totals.AgentMinutes, 0.001)
		assert.Zero(t, hour.Machines[contractOrigin].Activity.Totals.AgentMinutes)
	}
}

// seedCodexCloudCopy writes a computer's copy of one Codex Cloud chat whose
// last message is at last and that carries the given captured usage events.
// Every copy has the same listing end time, which need not move when a chat
// gains turns.
func seedCodexCloudCopy(t *testing.T, archive *db.DB, last string, usage ...db.UsageEvent) {
	t.Helper()
	seedSession(t, archive, "task", "codex_cloud", func(s *db.Session) {
		s.Agent = "codex-cloud"
		s.FilePath = new("/data/sources/codex-cloud/0f1e2d3c4b5a69788796a5b4c3d2e1f0/task.jsonl.zstd")
		s.StartedAt = new("2026-06-14T01:00:00Z")
		s.EndedAt = new("2026-06-14T01:03:00Z")
	})
	require.NoError(t, archive.ReplaceSessionMessages(t.Context(), "task", []db.Message{
		{SessionID: "task", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2026-06-14T01:00:00Z"},
		{SessionID: "task", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5, Timestamp: last, Model: "gpt-5-codex"},
	}))
	if len(usage) > 0 {
		require.NoError(t, archive.ReplaceSessionUsageEvents(t.Context(), "task", usage))
	}
}

// exportCodexCloudCopies imports the source computer's copy into an archive
// that already holds its own copy, and returns that archive's schema 7
// by-machine report for 01:00 UTC.
func exportCodexCloudCopies(t *testing.T, source, destination *db.DB) export.ReportingHour {
	t.Helper()
	store := newTestArtifactStore(t)
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: contractOrigin, Full: true})
	require.NoError(t, err)
	coordinator := NewStoreImportCoordinator(destination, store, importLocalOrigin)
	recordAllImportEntries(t, coordinator, store, contractOrigin)
	_, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)
	day, err := destination.ExportReportingDay(t.Context(), db.ReportingExportOptions{
		Date:          time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC),
		Now:           time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
		SchemaVersion: 7,
		ByMachine:     true,
	})
	require.NoError(t, err)
	return day.Hours[1]
}

func TestArtifactImportCountsCodexCloudChatOnceWithLocalCopy(t *testing.T) {
	const origin = "codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	source := testExportDB(t)
	require.NoError(t, source.ConfigureArtifactLocalMachine(t.Context(), contractOrigin))
	seedCodexCloudCopy(t, source, "2026-06-14T01:03:00Z")
	// The importing computer fetched the same chat before its last turn.
	// Both copies have the same listing end time.
	destination := testDB(t)
	require.NoError(t, destination.ConfigureArtifactLocalMachine(t.Context(), importLocalOrigin))
	seedCodexCloudCopy(t, destination, "2026-06-14T01:02:00Z")

	hour := exportCodexCloudCopies(t, source, destination)
	assert.InDelta(t, 3, hour.Machines[origin].Activity.Totals.AgentMinutes, 0.001)
	assert.Zero(t, hour.Machines[importLocalOrigin].Activity.Totals.AgentMinutes)
	assert.Zero(t, hour.Machines[contractOrigin].Activity.Totals.AgentMinutes)
}

func TestArtifactImportReportsCodexCloudUsageFromCapturingCopy(t *testing.T) {
	const origin = "codex-cloud-account:0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	usage := func(key, at string, input, output int) db.UsageEvent {
		return db.UsageEvent{
			SessionID: "task", Source: "codex-cloud", Model: "gpt-5-codex",
			InputTokens: input, OutputTokens: output, OccurredAt: at, DedupKey: key,
		}
	}
	// The source computer captured one record holding the chat's whole
	// running total. The importing computer captured two records of an
	// earlier, smaller total. Both copies end with the same message, so the
	// importing computer's own copy supplies the activity.
	source := testExportDB(t)
	require.NoError(t, source.ConfigureArtifactLocalMachine(t.Context(), contractOrigin))
	seedCodexCloudCopy(t, source, "2026-06-14T01:03:00Z",
		usage("total", "2026-06-14T01:02:30Z", 150, 30))
	destination := testDB(t)
	require.NoError(t, destination.ConfigureArtifactLocalMachine(t.Context(), importLocalOrigin))
	seedCodexCloudCopy(t, destination, "2026-06-14T01:03:00Z",
		usage("turn-1", "2026-06-14T01:01:00Z", 10, 2),
		usage("turn-2", "2026-06-14T01:02:00Z", 10, 2))

	hour := exportCodexCloudCopies(t, source, destination)
	account := hour.Machines[origin]
	assert.InDelta(t, 3, account.Activity.Totals.AgentMinutes, 0.001)
	assert.Equal(t, int64(150), account.Usage.Totals.InputTokens)
	assert.Equal(t, int64(30), account.Usage.Totals.OutputTokens)
	assert.Equal(t, int64(30), account.Activity.Totals.OutputTokens)
	assert.Zero(t, hour.Machines[importLocalOrigin].Usage.Totals.InputTokens)
	assert.Zero(t, hour.Machines[contractOrigin].Usage.Totals.InputTokens)
}

func TestArtifactImportRejectsInvalidCodexCloudAccount(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		account string
	}{
		{"not a key", "codex-cloud", "../other"},
		{"not a Codex cloud chat", "claude", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := manifest{
				Version: manifestFormatVersion, Origin: contractOrigin, NativeSessionID: "task",
				Session:           manifestSession{ID: "task", Machine: contractOrigin, Agent: tt.agent},
				Segments:          []string{strings.Repeat("a", 64)},
				CodexCloudAccount: tt.account,
			}
			err := validateImportedManifest(m, contractOrigin, contractOrigin+"~task", productionArtifactLimits())
			require.EqualError(t, err, "manifest Codex cloud account is invalid")
		})
	}
}
