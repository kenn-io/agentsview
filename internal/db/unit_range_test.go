package db

import (
	"context"
	"errors"
	"fmt"
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
// real backend, so tests can assert batching behavior.
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

	insertSession(t, d, "s-sys-flip", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-sys-flip", 0, "user", "u0"),
		unitMsg("s-sys-flip", 1, "assistant", "a1"),
		asSystem(asSidechain(unitMsg("s-sys-flip", 2, "assistant", "sys+sidechain, not a flip"))),
		unitMsg("s-sys-flip", 3, "assistant", "a3"),
		unitMsg("s-sys-flip", 4, "user", "u4"),
	)
	// Units: user[0], run members {1,3} -> [1,3] (the is_system=1,
	// is_sidechain=1 assistant row at 2 is invisible: it must not act as a
	// flip boundary despite its opposite sidechain flag), user[4].

	insertSession(t, d, "s-dense", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-dense", 0, "user", "u0"),
		unitMsg("s-dense", 1, "assistant", "d1"),
		unitMsg("s-dense", 2, "user", "<task-notification> not a boundary"),
		unitMsg("s-dense", 3, "assistant", "d3"),
		unitMsg("s-dense", 4, "assistant", "d4"),
		unitMsg("s-dense", 5, "user", "u5"),
		unitMsg("s-dense", 6, "assistant", "d6"),
		asSystem(unitMsg("s-dense", 7, "assistant", "sys inside run")),
		unitMsg("s-dense", 8, "assistant", "d8"),
		unitMsg("s-dense", 9, "assistant", "d9"),
		asSidechain(unitMsg("s-dense", 10, "assistant", "sc10")),
		asSidechain(unitMsg("s-dense", 11, "assistant", "sc11")),
		asSidechain(unitMsg("s-dense", 12, "assistant", "sc12")),
		unitMsg("s-dense", 13, "assistant", "d13"),
		unitMsg("s-dense", 14, "assistant", "d14"),
		unitMsg("s-dense", 15, "assistant", "d15"),
		unitMsg("s-dense", 16, "user", "u16"),
	)
	// The dense-flow session: 12 run-member anchors in one session, so a page
	// of all of them clears UnitBoundsFlowFactor. Units: user[0], run members
	// {1,3,4} -> [1,4] (spanning the prefixed user row), user[5], run members
	// {6,8,9} -> [6,9] (spanning the system row), sidechain run[10,12],
	// run[13,15] (flip-bounded on the left, user-bounded on the right),
	// user[16].

	return 5 + 3 + 6 + 4 + 2 + 1 + 1 + 2 + 1 + 3 + 7
}

// TestDeriveUnitRangesReducerEquivalence is the invariant test: for every
// unit ScanEmbeddableUnits produces over the corpus (includeAutomated=true)
// and every member ordinal of that unit, DeriveUnitRanges must return exactly
// [unit.Ordinal, unit.OrdinalEnd]; user units must map to [o, o]. The units
// are checked both in one batched call over all anchors (exercising probe
// dedup across multi-run sessions) and one call per anchor (no dedup to
// exercise, each call trivially batches a single probe).
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

