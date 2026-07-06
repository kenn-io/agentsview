package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unitMsg builds a minimal message row for unit-range corpus seeding.
func unitMsg(sid string, ordinal int, role, content string) Message {
	return Message{
		SessionID: sid, Ordinal: ordinal, Role: role,
		Content: content, ContentLength: len(content), Timestamp: tsZero,
	}
}

// asSidechain marks a corpus message as is_sidechain.
func asSidechain(m Message) Message {
	m.IsSidechain = true
	return m
}

// asSystem marks a corpus message as is_system.
func asSystem(m Message) Message {
	m.IsSystem = true
	return m
}

// messageIsSidechain reads a message row's is_sidechain flag, the way a
// search enrichment pass carries the anchor's flag alongside the hit.
func messageIsSidechain(t *testing.T, d *DB, sessionID string, ordinal int) bool {
	t.Helper()
	var sidechain bool
	err := d.getReader().QueryRowContext(context.Background(),
		"SELECT is_sidechain FROM messages WHERE session_id = ? AND ordinal = ?",
		sessionID, ordinal).Scan(&sidechain)
	require.NoError(t, err)
	return sidechain
}

// unitMembers returns every member ordinal of a scanned unit: the Offsets
// walk for runs, the single ordinal for user units (Offsets is nil there).
func unitMembers(u EmbeddableUnit) []int {
	if u.Kind == "user" {
		return []int{u.Ordinal}
	}
	members := make([]int, len(u.Offsets))
	for i, o := range u.Offsets {
		members[i] = o.Ordinal
	}
	return members
}

// unitAnchorForMember builds the anchor a search path would construct for a
// member ordinal of a unit: role from the unit kind, sidechain from the
// message row, embeddable by definition of unit membership.
func unitAnchorForMember(
	t *testing.T, d *DB, u EmbeddableUnit, ordinal int,
) UnitAnchor {
	t.Helper()
	role := "user"
	if u.Kind == "run" {
		role = "assistant"
	}
	return UnitAnchor{
		SessionID:  u.SessionID,
		Ordinal:    ordinal,
		Role:       role,
		Sidechain:  messageIsSidechain(t, d, u.SessionID, ordinal),
		Embeddable: true,
	}
}

// countingUnitQuerier counts seam calls and probes while delegating to a
// real backend, so tests can assert batching and memoization behavior.
type countingUnitQuerier struct {
	inner        UnitBoundsQuerier
	boundsCalls  int
	boundsProbes int
	extentCalls  int
	extentProbes int
}

func (c *countingUnitQuerier) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	c.boundsCalls++
	c.boundsProbes += len(probes)
	return c.inner.NearestUserBoundaries(ctx, probes)
}

func (c *countingUnitQuerier) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	c.extentCalls++
	c.extentProbes += len(probes)
	return c.inner.RunExtents(ctx, probes)
}

// noQueryUnitQuerier fails any seam call: anchors that resolve locally
// (rules 1/3, missing anchors) must never reach the backend.
type noQueryUnitQuerier struct{}

func (noQueryUnitQuerier) NearestUserBoundaries(
	context.Context, []UnitProbe,
) ([]UnitBounds, error) {
	return nil, errors.New("NearestUserBoundaries must not be called")
}

func (noQueryUnitQuerier) RunExtents(
	context.Context, []ExtentProbe,
) ([][2]int, error) {
	return nil, errors.New("RunExtents must not be called")
}

func TestSubordinateSession(t *testing.T) {
	tests := []struct {
		name             string
		relationshipType string
		parentSessionID  string
		want             bool
	}{
		{"Subagent", "subagent", "", true},
		{"Fork", "fork", "", true},
		{"ForkWithParent", "fork", "parent-1", true},
		{"ContinuationWithParent", "continuation", "parent-1", false},
		{"ParentLinkedEmptyRelationship", "", "parent-1", true},
		{"ParentLinkedOtherRelationship", "related", "parent-1", true},
		{"NoParentNoRelationship", "", "", false},
		{"ContinuationWithoutParent", "continuation", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				SubordinateSession(tt.relationshipType, tt.parentSessionID))
		})
	}
}

