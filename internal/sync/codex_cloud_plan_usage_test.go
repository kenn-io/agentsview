package sync_test

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// A codex-cloud mirror file's plan usage and capture coverage reach the
// session's plan usage row through each write path: a sync, a single
// session refresh, and the bulk writes of a full resync.
func TestCodexCloudPlanUsageReachesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "account-key", "thread-a.jsonl.zstd")
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	record := func(kind mirror.Kind, data string) mirror.Record {
		return mirror.Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
	}
	_, err := mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindThread, `{"id":"thread-a","createdAt":1759320000,"updatedAt":1759323600,"cwd":"/workspace/project-a"}`),
		record(mirror.KindTurn, `{"id":"turn-1","status":"completed","startedAt":1759320000}`),
		record(mirror.KindTurn, `{"id":"turn-2","status":"completed","startedAt":1759320600}`),
		record(mirror.KindTokenUsage, `{"join_id":"join-a","turn_id":"turn-1",`+
			`"total":{"input":1000,"output":100,"total":1100},"last":{"input":1000,"output":100,"total":1100},`+
			`"observed_at":"2026-10-01T12:05:00Z"}`),
		record(mirror.KindTurnCompleted, `{"join_id":"join-a","turn_id":"turn-1","observed_at":"2026-10-01T12:06:00Z"}`),
		record(mirror.KindPlanUsageRow, `{"row":{"thread_id":"thread-a","data_status":"available","usage_source":"wham",`+
			`"weekly_limit_percent":2.25,"five_hour_limit_percent":null,"balance_usage_credits":"0","groups":[]},`+
			`"data_as_of":"2026-10-02T09:30:00Z"}`),
	})
	require.NoError(t, err)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(ctx, database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(ctx, nil)

	got, err := database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, &db.SessionPlanUsage{
		HasPlanUsage:  true,
		DataAsOf:      "2026-10-02T09:30:00Z",
		DataStatus:    "available",
		UsageSource:   "wham",
		WeeklyPercent: new(2.25),
		Credits:       new("0"),
		HasCoverage:   true,
		LastCaptureAt: "2026-10-01T12:05:00Z",
		// turn-2 finished with no token record after it.
		UncapturedTurns: 1,
	}, got)

	_, err = mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindPlanUsageOutcome, `{"kind":"counted_under","at":"2026-10-03T08:00:00Z","root":"thread-root"}`),
	})
	require.NoError(t, err)
	require.NoError(t, engine.SyncSingleSession("codex-cloud:thread-a"))

	descendant := &db.SessionPlanUsage{
		HasPlanUsage:    true,
		OutcomeKind:     "counted_under",
		OutcomeAt:       "2026-10-03T08:00:00Z",
		CountedUnder:    "codex-cloud:thread-root",
		HasCoverage:     true,
		LastCaptureAt:   "2026-10-01T12:05:00Z",
		UncapturedTurns: 1,
	}
	got, err = database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, descendant, got)

	// The resync builds a fresh archive, so the row it holds afterwards
	// came from the bulk write of the re-parsed file.
	stats := engine.ResyncAll(ctx, nil)
	require.False(t, stats.Aborted, "resync aborted: %v", stats.Warnings)
	got, err = database.GetSessionPlanUsage(ctx, "codex-cloud:thread-a")
	require.NoError(t, err)
	assert.Equal(t, descendant, got)
}
