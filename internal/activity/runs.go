package activity

import (
	"cmp"
	"context"
	"runtime"
	"slices"
	"sort"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"
)

// Run is a stretch of one session's activity that needs no further
// resolving: touching pieces, each already capped at the gap and trimmed
// against the session's earlier activity, attributed to one model and
// clipped to the report range. Tail is the start of the run's last piece,
// which bucket membership depends on.
type Run struct {
	SessionID        string
	Start, End, Tail time.Time
	Model            string
}

// RunSource emits runs in start order.
type RunSource func(ctx context.Context, yield func(Run) error) error

// RunSourceBackend is a report backend that keeps each session's runs and
// sweeps those instead of streaming interval candidates.
type RunSourceBackend interface {
	ActivityReportRunSource([]string, Query) RunSource
}

// BuildRunArtifactsWithSurvivorUsage is BuildCandidateArtifactsFromSourceWithSurvivorUsage
// for runs. It gives the same artifacts as the candidates the runs were
// derived from.
func BuildRunArtifactsWithSurvivorUsage(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source RunSource,
	usage func() ([]UsageRow, error),
) (CandidateArtifacts, error) {
	return buildIntervalArtifacts(ctx, p, sessions, func(yield func(interval) error) error {
		return source(ctx, func(run Run) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !run.End.After(run.Start) {
				return nil
			}
			return yield(interval{
				sessionID: run.SessionID, start: run.Start, end: run.End,
				tail: run.Tail, model: run.Model,
			})
		})
	}, usage, true, nil)
}

// SessionRuns is one session's activity reduced to runs. It depends on the
// session's candidates and the gap cap, not on the report range; Clip
// applies the range.
type SessionRuns struct {
	// bounds holds each run's piece boundaries in Unix microseconds, the
	// precision the aggregator compares candidates at. A run of n pieces
	// takes n+1 entries.
	bounds []int64
	// trimmed marks, by its start's index in bounds, each piece whose
	// candidate started before it, under an earlier piece of the session.
	trimmed []uint64
	runs    []sessionRun
}

type sessionRun struct {
	model string
	// first and last index the run's start and end in bounds.
	first, last int32
}

// Bytes estimates the memory the runs hold.
func (r SessionRuns) Bytes() int64 {
	return int64(unsafe.Sizeof(r)) + 8*int64(len(r.bounds)+len(r.trimmed)) +
		int64(unsafe.Sizeof(sessionRun{}))*int64(len(r.runs))
}

// Runs counts the runs.
func (r SessionRuns) Runs() int { return len(r.runs) }

// SessionRunsBuilder derives one session's runs from its candidates. Add
// takes them in the order the aggregator would sweep them: by start, and
// equal starts in the order the backend's candidate stream emits them.
type SessionRunsBuilder struct {
	gapCap  int64
	runs    SessionRuns
	reach   int64
	reached bool
}

// NewSessionRunsBuilder returns a builder for the gap cap of a query.
func NewSessionRunsBuilder(gapCapSeconds float64) *SessionRunsBuilder {
	gapCap := time.Duration(gapCapSeconds) * time.Second
	return &SessionRunsBuilder{gapCap: gapCap.Microseconds()}
}

// Add resolves one candidate as the aggregator would without a range:
// it caps the candidate at the gap and keeps only the part after the
// session's earlier pieces.
func (b *SessionRunsBuilder) Add(c IntervalCandidate) {
	start := c.Start.Truncate(time.Microsecond).UnixMicro()
	end := c.End.Truncate(time.Microsecond).UnixMicro()
	if end <= start {
		return
	}
	end = min(end, start+b.gapCap)
	if end <= start || (b.reached && end <= b.reach) {
		return
	}
	trimmed := b.reached && b.reach > start
	if trimmed {
		start = b.reach
	}
	b.reach, b.reached = end, true
	b.AddPiece(candidateModel(c), start, end, trimmed)
}

// AddPiece appends a piece Add has already resolved: it spans [start, end)
// in Unix microseconds, starts where the session's earlier pieces reach or
// later, and is trimmed when its candidate started under an earlier piece.
func (b *SessionRunsBuilder) AddPiece(model string, start, end int64, trimmed bool) {
	r := &b.runs
	if n := len(r.runs); n > 0 && r.runs[n-1].model == model && r.bounds[r.runs[n-1].last] == start {
		r.bounds = append(r.bounds, end)
		r.runs[n-1].last++
	} else {
		r.bounds = append(r.bounds, start, end)
		last := int32(len(r.bounds) - 1)
		r.runs = append(r.runs, sessionRun{model: model, first: last - 1, last: last})
	}
	if trimmed {
		i := len(r.bounds) - 2
		for len(r.trimmed) <= i/64 {
			r.trimmed = append(r.trimmed, 0)
		}
		r.trimmed[i/64] |= 1 << (i % 64)
	}
}

