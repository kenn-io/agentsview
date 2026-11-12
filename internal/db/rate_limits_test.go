package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func rateLimitWindow(used float64, minutes int64, resets string) *RateLimitWindow {
	return &RateLimitWindow{UsedPercent: used, WindowMinutes: &minutes, ResetsAt: resets}
}

func TestRateLimitSnapshotsFromParsedKeepsReportedValues(t *testing.T) {
	minutes := int64(300)
	resets := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	got := RateLimitSnapshotsFromParsed("codex", "codex:s1", []parser.ParsedRateLimitSnapshot{{
		ObservedAt: time.Date(2026, 11, 12, 10, 0, 1, 250_000_000, time.FixedZone("x", 3600)),
		AccountKey: "acct", LimitID: "codex", PlanType: "plus",
		Primary:   &parser.ParsedRateLimitWindow{UsedPercent: 40.5, WindowMinutes: &minutes, ResetsAt: &resets},
		Secondary: &parser.ParsedRateLimitWindow{UsedPercent: 3},
	}})
	require.Len(t, got, 1)
	assert.Equal(t, RateLimitSnapshot{
		Agent: "codex", AccountKey: "acct", LimitID: "codex", PlanType: "plus",
		SessionID: "codex:s1", ObservedAt: "2026-11-12T09:00:01.250Z",
		Primary:   rateLimitWindow(40.5, 300, "2026-11-12T15:00:00.000Z"),
		Secondary: &RateLimitWindow{UsedPercent: 3},
	}, got[0])
}

func TestLatestRateLimitsPerAccountAndLimit(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	weekly := func(used float64) *RateLimitWindow {
		return rateLimitWindow(used, 10080, "2026-11-16T06:00:00.000Z")
	}
	snaps := []RateLimitSnapshot{
		{Agent: "codex", AccountKey: "a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s1", ObservedAt: "2026-11-10T09:00:00.000Z", Primary: weekly(10)},
		{Agent: "codex", AccountKey: "a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s2", ObservedAt: "2026-11-11T09:00:00.000Z", Primary: weekly(12)},
		// An older report from another session must not win over a newer one.
		{Agent: "codex", AccountKey: "a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s1", ObservedAt: "2026-11-10T12:00:00.000Z", Primary: weekly(11)},
		{Agent: "codex", AccountKey: "b", LimitID: "codex", PlanType: "plus", SessionID: "codex:s3", ObservedAt: "2026-11-09T09:00:00.000Z",
			Primary: rateLimitWindow(80, 300, "2026-11-09T12:00:00.000Z"), Secondary: weekly(30)},
	}
	require.NoError(t, d.InsertRateLimitSnapshots(ctx, snaps))
	// Reparsing the same sources adds nothing.
	require.NoError(t, d.InsertRateLimitSnapshots(ctx, snaps))

	all, err := d.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	assert.Len(t, all, 4)

	latest, err := d.GetLatestRateLimits(ctx)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	assert.Equal(t, "a", latest[0].AccountKey, "newest report first")
	assert.Equal(t, "codex:s2", latest[0].SessionID)
	assert.Equal(t, weekly(12), latest[0].Primary)
	assert.Nil(t, latest[0].Secondary)
	assert.Equal(t, "b", latest[1].AccountKey)
	assert.Equal(t, rateLimitWindow(80, 300, "2026-11-09T12:00:00.000Z"), latest[1].Primary)
	assert.Equal(t, weekly(30), latest[1].Secondary)
}

func TestLatestRateLimitsEmpty(t *testing.T) {
	d := testDB(t)
	latest, err := d.GetLatestRateLimits(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, latest, "the API returns an empty list, not null")
	assert.Empty(t, latest)
}

func TestCopySessionMetadataFrom_PreservesRateLimitSnapshots(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	gone := RateLimitSnapshot{Agent: "codex", LimitID: "codex", SessionID: "codex:deleted-rollout",
		ObservedAt: "2026-10-01T00:00:00.000Z", Primary: rateLimitWindow(50, 10080, "")}
	kept := RateLimitSnapshot{Agent: "codex", LimitID: "codex", SessionID: "codex:live",
		ObservedAt: "2026-11-01T00:00:00.000Z", Primary: rateLimitWindow(5, 10080, "")}
	require.NoError(t, srcDB.InsertRateLimitSnapshots(ctx, []RateLimitSnapshot{gone, kept}))
	require.NoError(t, srcDB.CloseConnections(ctx))
	defer srcDB.Close()

	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()
	// The fresh sync already re-read the rollout that is still on disk.
	require.NoError(t, dstDB.InsertRateLimitSnapshots(ctx, []RateLimitSnapshot{kept}))

	require.NoError(t, dstDB.CopySessionMetadataFrom(ctx, srcPath))

	got, err := dstDB.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	require.Len(t, got, 2, "the old report survives and the re-read one is not duplicated")
	sessions := []string{got[0].SessionID, got[1].SessionID}
	assert.ElementsMatch(t, []string{"codex:live", "codex:deleted-rollout"}, sessions)
}