// seedUnitRangeCorpus seeds sessions covering every structural case the
// reducer handles: plain runs, runs at session start/end, sidechain flips
// mid-run, a sidechain user boundary, system and system-prefixed rows inside
// runs and adjacent to run boundaries, a prefix-looking assistant row (which
// stays embeddable: SystemPrefixSQL only constrains user rows), single-message
// runs, and automated/subagent/fork/continuation/parent-linked sessions.
// It returns the expected total unit count.
func seedUnitRangeCorpus(t *testing.T, d *DB) int {
	t.Helper()

	insertSession(t, d, "s-plain", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-plain", 0, "user", "u0"),
		unitMsg("s-plain", 1, "assistant", "a1"),
		unitMsg("s-plain", 2, "assistant", "a2"),
		unitMsg("s-plain", 3, "user", "u3"),
		unitMsg("s-plain", 4, "assistant", "a4"),
		unitMsg("s-plain", 5, "user", "<task-notification> not a boundary"),
		unitMsg("s-plain", 6, "assistant", "a6"),
		unitMsg("s-plain", 7, "user", "u7"),
	)
	// Units: user[0], run[1,2], user[3], run members {4,6} -> [4,6], user[7].

	insertSession(t, d, "s-edges", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-edges", 0, "assistant", "a0"),
		unitMsg("s-edges", 1, "assistant", "a1"),
		unitMsg("s-edges", 2, "user", "u2"),
		unitMsg("s-edges", 3, "assistant", "a3"),
		unitMsg("s-edges", 4, "assistant", "a4"),
	)
	// Units: run[0,1] at session start, user[2], run[3,4] at session end.

	insertSession(t, d, "s-side", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-side", 0, "user", "u0"),
		unitMsg("s-side", 1, "assistant", "a1"),
		asSidechain(unitMsg("s-side", 2, "assistant", "a2")),
		asSidechain(unitMsg("s-side", 3, "assistant", "a3")),
		unitMsg("s-side", 4, "assistant", "a4"),
		asSidechain(unitMsg("s-side", 5, "user", "u5")),
		unitMsg("s-side", 6, "assistant", "a6"),
	)
	// Units: user[0], run[1,1], sidechain run[2,3], run[4,4],
	// sidechain user[5] (a user boundary regardless of sidechain), run[6,6].

	insertSession(t, d, "s-system", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-system", 0, "user", "u0"),
		asSystem(unitMsg("s-system", 1, "assistant", "sys adjacent to start")),
		unitMsg("s-system", 2, "assistant", "a2"),
		unitMsg("s-system", 3, "system", "system-role row"),
		unitMsg("s-system", 4, "assistant", "a4"),
		asSystem(unitMsg("s-system", 5, "assistant", "sys adjacent to end")),
		unitMsg("s-system", 6, "user", "u6"),
		unitMsg("s-system", 7, "assistant", "<command-message> prefixed assistant stays embeddable"),
		unitMsg("s-system", 8, "assistant", "a8"),
	)
	// Units: user[0], run members {2,4} -> [2,4], user[6], run[7,8].

	insertSession(t, d, "s-auto", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.IsAutomated = true
	})
	insertMessages(t, d,
		unitMsg("s-auto", 0, "user", "u0"),
		unitMsg("s-auto", 1, "assistant", "a1"),
		unitMsg("s-auto", 2, "assistant", "a2"),
	)
	// Units: user[0], run[1,2].

	insertSession(t, d, "s-subagent", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d,
		unitMsg("s-subagent", 0, "assistant", "a0"),
		unitMsg("s-subagent", 1, "assistant", "a1"),
	)
	// Units: run[0,1].

	insertSession(t, d, "s-fork", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.RelationshipType = "fork"
		s.ParentSessionID = Ptr("s-plain")
	})
	insertMessages(t, d, unitMsg("s-fork", 0, "user", "u0"))
	// Units: user[0].

	insertSession(t, d, "s-cont", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.RelationshipType = "continuation"
		s.ParentSessionID = Ptr("s-plain")
	})
	insertMessages(t, d,
		unitMsg("s-cont", 0, "user", "u0"),
		unitMsg("s-cont", 1, "assistant", "a1"),
	)
	// Units: user[0], run[1,1].

	insertSession(t, d, "s-parent-linked", "proj", func(s *Session) {
		s.EndedAt = Ptr(tsHour1)
		s.ParentSessionID = Ptr("s-plain")
	})
	insertMessages(t, d, unitMsg("s-parent-linked", 1, "assistant", "a1"))
	// Units: run[1,1].

	return 5 + 3 + 6 + 4 + 2 + 1 + 1 + 2 + 1
}

