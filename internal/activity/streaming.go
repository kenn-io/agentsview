package activity

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"time"
)

// IntervalCandidate is one pair of adjacent timestamped messages. Storage
// backends emit this mechanical shape; AggregateCandidates owns range, gap,
// duration, and model-attribution semantics.
type IntervalCandidate struct {
	SessionID    string
	StartOrdinal int
	EndOrdinal   int
	Start        time.Time
	End          time.Time
	ClosingRole  string
	ClosingModel string
	PriorModel   string
}

// BucketMembership is a compact set of bucket indexes for one session.
type BucketMembership []uint64

// Contains reports whether the session belongs to bucket index.
func (m BucketMembership) Contains(index int) bool {
	if index < 0 || index/64 >= len(m) {
		return false
	}
	return m[index/64]&(uint64(1)<<uint(index%64)) != 0
}

func (m BucketMembership) add(index int) {
	if index < 0 || index/64 >= len(m) {
		return
	}
	m[index/64] |= uint64(1) << uint(index%64)
}

// CandidateArtifacts contains the bounded report and per-session drill-down
// membership retained by the server cache.
type CandidateArtifacts struct {
	Report     Report
	Sessions   []SessionRow
	Membership map[string]BucketMembership
}

// CandidateSource emits adjacent-message pairs in candidate-start order.
// Sources may scan database rows directly; callers must not retain candidates
// after yield returns.
type CandidateSource func(
	ctx context.Context, yield func(IntervalCandidate) error,
) error

// MergeCandidateSlice merges an ordered candidate slice into an ordered
// candidate source without retaining the source stream. Equal keys from the
// source are emitted first, matching storage query event ordering.
func MergeCandidateSlice(
	extra []IntervalCandidate, source CandidateSource,
) CandidateSource {
	return func(
		ctx context.Context, yield func(IntervalCandidate) error,
	) error {
		next := 0
		emitExtraBefore := func(candidate IntervalCandidate) error {
			for next < len(extra) && candidateLess(extra[next], candidate) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(extra[next]); err != nil {
					return err
				}
				next++
			}
			return yield(candidate)
		}
		if err := source(ctx, emitExtraBefore); err != nil {
			return err
		}
		for next < len(extra) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(extra[next]); err != nil {
				return err
			}
			next++
		}
		return nil
	}
}

func candidateLess(a, b IntervalCandidate) bool {
	if !a.Start.Equal(b.Start) {
		return a.Start.Before(b.Start)
	}
	if a.SessionID != b.SessionID {
		return a.SessionID < b.SessionID
	}
	return a.StartOrdinal < b.StartOrdinal
}