func (r SessionRuns) pieceTrimmed(i int32) bool {
	return int(i/64) < len(r.trimmed) && r.trimmed[i/64]&(1<<(i%64)) != 0
}

// Finish returns the session's runs and resets the builder for the next
// session, keeping its buffers.
func (b *SessionRunsBuilder) Finish() SessionRuns {
	// Appending to nil keeps an empty part nil, as a fresh builder leaves it.
	runs := SessionRuns{
		bounds:  append([]int64(nil), b.runs.bounds...),
		trimmed: append([]uint64(nil), b.runs.trimmed...),
		runs:    append([]sessionRun(nil), b.runs.runs...),
	}
	b.runs.bounds, b.runs.trimmed, b.runs.runs = b.runs.bounds[:0], b.runs.trimmed[:0], b.runs.runs[:0]
	b.reach, b.reached = 0, false
	return runs
}

// DeriveSessionRuns reduces the candidates source streams for the sessions
// ids names to each session's runs and ignores other sessions. source must
// stream in sweep order: each session's candidates then reach its builder in
// the order the aggregator takes them.
func DeriveSessionRuns(
	ctx context.Context, ids []string, gapCapSeconds float64, source CandidateSource,
) (map[string]SessionRuns, error) {
	index := make(map[string]int, len(ids))
	builders := make([]SessionRunsBuilder, len(ids))
	for i, id := range ids {
		index[id] = i
		builders[i] = *NewSessionRunsBuilder(gapCapSeconds)
	}
	err := source(ctx, func(c IntervalCandidate) error {
		if i, ok := index[c.SessionID]; ok {
			builders[i].Add(c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	runs := make(map[string]SessionRuns, len(ids))
	for id, i := range index {
		runs[id] = builders[i].Finish()
	}
	return runs, nil
}

// SessionCandidates are one session's interval candidates as a backend
// read them: its message pairs in any order, and its terminal tool
// candidates in the order the backend's candidate stream emits them.
type SessionCandidates struct {
	Pairs, Terminal []IntervalCandidate
}

// GroupSessionCandidates arranges the candidates a pairing read found for
// ids as DeriveSessionRunsInParallel takes them. A session's pairs are
// expected next to each other, as a read that pairs one session at a time
// appends them; pairs of one session found apart are joined. Each session's
// terminal candidates keep their order. Candidates of sessions outside ids
// are dropped.
func GroupSessionCandidates(ids []string, pairs, terminal []IntervalCandidate) []SessionCandidates {
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}
	out := make([]SessionCandidates, len(ids))
	for lo := 0; lo < len(pairs); {
		hi := lo + 1
		for hi < len(pairs) && pairs[hi].SessionID == pairs[lo].SessionID {
			hi++
		}
		if i, ok := index[pairs[lo].SessionID]; ok {
			if out[i].Pairs == nil {
				// Capping the capacity makes a later join copy instead of
				// overwriting the next session's pairs.
				out[i].Pairs = pairs[lo:hi:hi]
			} else {
				out[i].Pairs = append(out[i].Pairs, pairs[lo:hi]...)
			}
		}
		lo = hi
	}
	for _, c := range terminal {
		if i, ok := index[c.SessionID]; ok {
			out[i].Terminal = append(out[i].Terminal, c)
		}
	}
	return out
}

// parallelChunk is how many items an EachInParallel worker takes at a
// time. Sessions differ widely in size, so workers take small chunks in
// turn instead of one equal share each.
const parallelChunk = 256

// EachInParallel calls each(w, i) for every i in [0, n) on as many
// goroutines as can run at once. Every goroutine gets its own w from
// newWorker, so each may reuse w's scratch space between calls. It stops
// taking items once ctx is done and returns ctx's error.
func EachInParallel[W any](ctx context.Context, n int, newWorker func() W, each func(w W, i int)) error {
	workers := min(runtime.GOMAXPROCS(0), (n+parallelChunk-1)/parallelChunk)
	var next atomic.Int64
	g, ctx := errgroup.WithContext(ctx)
	for range workers {
		g.Go(func() error {
			w := newWorker()
			for {
				start := int(next.Add(parallelChunk)) - parallelChunk
				if start >= n {
					return nil
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				for i := start; i < min(start+parallelChunk, n); i++ {
					each(w, i)
				}
			}
		})
	}
	return g.Wait()
}

// DeriveSessionRunsInParallel is DeriveSessionRuns over the stream that
// merges each session's terminal candidates into its message pairs, sorted
// by CandidateLess, as MergeCandidateSlice merges them. sessions[i] holds
// the candidates of ids[i]. A session's runs depend only on its own
// candidates, in the order the stream gives them, so sessions are derived
// in parallel. It sorts each session's pairs in place.
func DeriveSessionRunsInParallel(
	ctx context.Context, ids []string, gapCapSeconds float64, sessions []SessionCandidates,
) (map[string]SessionRuns, error) {
	derived := make([]SessionRuns, len(ids))
	if err := EachInParallel(ctx, len(ids), func() *SessionRunsBuilder {
		return NewSessionRunsBuilder(gapCapSeconds)
	}, func(builder *SessionRunsBuilder, i int) {
		derived[i] = deriveSession(builder, sessions[i])
	}); err != nil {
		return nil, err
	}
	runs := make(map[string]SessionRuns, len(ids))
	for i, id := range ids {
		runs[id] = derived[i]
	}
	return runs, nil
}

// deriveSession merges one session's candidates as MergeCandidateSlice
// does, emitting a terminal candidate only before a pair it sorts strictly
// before, and reduces them to runs.
func deriveSession(builder *SessionRunsBuilder, session SessionCandidates) SessionRuns {
	slices.SortFunc(session.Pairs, compareCandidates)
	terminal := session.Terminal
	for _, c := range session.Pairs {
		for len(terminal) > 0 && CandidateLess(terminal[0], c) {
			builder.Add(terminal[0])
			terminal = terminal[1:]
		}
		builder.Add(c)
	}
	for _, c := range terminal {
		builder.Add(c)
	}
	return builder.Finish()
}

func compareCandidates(a, b IntervalCandidate) int {
	switch {
	case CandidateLess(a, b):
		return -1
	case CandidateLess(b, a):
		return 1
	}
	return 0
}

// AppendClipped appends the session's runs clipped to [start, end), the
// range start and effective end of a report, in start order.
func (r SessionRuns) AppendClipped(out []Run, sessionID string, start, end time.Time) []Run {
	return r.AppendClippedBefore(out, sessionID, start, end, end)
}

// AppendClippedBefore is AppendClipped for a candidate stream that also
// leaves out every candidate starting at or after before, which is at most
// end. A piece whose candidate started earlier, under an earlier piece of
// the session, stays even when the piece itself starts at before.
func (r SessionRuns) AppendClippedBefore(out []Run, sessionID string, start, end, before time.Time) []Run {
	at := func(i int32) time.Time { return time.UnixMicro(r.bounds[i]).UTC() }
	for _, run := range r.runs {
		runStart := at(run.first)
		if !runStart.Before(end) || runStart.After(before) ||
			(runStart.Equal(before) && !r.pieceTrimmed(run.first)) {
			break
		}
		// kept counts the pieces left: those starting before both bounds,
		// and a trimmed piece starting at before. Piece starts increase.
		pieces := int(run.last - run.first)
		kept := sort.Search(pieces, func(i int) bool { return !at(run.first + int32(i)).Before(before) })
		if i := run.first + int32(kept); kept < pieces && at(i).Equal(before) && at(i).Before(end) && r.pieceTrimmed(i) {
			kept++
		}
		runEnd := at(run.first + int32(kept))
		if !runEnd.After(start) {
			continue
		}
		// Clip the way EffectiveIntervalBounds clips each piece.
		clippedStart, clippedEnd := runStart, runEnd
		if clippedStart.Before(start) {
			clippedStart = start
		}
		tail := at(run.first + int32(kept) - 1)
		if clippedEnd.After(end) {
			clippedEnd = end
			// The last piece left is the last that starts before the end.
			i := sort.Search(kept, func(i int) bool { return !at(run.first + int32(i)).Before(end) })
			tail = at(run.first + int32(i) - 1)
		}
		if !clippedEnd.After(clippedStart) {
			continue
		}
		if tail.Before(clippedStart) {
			tail = clippedStart
		}
		out = append(out, Run{
			SessionID: sessionID, Start: clippedStart, End: clippedEnd, Tail: tail, Model: run.model,
		})
	}
	return out
}

// SortRuns orders runs of many sessions by start for the aggregator.
func SortRuns(runs []Run) {
	slices.SortFunc(runs, func(a, b Run) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.SessionID, b.SessionID))
	})
}