// TestDeriveUnitRangesReducerEquivalence is the invariant test: for every
// unit ScanEmbeddableUnits produces over the corpus (includeAutomated=true)
// and every member ordinal of that unit, DeriveUnitRanges must return exactly
// [unit.Ordinal, unit.OrdinalEnd]; user units must map to [o, o]. The units
// are checked both in one batched call over all anchors (exercising
// memoization and multi-run sessions) and per-anchor with a fresh memo.
func TestDeriveUnitRangesReducerEquivalence(t *testing.T) {
	d := testDB(t)
	wantUnits := seedUnitRangeCorpus(t, d)

	units, _ := scanUnits(t, d, "", true)
	require.Len(t, units, wantUnits, "corpus produced an unexpected unit count")

	ctx := context.Background()
	var anchors []UnitAnchor
	var want [][2]int
	for _, u := range units {
		for _, member := range unitMembers(u) {
			anchors = append(anchors, unitAnchorForMember(t, d, u, member))
			want = append(want, [2]int{u.Ordinal, u.OrdinalEnd})
			if u.Kind == "user" {
				assert.Equal(t, u.Ordinal, u.OrdinalEnd,
					"user unit %s#%d must be a single-ordinal unit",
					u.SessionID, u.Ordinal)
			}
		}
	}

	got, err := DeriveUnitRanges(ctx, d, anchors)
	require.NoError(t, err)
	require.Len(t, got, len(anchors))
	for i, a := range anchors {
		assert.Equal(t, want[i], got[i],
			"batched derivation for anchor %s#%d", a.SessionID, a.Ordinal)
	}

	for i, a := range anchors {
		single, err := DeriveUnitRanges(ctx, d, []UnitAnchor{a})
		require.NoError(t, err)
		require.Len(t, single, 1)
		assert.Equal(t, want[i], single[0],
			"per-anchor derivation for anchor %s#%d", a.SessionID, a.Ordinal)
	}
}

// TestDeriveUnitRangesLocalAnchors asserts rule-1, rule-3, and missing
// anchors resolve to [o, o] without ever touching the backend seam.
func TestDeriveUnitRangesLocalAnchors(t *testing.T) {
	tests := []struct {
		name   string
		anchor UnitAnchor
	}{
		{"EmbeddableUserRule1", UnitAnchor{
			SessionID: "s", Ordinal: 3, Role: "user", Embeddable: true,
		}},
		{"SystemRowInsideRun", UnitAnchor{
			SessionID: "s", Ordinal: 4, Role: "assistant", Embeddable: false,
		}},
		{"ToolRoleRow", UnitAnchor{
			SessionID: "s", Ordinal: 5, Role: "tool", Embeddable: true,
		}},
		{"PrefixedUserRow", UnitAnchor{
			SessionID: "s", Ordinal: 6, Role: "user", Embeddable: false,
		}},
		{"MissingAnchor", UnitAnchor{
			SessionID: "s", Ordinal: 42, Missing: true,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveUnitRanges(
				context.Background(), noQueryUnitQuerier{},
				[]UnitAnchor{tt.anchor},
			)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, [2]int{tt.anchor.Ordinal, tt.anchor.Ordinal}, got[0])
		})
	}
}