// PairActivityEvents is the Go reference implementation for backend pairing.
// It preserves ordinal adjacency before applying the safe candidate-start
// pruning window.
func PairActivityEvents(
	events []ActivityEvent, rangeStart, calculationEnd time.Time, gapCap time.Duration,
) []IntervalCandidate {
	bySession := make(map[string][]ActivityEvent)
	for _, event := range events {
		if _, ok := parseTS(event.Timestamp); ok {
			bySession[event.SessionID] = append(bySession[event.SessionID], event)
		}
	}
	lower := rangeStart.Add(-gapCap)
	var out []IntervalCandidate
	for sessionID, sessionEvents := range bySession {
		sort.Slice(sessionEvents, func(i, j int) bool {
			return sessionEvents[i].Ordinal < sessionEvents[j].Ordinal
		})
		lastModel := "unknown"
		for i := 1; i < len(sessionEvents); i++ {
			previous, _ := parseTS(sessionEvents[i-1].Timestamp)
			current, _ := parseTS(sessionEvents[i].Timestamp)
			candidate := IntervalCandidate{
				SessionID:    sessionID,
				StartOrdinal: sessionEvents[i-1].Ordinal,
				EndOrdinal:   sessionEvents[i].Ordinal,
				Start:        previous,
				End:          current,
				ClosingRole:  sessionEvents[i].Role,
				ClosingModel: sessionEvents[i].Model,
				PriorModel:   lastModel,
			}
			if !previous.Before(lower) && previous.Before(calculationEnd) {
				out = append(out, candidate)
			}
			if current.After(previous) && sessionEvents[i].Role == "assistant" &&
				sessionEvents[i].Model != "" {
				lastModel = sessionEvents[i].Model
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return candidateLess(out[i], out[j]) })
	return out
}

// AggregateCandidates builds a report from ordered adjacent-message pairs.
func AggregateCandidates(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	candidates []IntervalCandidate,
	usage []UsageRow,
) (Report, error) {
	artifacts, err := BuildCandidateArtifacts(ctx, p, sessions, candidates, usage)
	artifacts.Report.BySession = artifacts.Sessions
	return artifacts.Report, err
}

// AggregateCandidateSource builds a report without retaining the candidate
// stream in memory.
func AggregateCandidateSource(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	usage []UsageRow,
) (Report, error) {
	artifacts, err := BuildCandidateArtifactsFromSource(
		ctx, p, sessions, source, usage,
	)
	artifacts.Report.BySession = artifacts.Sessions
	return artifacts.Report, err
}

// BuildCandidateArtifacts aggregates candidates and derives compact
// second-resolution bucket membership without serializing raw intervals.
func BuildCandidateArtifacts(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	candidates []IntervalCandidate,
	usage []UsageRow,
) (CandidateArtifacts, error) {
	return BuildCandidateArtifactsFromSource(
		ctx, p, sessions,
		func(ctx context.Context, yield func(IntervalCandidate) error) error {
			for _, candidate := range candidates {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(candidate); err != nil {
					return err
				}
			}
			return nil
		},
		usage,
	)
}

// BuildCandidateArtifactsFromSource aggregates an ordered candidate stream
// while retaining only bounded bucket state and per-session accumulators.
func BuildCandidateArtifactsFromSource(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	usage []UsageRow,
) (CandidateArtifacts, error) {
	return buildCandidateArtifactsFromSource(
		ctx, p, sessions, source, usage, false,
	)
}

// BuildCandidateArtifactsFromSourceWithSurvivorUsage is the store-facing
// variant for usage rows that have already passed the shared survivor
// selection. It prevents a second deduplication pass while keeping slice-backed
// callers compatible with the raw-usage API above.
func BuildCandidateArtifactsFromSourceWithSurvivorUsage(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	usage []UsageRow,
) (CandidateArtifacts, error) {
	return buildCandidateArtifactsFromSource(
		ctx, p, sessions, source, usage, true,
	)
}

func buildCandidateArtifactsFromSource(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	usage []UsageRow,
	usageIsSurvivorSet bool,
) (CandidateArtifacts, error) {
	windows := rangeWindows(p)
	membershipWindows := secondPrecisionWindows(windows)
	report := newReport(p, windows)
	report.Buckets = make([]Bucket, len(windows))
	for i, window := range windows {
		report.Buckets[i] = Bucket{
			Start: window.Start.Format(time.RFC3339),
			End:   window.End.Format(time.RFC3339),
		}
	}
	automatedBy := automatedSet(sessions)
	state := candidateSweep{
		report: &report, windows: windows, effectiveEnd: p.EffectiveEnd,
		last: p.RangeStart, automatedBy: automatedBy,
		sessionEnds: make(map[string]time.Time),
	}
	heap.Init(&state.ends)
	aggregates := make(map[string]*sessionIntervalAgg)
	membership := make(map[string]BucketMembership)
	words := (len(windows) + 63) / 64
	var previousStart time.Time
	err := source(ctx, func(candidate IntervalCandidate) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		iv, effective := effectiveCandidateInterval(p, candidate)
		if !effective {
			return nil
		}
		if !previousStart.IsZero() && iv.start.Before(previousStart) {
			return fmt.Errorf(
				"activity candidates out of start order: %s before %s",
				iv.start.Format(time.RFC3339Nano),
				previousStart.Format(time.RFC3339Nano),
			)
		}
		previousStart = iv.start
		state.advance(iv.start)
		if previousEnd, ok := state.sessionEnds[iv.sessionID]; ok {
			if !iv.end.After(previousEnd) {
				return nil
			}
			iv.start = previousEnd
			state.extend(iv)
		} else {
			state.open(iv)
		}
		foldCandidateInterval(
			&report, windows, membershipWindows, aggregates, membership, words,
			automatedBy, iv,
		)
		return nil
	})
	if err != nil {
		return CandidateArtifacts{}, err
	}
	state.advance(p.EffectiveEnd)
	report.Totals.ActiveMinutes = state.active.Minutes()
	report.Totals.IdleMinutes = p.EffectiveEnd.Sub(p.RangeStart).Minutes() -
		report.Totals.ActiveMinutes
	if report.Totals.IdleMinutes < 0 {
		report.Totals.IdleMinutes = 0
	}
	survivors := usage
	if !usageIsSurvivorSet {
		survivors = dedupUsage(p.RangeStart, p.RangeEnd, p.EffectiveEnd, usage)
	}
	allocated := AllocateUsageCosts(survivors)
	if err := applyUsageRows(
		&report, windows, survivors, allocated, automatedBy,
	); err != nil {
		return CandidateArtifacts{}, err
	}
	if err := buildSessionsTableFromDedupedUsage(
		&report, sessions, aggregates, survivors, allocated,
	); err != nil {
		return CandidateArtifacts{}, err
	}
	report.Intervals = []ReportInterval{}
	rows := report.BySession
	report.BySession = []SessionRow{}
	return CandidateArtifacts{
		Report: report, Sessions: rows, Membership: membership,
	}, nil
}

