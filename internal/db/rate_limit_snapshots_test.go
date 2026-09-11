package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rlSnap builds a minimal RateLimitSnapshot fixture; callers override
// additional fields on the returned value.
func rlSnap(sessionID, machine, limitID, windowKind, observedAt string, usedPercent float64) RateLimitSnapshot {
	return RateLimitSnapshot{
		SessionID: sessionID, Machine: machine, LimitID: limitID, PlanType: "pro",
		WindowKind: windowKind, WindowMinutes: 10080, UsedPercent: usedPercent, ObservedAt: observedAt,
	}
}

// TestInsertRateLimitSnapshots_NullableResetsAt: an unknown reset time
// round-trips as nil, not flattened to 0.
func TestInsertRateLimitSnapshots_NullableResetsAt(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 95),
	}))
	rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Machine: "laptop"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].ResetsAt, "a null resets_at must not be flattened to 0")
}

// TestLatestRateLimitSnapshots_ObservationResolution pins the
// observation_key invariant: "latest" resolves per whole observation, not
// per window_kind, and a NULL session_id left by a deleted session must
// never merge or split distinct observations.
func TestLatestRateLimitSnapshots_ObservationResolution(t *testing.T) {
	const at = "2026-09-09T10:00:00Z"
	cases := []struct {
		name  string
		build func(t *testing.T, d *DB)
		check func(t *testing.T, rows []RateLimitSnapshot)
	}{
		{"newer observation omitting a window supersedes it", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
				rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 80),
				rlSnap("codex:sess-1", "laptop", "codex", "secondary", "2026-09-09T09:00:00Z", 10),
				rlSnap("codex:sess-1", "laptop", "codex", "primary", at, 95),
			}))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 1)
			assert.Equal(t, "primary", rows[0].WindowKind)
		}},
		{"two sessions observing at the same instant do not merge", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
				rlSnap("codex:sess-1", "laptop", "codex", "secondary", at, 5),
				rlSnap("codex:sess-2", "laptop", "codex", "primary", at, 77),
			}))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 1)
			assert.Equal(t, "codex:sess-2", rows[0].SessionID)
		}},
		{"deleting both colliding sessions keeps observations apart", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
				rlSnap("codex:sess-1", "laptop", "codex", "secondary", at, 5),
				rlSnap("codex:sess-2", "laptop", "codex", "primary", at, 77),
			}))
			require.NoError(t, d.DeleteSession("codex:sess-1"))
			require.NoError(t, d.DeleteSession("codex:sess-2"))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 1)
			assert.Empty(t, rows[0].SessionID)
		}},
		{"deleting one session keeps its sibling windows together", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
				rlSnap("codex:sess-1", "laptop", "codex", "primary", at, 60),
				rlSnap("codex:sess-1", "laptop", "codex", "secondary", at, 15),
				rlSnap("codex:sess-2", "desktop", "codex", "primary", at, 30),
			}))
			require.NoError(t, d.DeleteSession("codex:sess-1"))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 3)
			empty := 0
			for _, r := range rows {
				if r.SessionID == "" {
					empty++
				}
			}
			assert.Equal(t, 2, empty, "both of the deleted session's windows survive")
		}},
		{"a newer observation with empty plan_type keeps the latest non-empty label", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			newer := rlSnap("codex:sess-1", "laptop", "codex", "primary", at, 95)
			newer.PlanType = ""
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 80), newer}))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 1)
			assert.Equal(t, "pro", rows[0].PlanType, "plan_type falls back to the latest non-empty label")
		}},
		{"an authoritative reparse supersedes a fallback parse's rows", func(t *testing.T, d *DB) {
			require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
			require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 40)}))
			require.NoError(t, d.InsertRateLimitSnapshotsReplacingSession("codex:sess-1", []RateLimitSnapshot{rlSnap("codex:sess-1", "laptop", "codex", "primary", at, 95)}))
		}, func(t *testing.T, rows []RateLimitSnapshot) {
			require.Len(t, rows, 1, "the superseded fallback row must not survive")
			assert.Equal(t, 95.0, rows[0].UsedPercent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			tc.build(t, d)
			rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{})
			require.NoError(t, err)
			tc.check(t, rows)
		})
	}
}

// TestLatestRateLimitSnapshots_AccountIDPassesThroughAccountlessVendor: an account_id filter must not exclude a vendor whose rows carry no account.
func TestLatestRateLimitSnapshots_AccountIDPassesThroughAccountlessVendor(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "claude:sess-a", Agent: "claude"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	acctA := rlSnap("claude:sess-a", "laptop", "5h", "primary", "2026-09-09T10:00:00Z", 30)
	acctA.Vendor, acctA.AccountID = "claude", "acct-a"
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{acctA, rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 50)}))
	rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{AccountID: "acct-a"})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "account A's row plus the account-less Codex row must both return")
}

// TestRateLimitFilter_AgentAloneScopesVendorInSQL: Agent alone (Vendor
// unset) must scope the SQL itself, not just an early-return check.
func TestRateLimitFilter_AgentAloneScopesVendorInSQL(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "claude:sess-a", Agent: "claude"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	claude := rlSnap("claude:sess-a", "laptop", "5h", "primary", "2026-09-09T10:00:00Z", 30)
	claude.Vendor = "claude"
	codex := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 50)
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{claude, codex}))
	latest, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{Agent: "codex"})
	require.NoError(t, err)
	require.Len(t, latest, 1, "codex only, not every vendor")
	history, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Agent: "codex"})
	require.NoError(t, err)
	require.Len(t, history, 1, "history must scope by Agent too")
}