// TestDeriveUnitRangesEmptyAnchors asserts an empty anchor list returns an
// empty result without backend calls.
func TestDeriveUnitRangesEmptyAnchors(t *testing.T) {
	got, err := DeriveUnitRanges(
		context.Background(), noQueryUnitQuerier{}, nil,
	)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestDeriveUnitRangesMemoizesRunAnchors asserts 20 anchors inside one run
// cost exactly one NearestUserBoundaries batch and one RunExtents batch, each
// carrying a single probe: the first anchor derives the range, every other
// anchor reuses it from the per-session memo.
func TestDeriveUnitRangesMemoizesRunAnchors(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-memo", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	msgs := []Message{unitMsg("s-memo", 0, "user", "u0")}
	for i := 1; i <= 25; i++ {
		msgs = append(msgs, unitMsg("s-memo", i, "assistant", "a"))
	}
	msgs = append(msgs, unitMsg("s-memo", 26, "user", "u26"))
	insertMessages(t, d, msgs...)

	anchors := make([]UnitAnchor, 0, 20)
	for o := 2; o <= 21; o++ {
		anchors = append(anchors, UnitAnchor{
			SessionID: "s-memo", Ordinal: o, Role: "assistant",
			Embeddable: true,
		})
	}

	q := &countingUnitQuerier{inner: d}
	got, err := DeriveUnitRanges(context.Background(), q, anchors)
	require.NoError(t, err)
	require.Len(t, got, len(anchors))
	for i := range got {
		assert.Equal(t, [2]int{1, 25}, got[i], "anchor %d", anchors[i].Ordinal)
	}

	assert.Equal(t, 1, q.boundsCalls, "NearestUserBoundaries calls")
	assert.Equal(t, 1, q.boundsProbes, "NearestUserBoundaries probes")
	assert.Equal(t, 1, q.extentCalls, "RunExtents calls")
	assert.Equal(t, 1, q.extentProbes, "RunExtents probes")
}

// TestDeriveUnitRangesNonMemberSpan asserts a run whose members are {5, 7}
// (system row at 6) derives [5, 7] from either member anchor, and that
// system rows at 3-4 and 8-9 sitting between the user boundaries do not
// widen the range past the first/last member.
func TestDeriveUnitRangesNonMemberSpan(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-span", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-span", 2, "user", "u2"),
		asSystem(unitMsg("s-span", 3, "assistant", "sys3")),
		asSystem(unitMsg("s-span", 4, "assistant", "sys4")),
		unitMsg("s-span", 5, "assistant", "member5"),
		asSystem(unitMsg("s-span", 6, "assistant", "sys6")),
		unitMsg("s-span", 7, "assistant", "member7"),
		asSystem(unitMsg("s-span", 8, "assistant", "sys8")),
		asSystem(unitMsg("s-span", 9, "assistant", "sys9")),
		unitMsg("s-span", 10, "user", "u10"),
	)

	for _, ordinal := range []int{5, 7} {
		got, err := DeriveUnitRanges(context.Background(), d, []UnitAnchor{{
			SessionID: "s-span", Ordinal: ordinal, Role: "assistant",
			Embeddable: true,
		}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, [2]int{5, 7}, got[0], "anchor at ordinal %d", ordinal)
	}
}