func effectiveCandidateInterval(p Params, candidate IntervalCandidate) (interval, bool) {
	gapCap := time.Duration(p.GapCapSeconds) * time.Second
	start := candidate.Start.Truncate(time.Microsecond)
	end := candidate.End.Truncate(time.Microsecond)
	intervalStart, intervalEnd, effective := EffectiveIntervalBounds(
		start, end, p.RangeStart, p.EffectiveEnd, gapCap,
	)
	if !effective {
		return interval{}, false
	}
	model := candidate.PriorModel
	if candidate.ClosingRole == "assistant" && candidate.ClosingModel != "" {
		model = candidate.ClosingModel
	}
	if model == "" {
		model = "unknown"
	}
	return interval{
		sessionID: candidate.SessionID,
		start:     intervalStart,
		end:       intervalEnd,
		model:     model,
	}, true
}

type activeCandidateEnd struct {
	sessionID string
	end       time.Time
	automated bool
}

type activeCandidateHeap []activeCandidateEnd

func (h activeCandidateHeap) Len() int { return len(h) }
func (h activeCandidateHeap) Less(i, j int) bool {
	return h[i].end.Before(h[j].end)
}
func (h activeCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *activeCandidateHeap) Push(value any) {
	*h = append(*h, value.(activeCandidateEnd))
}
func (h *activeCandidateHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

type candidateSweep struct {
	report       *Report
	windows      []BucketWindow
	effectiveEnd time.Time
	automatedBy  map[string]bool
	ends         activeCandidateHeap
	sessionEnds  map[string]time.Time
	bucket       int
	last         time.Time
	live         int
	liveAuto     int
	liveInter    int
	active       time.Duration
}

func (s *candidateSweep) advance(target time.Time) {
	for {
		var next time.Time
		if len(s.ends) > 0 && !s.ends[0].end.After(target) {
			next = s.ends[0].end
		}
		if s.bucket < len(s.windows) {
			boundary := s.windows[s.bucket].End
			if !boundary.After(target) && (next.IsZero() || boundary.Before(next)) {
				next = boundary
			}
		}
		if next.IsZero() {
			break
		}
		s.accrue(next)
		for len(s.ends) > 0 && s.ends[0].end.Equal(next) {
			ended := heap.Pop(&s.ends).(activeCandidateEnd)
			currentEnd, ok := s.sessionEnds[ended.sessionID]
			if !ok || !currentEnd.Equal(ended.end) {
				continue
			}
			delete(s.sessionEnds, ended.sessionID)
			s.live--
			if ended.automated {
				s.liveAuto--
			} else {
				s.liveInter--
			}
		}
		for s.bucket < len(s.windows) && s.windows[s.bucket].End.Equal(next) {
			s.bucket++
			if s.bucket < len(s.windows) &&
				s.windows[s.bucket].Start.Before(s.effectiveEnd) {
				s.recordBucketPeak()
			}
		}
	}
	s.accrue(target)
}

func (s *candidateSweep) accrue(target time.Time) {
	if target.After(s.last) && s.live > 0 {
		s.active += target.Sub(s.last)
	}
	if target.After(s.last) {
		s.last = target
	}
}

func (s *candidateSweep) open(iv interval) {
	automated := s.automatedBy[iv.sessionID]
	s.sessionEnds[iv.sessionID] = iv.end
	heap.Push(&s.ends, activeCandidateEnd{
		sessionID: iv.sessionID, end: iv.end, automated: automated,
	})
	s.live++
	if automated {
		s.liveAuto++
	} else {
		s.liveInter++
	}
	if s.live > s.report.Peak.Agents {
		s.report.Peak.Agents = s.live
		at := iv.start.Format(time.RFC3339)
		s.report.Peak.At = &at
	}
	s.recordBucketPeak()
}

func (s *candidateSweep) extend(iv interval) {
	automated := s.automatedBy[iv.sessionID]
	s.sessionEnds[iv.sessionID] = iv.end
	heap.Push(&s.ends, activeCandidateEnd{
		sessionID: iv.sessionID, end: iv.end, automated: automated,
	})
}

func (s *candidateSweep) recordBucketPeak() {
	if s.bucket >= len(s.report.Buckets) ||
		!s.windows[s.bucket].Start.Before(s.effectiveEnd) {
		return
	}
	bucket := &s.report.Buckets[s.bucket]
	if s.live > bucket.MaxAgents {
		bucket.MaxAgents = s.live
		bucket.AutomatedAtPeak = s.liveAuto
		bucket.InteractiveAtPeak = s.liveInter
	}
}

func foldCandidateInterval(
	report *Report,
	windows []BucketWindow,
	membershipWindows []BucketWindow,
	aggregates map[string]*sessionIntervalAgg,
	membership map[string]BucketMembership,
	words int,
	automatedBy map[string]bool,
	iv interval,
) {
	minutes := iv.end.Sub(iv.start).Minutes()
	report.Totals.AgentMinutes += minutes
	if automatedBy[iv.sessionID] {
		report.Totals.AutomatedAgentMinutes += minutes
	} else {
		report.Totals.InteractiveAgentMinutes += minutes
	}
	aggregate := aggregates[iv.sessionID]
	if aggregate == nil {
		aggregate = &sessionIntervalAgg{
			modelMins: make(map[string]float64), modelDuration: make(map[string]time.Duration),
		}
		aggregates[iv.sessionID] = aggregate
	}
	aggregate.minutes += minutes
	aggregate.modelMins[iv.model] += minutes
	aggregate.modelDuration[iv.model] += iv.end.Sub(iv.start)
	if !aggregate.hasIv || iv.start.Before(aggregate.first) {
		aggregate.first = iv.start
	}
	if !aggregate.hasIv || iv.end.After(aggregate.last) {
		aggregate.last = iv.end
	}
	aggregate.hasIv = true

	for index := max(0, windowIndex(windows, iv.start)); index < len(windows) && windows[index].Start.Before(iv.end); index++ {
		lo := maxTime(iv.start, windows[index].Start)
		hi := minTime(iv.end, windows[index].End)
		if hi.After(lo) {
			report.Buckets[index].AgentMinutes += hi.Sub(lo).Minutes()
		}
	}

	bits := membership[iv.sessionID]
	if bits == nil {
		bits = make(BucketMembership, words)
		membership[iv.sessionID] = bits
	}
	startSecond := iv.start.Truncate(time.Second)
	endSecond := iv.end.Truncate(time.Second)
	if startSecond.Equal(endSecond) {
		bits.add(windowIndex(membershipWindows, startSecond))
		return
	}
	for index := max(0, windowIndex(membershipWindows, startSecond)); index < len(membershipWindows) && membershipWindows[index].Start.Before(endSecond); index++ {
		if startSecond.Before(membershipWindows[index].End) && endSecond.After(membershipWindows[index].Start) {
			bits.add(index)
		}
	}
}

// secondPrecisionWindows matches the RFC3339 bucket bounds exposed to clients.
// Membership uses the same wire-level precision as the interval timestamps so
// fractional custom-range anchors cannot shift a session between visible slots.
func secondPrecisionWindows(windows []BucketWindow) []BucketWindow {
	normalized := make([]BucketWindow, len(windows))
	for index, window := range windows {
		normalized[index] = BucketWindow{
			Start: window.Start.Truncate(time.Second),
			End:   window.End.Truncate(time.Second),
		}
	}
	return normalized
}