// TestRateLimitSnapshotHistory_MachineFilter: a comma-separated machine
// list must OR-match, not compare as one opaque string.
func TestRateLimitSnapshotHistory_MachineFilter(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-08T00:00:00Z", 20),
		rlSnap("codex:sess-2", "desktop", "codex", "primary", "2026-09-08T00:00:00Z", 50),
		rlSnap("codex:sess-2", "phone", "codex", "primary", "2026-09-08T00:00:00Z", 50),
	}))
	rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
		Machine: "laptop,desktop", LimitID: "codex", WindowKind: "primary",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "a comma-separated machine list must use IN semantics")
}

// TestRateLimitSnapshotHistory_DownsamplesWideRange: more matching rows
// than MaxPoints must come back capped at MaxPoints and still ascending.
func TestRateLimitSnapshotHistory_DownsamplesWideRange(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	const totalRows, maxPoints = 50, 10
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshots := make([]RateLimitSnapshot, totalRows)
	for i := range snapshots {
		snapshots[i] = rlSnap("codex:sess-1", "laptop", "codex", "primary",
			base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339Nano), float64(i))
	}
	require.NoError(t, d.InsertRateLimitSnapshots(snapshots))
	rows, err := d.RateLimitSnapshotHistory(context.Background(),
		RateLimitHistoryFilter{Machine: "laptop", MaxPoints: maxPoints})
	require.NoError(t, err)
	require.LessOrEqual(t, len(rows), maxPoints, "a wide range must be downsampled to at most MaxPoints")
	for i := 1; i < len(rows); i++ {
		prev, _ := time.Parse(time.RFC3339Nano, rows[i-1].ObservedAt)
		cur, _ := time.Parse(time.RFC3339Nano, rows[i].ObservedAt)
		assert.True(t, cur.After(prev), "downsampled rows must stay strictly ascending")
	}
}

// TestRateLimitAgentMatchesVendor: an exact-equality check would wrongly
// reject "codex" named alongside another agent.
func TestRateLimitAgentMatchesVendor(t *testing.T) {
	assert.True(t, RateLimitAgentMatchesVendor("", "codex"), "no filter matches everything")
	assert.True(t, RateLimitAgentMatchesVendor("claude,codex", "codex"), "codex named alongside another agent")
	assert.False(t, RateLimitAgentMatchesVendor("claude", "codex"), "a single other agent matches nothing")
}

// TestCopyRateLimitSnapshotsFrom_NullsSessionIDForUnrestoredSession pins the resync-copy invariants: an unrestored source row is copied with
// session_id NULLed and its dedup_key preserved, while a session the resync's own reparse already rebuilt keeps only its fresh row.
func TestCopyRateLimitSnapshotsFrom_NullsSessionIDForUnrestoredSession(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:superseded", Agent: "codex"}))
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:rebuilt", Agent: "codex"}))
	superseded := rlSnap("codex:superseded", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 0)
	require.NoError(t, srcDB.InsertRateLimitSnapshots([]RateLimitSnapshot{
		superseded, rlSnap("codex:rebuilt", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 40),
	}))
	srcDB.Close()
	wantDedupKey := RateLimitSnapshotDedupKey(superseded)

	// The destination does not restore "codex:superseded" but already reparsed "codex:rebuilt" with its own current row before the copy runs.
	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()
	require.NoError(t, dstDB.UpsertSession(Session{ID: "codex:rebuilt", Agent: "codex"}))
	require.NoError(t, dstDB.InsertRateLimitSnapshots([]RateLimitSnapshot{rlSnap("codex:rebuilt", "laptop", "codex", "primary", "2026-09-09T11:00:00Z", 95)}))
	require.NoError(t, dstDB.CopyRateLimitSnapshotsFrom(srcPath, nil))

	copied, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, copied, 2, "unrestored row copied, rebuilt session's stale row skipped")
	for _, r := range copied {
		if r.SessionID == "codex:rebuilt" {
			assert.Equal(t, 95.0, r.UsedPercent, "the rebuilt session's stale row must not resurface")
			continue
		}
		assert.Empty(t, r.SessionID, "session_id must be NULLed for a session absent from the destination")
		assert.Equal(t, wantDedupKey, r.DedupKey, "dedup_key must be preserved from the source, not recomputed")
	}
}

// TestInsertRateLimitSnapshots_ReattachesDetachedRow: a session copied as
// NULL-session by a resync must be reattached, not ignored, once that
// session's dedup_key reappears via a normal insert.
func TestInsertRateLimitSnapshots_ReattachesDetachedRow(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:missing", Agent: "codex"}))
	snap := rlSnap("codex:missing", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 40)
	require.NoError(t, srcDB.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	srcDB.Close()

	dstDB := testDB(t)
	require.NoError(t, dstDB.CopyRateLimitSnapshotsFrom(srcPath, nil))
	detached, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, detached, 1)
	assert.Empty(t, detached[0].SessionID, "copied row starts detached")

	require.NoError(t, dstDB.UpsertSession(Session{ID: "codex:missing", Agent: "codex"}))
	require.NoError(t, dstDB.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	reattached, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, reattached, 1, "reattachment must not duplicate the row")
	assert.Equal(t, "codex:missing", reattached[0].SessionID, "the detached row must be reattached, not left stale")
}
