package activity

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"time"
	"unsafe"
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
	model := candidateModel(c)
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
	runs := SessionRuns{
		bounds: slices.Clone(b.runs.bounds), trimmed: slices.Clone(b.runs.trimmed), runs: slices.Clone(b.runs.runs),
	}
	b.runs.bounds, b.runs.trimmed, b.runs.runs = b.runs.bounds[:0], b.runs.trimmed[:0], b.runs.runs[:0]
	b.reach, b.reached = 0, false
	return runs
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
