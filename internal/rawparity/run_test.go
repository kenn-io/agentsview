package rawparity

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func TestParityPassProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		r := Report{Counts: map[string]int{}}
		values := make([]bool, 7)
		for i := range values {
			values[i] = hegel.Draw(ht, hegel.Booleans())
		}
		if values[0] {
			r.Readiness = []Readiness{{Projected: 1}}
		} else {
			r.Readiness = []Readiness{{}}
		}
		if values[1] {
			r.Readiness[0].Pending = 1
		}
		if values[2] {
			r.Readiness[0].Failed = 1
		}
		if values[3] {
			r.Readiness[0].Unsupported = 1
		}
		for i, class := range []string{"differs", "ambiguous", "unmatchable"} {
			if values[i+4] {
				r.Counts[class] = 1
			}
		}
		r.Counts["raw_only"], r.Counts["legacy_only"] = hegel.Draw(ht, hegel.Integers(0, 1000)), hegel.Draw(ht, hegel.Integers(0, 1000))
		assert.Equal(ht, values[0] && !values[1] && !values[2] && !values[3] && !values[4] && !values[5] && !values[6], r.Passes())
	}, hegel.WithTestCases(1000))
}

func TestRunRelationshipsAndSharedTotals(t *testing.T) {
	rows := []Candidate{
		{ID: "raw-parent", PublicID: "parent", Agent: "claude", Group: "p", Aliases: []string{"parent"}, Devices: []string{"a", "b"}},
		{ID: "raw-child", PublicID: "child", Agent: "claude", Group: "c", Aliases: []string{"child"}, Devices: []string{"a"}},
		{ID: "parent", PublicID: "parent", Agent: "claude"},
		{ID: "child", PublicID: "child", Agent: "claude"},
	}
	reader := memoryReader{rows: rows, links: map[string][]Link{"raw-child": {{Kind: "parent", Targets: []string{"raw-parent"}}}}, snapshots: map[string]Snapshot{
		"raw-parent": {Session: db.Session{Agent: "claude"}}, "parent": {Session: db.Session{Agent: "claude"}},
		"raw-child": {Session: db.Session{Agent: "claude", ParentSessionID: new("raw-parent")}},
		"child":     {Session: db.Session{Agent: "claude", ParentSessionID: new("parent")}},
	}}
	got, err := Run(t.Context(), reader, Scope{})
	require.NoError(t, err)
	assert.Equal(t, 2, got.Counts["equal"])
	assert.Equal(t, 2, got.Devices["a"]["claude"]["equal"])
	assert.Equal(t, 1, got.Devices["b"]["claude"]["equal"])
	assert.Empty(t, got.Sessions)
	reader.snapshots["raw-child"] = Snapshot{Session: db.Session{Agent: "claude", Project: "changed", ParentSessionID: new("raw-parent")}}
	got, err = Run(t.Context(), reader, Scope{Device: "b"})
	require.NoError(t, err)
	assert.Zero(t, got.Counts["differs"])
	got, err = Run(t.Context(), reader, Scope{})
	require.NoError(t, err)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, []string{"metadata"}, got.Sessions[0].Differing)
}

func TestRunDeviceScopePreservesGlobalAmbiguity(t *testing.T) {
	rows := []Candidate{
		{ID: "raw-a", PublicID: "shared~branch-a", Agent: "claude", Group: "a", Aliases: []string{"shared"}, Devices: []string{"a"}},
		{ID: "raw-b", PublicID: "shared~branch-b", Agent: "claude", Group: "b", Aliases: []string{"shared"}, Devices: []string{"b"}},
		{ID: "shared", PublicID: "shared", Agent: "claude", Machine: "archive"},
	}
	reader := memoryReader{rows: rows, snapshots: map[string]Snapshot{
		"raw-a":  {Session: db.Session{Agent: "claude"}},
		"raw-b":  {Session: db.Session{Agent: "claude"}},
		"shared": {Session: db.Session{Agent: "claude"}},
	}}
	got, err := Run(t.Context(), reader, Scope{Device: "a"})
	require.NoError(t, err)
	ambiguous := Counts{"equal": 0, "differs": 0, "raw_only": 0, "legacy_only": 0, "ambiguous": 1, "unmatchable": 0}
	assert.Equal(t, ambiguous, got.Counts)
	assert.False(t, got.Pass)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "ambiguous", got.Sessions[0].Class)
	assert.ElementsMatch(t, []string{"shared~branch-a", "shared~branch-b"}, got.Sessions[0].RawIDs)
	assert.Equal(t, []LegacyRef{{ID: "shared", Machine: "archive"}}, got.Sessions[0].Legacy)
	assert.Empty(t, got.Sessions[0].Differing)
	assert.Equal(t, map[string]map[string]Counts{"a": {"claude": ambiguous}}, got.Devices)

	// Removing the other cohort makes this a genuinely unique identity pair.
	reader.rows = []Candidate{rows[0], rows[2]}
	got, err = Run(t.Context(), reader, Scope{Device: "a"})
	require.NoError(t, err)
	equal := Counts{"equal": 1, "differs": 0, "raw_only": 0, "legacy_only": 0, "ambiguous": 0, "unmatchable": 0}
	assert.Equal(t, equal, got.Counts)
	assert.True(t, got.Pass)
	assert.Empty(t, got.Sessions)
	assert.Equal(t, map[string]map[string]Counts{"a": {"claude": equal}}, got.Devices)
}

// This substitutes only storage; Run still performs real matching/comparison.
type memoryReader struct {
	rows      []Candidate
	snapshots map[string]Snapshot
	err       error
	links     map[string][]Link
}

func (r memoryReader) Links(_ context.Context, id string) ([]Link, error) { return r.links[id], r.err }

func (r memoryReader) Candidates(context.Context) ([]Candidate, error) { return r.rows, r.err }
func (r memoryReader) Readiness(context.Context, Scope) ([]Readiness, error) {
	return []Readiness{{Projected: 1}}, r.err
}

func (r memoryReader) Snapshots(_ context.Context, ids []string) (map[string]Snapshot, error) {
	out := map[string]Snapshot{}
	for _, id := range ids {
		if s, ok := r.snapshots[id]; ok {
			out[id] = s
		}
	}
	return out, r.err
}

func TestRunFailsClosedOnStorageError(t *testing.T) {
	_, err := Run(t.Context(), memoryReader{err: errors.New("read failed")}, Scope{})
	require.ErrorContains(t, err, "read failed")
}
