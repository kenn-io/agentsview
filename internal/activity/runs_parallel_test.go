package activity

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupSessionCandidates(t *testing.T) {
	pair := func(id string, ordinal int) IntervalCandidate {
		return IntervalCandidate{SessionID: id, StartOrdinal: ordinal}
	}
	tool := func(id string, ordinal, end int) IntervalCandidate {
		return IntervalCandidate{SessionID: id, StartOrdinal: ordinal, EndOrdinal: end, ClosingRole: "tool"}
	}
	// Session a's pairs come in two runs with b's between them, and x is
	// not a requested session.
	pairs := []IntervalCandidate{pair("a", 1), pair("a", 2), pair("b", 1), pair("a", 3), pair("x", 1)}
	terminal := []IntervalCandidate{tool("b", 4, 5), tool("a", 1, 2), tool("x", 1, 2), tool("b", 2, 3)}

	got := GroupSessionCandidates([]string{"a", "b", "c"}, pairs, terminal)

	require.Len(t, got, 3)
	assert.Equal(t, []IntervalCandidate{pair("a", 1), pair("a", 2), pair("a", 3)}, got[0].Pairs)
	assert.Equal(t, []IntervalCandidate{tool("a", 1, 2)}, got[0].Terminal)
	assert.Equal(t, []IntervalCandidate{pair("b", 1)}, got[1].Pairs)
	// b's terminal candidates keep the order they came in.
	assert.Equal(t, []IntervalCandidate{tool("b", 4, 5), tool("b", 2, 3)}, got[1].Terminal)
	assert.Empty(t, got[2].Pairs)
	assert.Empty(t, got[2].Terminal)
	// Joining a's pairs left the read's own slice as it was.
	assert.Equal(t, pair("b", 1), pairs[2])
}

// A message pair and a terminal candidate with the same start and ordinal
// reach the builder pair first, as MergeCandidateSlice emits them; the
// earlier piece decides the model of the overlap.
func TestDeriveSessionRunsInParallelTakesPairBeforeEqualTerminal(t *testing.T) {
	early := IntervalCandidate{
		SessionID: "s", StartOrdinal: 1, Start: at(9, 0, 0, 0), End: at(9, 0, 30, 0),
		ClosingRole: "tool", PriorModel: "early",
	}
	pairAt := IntervalCandidate{
		SessionID: "s", StartOrdinal: 3, Start: at(9, 1, 0, 0), End: at(9, 1, 30, 0),
		ClosingRole: "assistant", ClosingModel: "pair-model",
	}
	toolAt := IntervalCandidate{
		SessionID: "s", StartOrdinal: 3, Start: at(9, 1, 0, 0), End: at(9, 2, 0, 0),
		ClosingRole: "tool", PriorModel: "tool-model",
	}
	late := IntervalCandidate{
		SessionID: "s", StartOrdinal: 2, Start: at(9, 0, 40, 0), End: at(9, 0, 50, 0),
		ClosingRole: "assistant", ClosingModel: "late",
	}

	got, err := DeriveSessionRunsInParallel(t.Context(), []string{"s", "empty"}, 300, []SessionCandidates{
		// Pairs arrive out of start order.
		{Pairs: []IntervalCandidate{pairAt, late}, Terminal: []IntervalCandidate{early, toolAt}},
		{},
	})

	require.NoError(t, err)
	assert.Equal(t, map[string]SessionRuns{
		"s":     sessionRuns(early, late, pairAt, toolAt),
		"empty": sessionRuns(),
	}, got)
	assert.NotEqual(t, sessionRuns(early, late, toolAt, pairAt), got["s"])
}

// Deriving sessions apart and in parallel gives what the serial derivation
// gives over the merged candidate stream, across more sessions than one
// worker takes.
func TestDeriveSessionRunsInParallelMatchesStream(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	base := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	models := []string{"a", "b", "c"}
	ids := make([]string, 1000)
	var pairs, terminal []IntervalCandidate
	for i := range ids {
		ids[i] = fmt.Sprintf("s%04d", i)
		start := base.Add(time.Duration(rng.IntN(3600)) * time.Second)
		var session []IntervalCandidate
		for ordinal := range rng.IntN(12) {
			start = start.Add(time.Duration(rng.IntN(400)) * time.Second)
			c := IntervalCandidate{
				SessionID: ids[i], StartOrdinal: ordinal, EndOrdinal: ordinal + 1,
				Start: start, End: start.Add(time.Duration(1+rng.IntN(600)) * time.Second),
				ClosingRole: "assistant", ClosingModel: models[rng.IntN(len(models))], PriorModel: "unknown",
			}
			session = append(session, c)
			if rng.IntN(3) == 0 {
				c.EndOrdinal, c.ClosingRole, c.ClosingModel = ordinal, "tool", ""
				c.PriorModel = models[rng.IntN(len(models))]
				c.End = c.Start.Add(time.Duration(1+rng.IntN(900)) * time.Second)
				terminal = append(terminal, c)
			}
		}
		rng.Shuffle(len(session), func(a, b int) { session[a], session[b] = session[b], session[a] })
		pairs = append(pairs, session...)
	}
	sort.SliceStable(terminal, func(a, b int) bool { return CandidateLess(terminal[a], terminal[b]) })
	sorted := slices.Clone(pairs)
	sort.Slice(sorted, func(a, b int) bool { return CandidateLess(sorted[a], sorted[b]) })
	want, err := DeriveSessionRuns(t.Context(), ids, 300, MergeCandidateSlice(terminal,
		func(_ context.Context, yield func(IntervalCandidate) error) error {
			for _, c := range sorted {
				if err := yield(c); err != nil {
					return err
				}
			}
			return nil
		}))
	require.NoError(t, err)

	got, err := DeriveSessionRunsInParallel(t.Context(), ids, 300, GroupSessionCandidates(ids, pairs, terminal))

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDeriveSessionRunsInParallelStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := DeriveSessionRunsInParallel(ctx, []string{"s"}, 300, []SessionCandidates{{}})
	require.ErrorIs(t, err, context.Canceled)
}