// TestDeriveUnitRangesReducerEquivalenceDenseFlow is the dense-flow variant
// of the invariant test: a page holding every run-member anchor of the
// structurally rich s-dense corpus session (multi-run, sidechain flip,
// prefixed user row, and system rows) clears UnitBoundsFlowFactor, so
// derivation fetches real user bounds with one NearestUserBoundaries call —
// and must still return exactly the reducer's extents, identical to the
// sparse per-anchor derivation.
func TestDeriveUnitRangesReducerEquivalenceDenseFlow(t *testing.T) {
	d := testDB(t)
	seedUnitRangeCorpus(t, d)
	units, _ := scanUnits(t, d, "", true)

	ctx := context.Background()
	var anchors []UnitAnchor
	var want [][2]int
	for _, u := range units {
		if u.SessionID != "s-dense" || u.Kind != "run" {
			continue
		}
		for _, member := range unitMembers(u) {
			anchors = append(anchors, unitAnchorForMember(t, d, u, member))
			want = append(want, [2]int{u.Ordinal, u.OrdinalEnd})
		}
	}
	require.GreaterOrEqual(t, len(anchors), UnitBoundsFlowFactor,
		"s-dense must supply at least UnitBoundsFlowFactor distinct run anchors "+
			"in one session; extend the corpus session if the factor grows")

	q := &countingUnitQuerier{inner: d}
	got, err := DeriveUnitRanges(ctx, q, anchors)
	require.NoError(t, err)
	require.Len(t, got, len(anchors))
	for i, a := range anchors {
		assert.Equal(t, want[i], got[i],
			"dense-flow derivation for anchor %s#%d", a.SessionID, a.Ordinal)
	}
	assert.Equal(t, 1, q.boundsCalls,
		"dense page must fetch real user bounds (dense flow)")

	// The sparse flow must agree exactly: one probe per call stays under the
	// flow gate and probes with sentinel bounds.
	for i, a := range anchors {
		single, err := DeriveUnitRanges(ctx, d, []UnitAnchor{a})
		require.NoError(t, err)
		require.Len(t, single, 1)
		assert.Equal(t, want[i], single[0],
			"sparse derivation for anchor %s#%d", a.SessionID, a.Ordinal)
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

// TestDeriveUnitRangesBatchesRunAnchors pins the DENSE flow's call counts: a
// session-dense page of anchors inside one run costs exactly one
// NearestUserBoundaries CALL (the anchor count is derived from
// UnitBoundsFlowFactor so the single-session page always clears the gate;
// every pending anchor rides one batch with duplicate (session, ordinal)
// anchors sharing one probe) and one RunExtents CALL carrying just ONE
// probe: the anchors share a (session, bounds, sidechain) group, so a single
// representative resolves the run and its extent is handed to every anchor
// it covers with no second round.
func TestDeriveUnitRangesBatchesRunAnchors(t *testing.T) {
	d := testDB(t)
	// Comfortably past the gate in one session, with the run extending on
	// both sides of the anchored ordinals.
	anchorCount := 2*UnitBoundsFlowFactor + 4
	runLen := anchorCount + 5
	insertSession(t, d, "s-batch", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	msgs := []Message{unitMsg("s-batch", 0, "user", "u0")}
	for i := 1; i <= runLen; i++ {
		msgs = append(msgs, unitMsg("s-batch", i, "assistant", "a"))
	}
	msgs = append(msgs, unitMsg("s-batch", runLen+1, "user", "u-end"))
	insertMessages(t, d, msgs...)

	anchors := make([]UnitAnchor, 0, anchorCount+1)
	for o := 2; o < 2+anchorCount; o++ {
		anchors = append(anchors, UnitAnchor{
			SessionID: "s-batch", Ordinal: o, Role: "assistant",
			Embeddable: true,
		})
	}
	// Duplicate of the first anchor: must reuse its probe, not add one.
	anchors = append(anchors, anchors[0])

	q := &countingUnitQuerier{inner: d}
	got, err := DeriveUnitRanges(context.Background(), q, anchors)
	require.NoError(t, err)
	require.Len(t, got, len(anchors))
	for i := range got {
		assert.Equal(t, [2]int{1, runLen}, got[i], "anchor %d", anchors[i].Ordinal)
	}

	assert.Equal(t, 1, q.boundsCalls, "NearestUserBoundaries calls (dense page)")
	assert.Equal(t, anchorCount, q.boundsProbes, "NearestUserBoundaries probes")
	assert.Equal(t, 1, q.extentCalls, "RunExtents calls")
	assert.Equal(t, 1, q.extentProbes, "RunExtents probes (one group representative)")
}

// TestDeriveUnitRangesSecondRoundAcrossFlip seeds one user interval holding
// two runs separated by a sidechain flip and anchors both runs' main-chain
// members. The main-chain anchors share one (session, interval, sidechain)
// group but sit in different runs, so the round-one representative's extent
// cannot cover the anchors past the flip: they must resolve in exactly one
// second RunExtents round, with correct per-run extents.
func TestDeriveUnitRangesSecondRoundAcrossFlip(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-flip2", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-flip2", 0, "user", "u0"),
		unitMsg("s-flip2", 1, "assistant", "run1-a"),
		unitMsg("s-flip2", 2, "assistant", "run1-b"),
		asSidechain(unitMsg("s-flip2", 3, "assistant", "sidechain flip")),
		unitMsg("s-flip2", 4, "assistant", "run2-a"),
		unitMsg("s-flip2", 5, "assistant", "run2-b"),
		unitMsg("s-flip2", 6, "user", "u6"),
	)

	anchors := []UnitAnchor{
		{SessionID: "s-flip2", Ordinal: 1, Role: "assistant", Embeddable: true},
		{SessionID: "s-flip2", Ordinal: 2, Role: "assistant", Embeddable: true},
		{SessionID: "s-flip2", Ordinal: 4, Role: "assistant", Embeddable: true},
		{SessionID: "s-flip2", Ordinal: 5, Role: "assistant", Embeddable: true},
	}
	// This test pins the SPARSE flow: the page must stay under the gate so no
	// NearestUserBoundaries round runs. Guard the coupling explicitly instead
	// of letting a lowered UnitBoundsFlowFactor flip the flow silently.
	require.Less(t, len(anchors), UnitBoundsFlowFactor,
		"across-flip page must stay sparse; restructure the test if the flow factor shrinks")
	q := &countingUnitQuerier{inner: d}
	got, err := DeriveUnitRanges(context.Background(), q, anchors)
	require.NoError(t, err)
	require.Len(t, got, len(anchors))
	assert.Equal(t, [2]int{1, 2}, got[0], "run 1 anchor 1")
	assert.Equal(t, [2]int{1, 2}, got[1], "run 1 anchor 2")
	assert.Equal(t, [2]int{4, 5}, got[2], "run 2 anchor 4")
	assert.Equal(t, [2]int{4, 5}, got[3], "run 2 anchor 5")

	assert.Equal(t, 0, q.boundsCalls,
		"NearestUserBoundaries calls (sparse page probes with sentinel bounds)")
	assert.Equal(t, 2, q.extentCalls,
		"RunExtents calls (representative round + across-flip remainder)")
	assert.Equal(t, 3, q.extentProbes,
		"RunExtents probes (1 representative + 2 across the flip)")
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
// Next=UnitOrdinalMax when no embeddable user row exists on that side, and
// real exclusive boundaries otherwise (ignoring system-prefixed user rows
// and the anchor's own ordinal). The empty-content user row at 5 pins the
// SQLite first-code-point guard's COALESCE path: unicode(”) is NULL, and an
// empty user row is still an embeddable boundary.
func TestNearestUserBoundariesSentinels(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-b", "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
	insertMessages(t, d,
		unitMsg("s-b", 0, "assistant", "a0"),
		unitMsg("s-b", 1, "user", "u1"),
		unitMsg("s-b", 2, "assistant", "a2"),
		unitMsg("s-b", 3, "user", "<command-message> prefixed, not a boundary"),
		unitMsg("s-b", 4, "assistant", "a4"),
		unitMsg("s-b", 5, "user", ""),
		unitMsg("s-b", 6, "assistant", "a6"),
	)

	got, err := d.NearestUserBoundaries(context.Background(), []UnitProbe{
		{SessionID: "s-b", Ordinal: 0},
		{SessionID: "s-b", Ordinal: 2},
		{SessionID: "s-b", Ordinal: 4},
		{SessionID: "s-b", Ordinal: 1},
		{SessionID: "s-b", Ordinal: 6},
	})
	require.NoError(t, err)
	require.Len(t, got, 5)
	assert.Equal(t, UnitBounds{Prev: -1, Next: 1}, got[0],
		"no user row before session start")
	assert.Equal(t, UnitBounds{Prev: 1, Next: 5}, got[1],
		"prefixed user row at 3 must not be a boundary; empty user row at 5 is")
	assert.Equal(t, UnitBounds{Prev: 1, Next: 5}, got[2],
		"empty-content user row is an embeddable boundary")
	assert.Equal(t, UnitBounds{Prev: -1, Next: 5}, got[3],
		"boundaries are exclusive of the probe ordinal itself")
	assert.Equal(t, UnitBounds{Prev: 5, Next: UnitOrdinalMax}, got[4],
		"no user row after the last assistant")
}

// TestUnitBoundsQuerierChunkingAlignment seeds more sessions than either
// seam method batches into one statement (unitSessionChunk sessions for
// NearestUserBoundaries, unitExtentChunk probes for RunExtents), so both
// must run their per-chunk loop across multiple statements. Each session k
// gets its own ordinal base (b = 10*k), so its boundary/extent answer is
// unique to that session: {Prev: b, Next: b+3} and extent [b+1, b+2]. A
// chunk-boundary slicing bug (e.g. an off-by-one in the chunk start/end
// arithmetic) either drops a slot (surfacing as an "index out of range" or
// row-count error) or shifts results between neighboring sessions — and
// because every session's expected value differs from its neighbors', a
// shift produces a wrong value rather than a coincidentally correct one.
// Every probe's expected value is asserted individually against its own
// session's base, so a single misattributed slot fails the assertion for
// that specific session.
func TestUnitBoundsQuerierChunkingAlignment(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// >1 chunk boundary crossed for both methods (unitExtentChunk is the
	// smaller of the two).
	const n = max(unitSessionChunk, unitExtentChunk) + 20

	sessionID := func(k int) string { return fmt.Sprintf("s-chunk-%d", k) }
	base := func(k int) int { return k * 10 }

	for k := range n {
		b := base(k)
		insertSession(t, d, sessionID(k), "proj", func(s *Session) { s.EndedAt = Ptr(tsHour1) })
		insertMessages(t, d,
			unitMsg(sessionID(k), b, "user", "u-before"),
			unitMsg(sessionID(k), b+1, "assistant", "member1"),
			unitMsg(sessionID(k), b+2, "assistant", "member2"),
			unitMsg(sessionID(k), b+3, "user", "u-after"),
		)
	}

	boundProbes := make([]UnitProbe, n)
	for k := range boundProbes {
		b := base(k)
		ordinal := b + 1
		if k%2 == 1 {
			ordinal = b + 2
		}
		boundProbes[k] = UnitProbe{SessionID: sessionID(k), Ordinal: ordinal}
	}
	bounds, err := d.NearestUserBoundaries(ctx, boundProbes)
	require.NoError(t, err)
	require.Len(t, bounds, n)
	for k, got := range bounds {
		b := base(k)
		assert.Equal(t, UnitBounds{Prev: b, Next: b + 3}, got,
			"bound probe for session %s", sessionID(k))
	}

	extentProbes := make([]ExtentProbe, n)
	for k := range extentProbes {
		b := base(k)
		ordinal := b + 1
		if k%2 == 1 {
			ordinal = b + 2
		}
		extentProbes[k] = ExtentProbe{
			SessionID: sessionID(k), Ordinal: ordinal, Lo: b, Hi: b + 3,
		}
	}
	extents, err := d.RunExtents(ctx, extentProbes)
	require.NoError(t, err)
	require.Len(t, extents, n)
	for k, got := range extents {
		b := base(k)
		assert.Equal(t, [2]int{b + 1, b + 2}, got,
			"extent probe for session %s", sessionID(k))
	}
}

// TestRunExtentsAnchorRowMissingErrors asserts the seam fails fast with
// context when a probe's anchor row does not qualify (here: the session does
// not exist), instead of silently returning a zero range.
func TestRunExtentsAnchorRowMissingErrors(t *testing.T) {
	d := testDB(t)
	_, err := d.RunExtents(context.Background(), []ExtentProbe{{
		SessionID: "no-such-session", Ordinal: 3,
		Lo: -1, Hi: UnitOrdinalMax,
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-session")
}