// TestNearestUserBoundariesSentinels asserts the seam returns Prev=-1 and
// Next=unitOrdinalMax when no embeddable user row exists on that side, and
// real exclusive boundaries otherwise (ignoring system-prefixed user rows
// and the anchor's own ordinal).
func TestNearestUserBoundariesSentinels(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-b", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-b", 0, "assistant", "a0"),
		unitMsg("s-b", 1, "user", "u1"),
		unitMsg("s-b", 2, "assistant", "a2"),
		unitMsg("s-b", 3, "user", "<command-message> prefixed, not a boundary"),
		unitMsg("s-b", 4, "assistant", "a4"),
	)

	got, err := d.NearestUserBoundaries(context.Background(), []UnitProbe{
		{SessionID: "s-b", Ordinal: 0},
		{SessionID: "s-b", Ordinal: 2},
		{SessionID: "s-b", Ordinal: 4},
		{SessionID: "s-b", Ordinal: 1},
	})
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, UnitBounds{Prev: -1, Next: 1}, got[0],
		"no user row before session start")
	assert.Equal(t, UnitBounds{Prev: 1, Next: unitOrdinalMax}, got[1],
		"prefixed user row at 3 must not be a boundary")
	assert.Equal(t, UnitBounds{Prev: 1, Next: unitOrdinalMax}, got[2],
		"no user row after the last assistant")
	assert.Equal(t, UnitBounds{Prev: -1, Next: unitOrdinalMax}, got[3],
		"boundaries are exclusive of the probe ordinal itself")
}

// TestUnitBoundsQuerierChunkingAlignment feeds both seam methods more probes
// than one chunk binds, alternating probes with different expected answers so
// any chunk misalignment surfaces as a wrong value.
func TestUnitBoundsQuerierChunkingAlignment(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-chunk", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-chunk", 0, "user", "u0"),
		unitMsg("s-chunk", 1, "assistant", "a1"),
		unitMsg("s-chunk", 2, "assistant", "a2"),
		unitMsg("s-chunk", 3, "assistant", "a3"),
		unitMsg("s-chunk", 4, "user", "u4"),
		unitMsg("s-chunk", 5, "assistant", "a5"),
		unitMsg("s-chunk", 6, "assistant", "a6"),
		unitMsg("s-chunk", 7, "assistant", "a7"),
		unitMsg("s-chunk", 8, "user", "u8"),
	)
	const n = 400
	ctx := context.Background()

	boundProbes := make([]UnitProbe, n)
	for i := range boundProbes {
		ordinal := 2
		if i%2 == 1 {
			ordinal = 6
		}
		boundProbes[i] = UnitProbe{SessionID: "s-chunk", Ordinal: ordinal}
	}
	bounds, err := d.NearestUserBoundaries(ctx, boundProbes)
	require.NoError(t, err)
	require.Len(t, bounds, n)
	for i, b := range bounds {
		want := UnitBounds{Prev: 0, Next: 4}
		if i%2 == 1 {
			want = UnitBounds{Prev: 4, Next: 8}
		}
		assert.Equal(t, want, b, "bound probe %d", i)
	}

	extentProbes := make([]ExtentProbe, n)
	for i := range extentProbes {
		if i%2 == 0 {
			extentProbes[i] = ExtentProbe{
				SessionID: "s-chunk", Ordinal: 2, Lo: 0, Hi: 4,
			}
		} else {
			extentProbes[i] = ExtentProbe{
				SessionID: "s-chunk", Ordinal: 6, Lo: 4, Hi: 8,
			}
		}
	}
	extents, err := d.RunExtents(ctx, extentProbes)
	require.NoError(t, err)
	require.Len(t, extents, n)
	for i, e := range extents {
		want := [2]int{1, 3}
		if i%2 == 1 {
			want = [2]int{5, 7}
		}
		assert.Equal(t, want, e, "extent probe %d", i)
	}
}

// TestRunExtentsAnchorRowMissingErrors asserts the seam fails fast with
// context when a probe's anchor row does not qualify (here: the session does
// not exist), instead of silently returning a zero range.
func TestRunExtentsAnchorRowMissingErrors(t *testing.T) {
	d := testDB(t)
	_, err := d.RunExtents(context.Background(), []ExtentProbe{{
		SessionID: "no-such-session", Ordinal: 3,
		Lo: -1, Hi: unitOrdinalMax,
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-session")
}
