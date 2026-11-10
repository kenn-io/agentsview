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

// Intersects reports whether the session belongs to any bucket in the
// half-open range [start, end).
func (m BucketMembership) Intersects(start, end int) bool {
	if start < 0 || end <= start || start/64 >= len(m) {
		return false
	}
	last := end - 1
	firstWord := start / 64
	lastWord := min(last/64, len(m)-1)
	for word := firstWord; word <= lastWord; word++ {
		mask := ^uint64(0)
		if word == firstWord {
			mask &= ^uint64(0) << uint(start%64)
		}
		if word == lastWord && last%64 != 63 {
			mask &= (uint64(1) << uint(last%64+1)) - 1
		}
		if m[word]&mask != 0 {
			return true
		}
	}
	return false
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
			for next < len(extra) && CandidateLess(extra[next], candidate) {
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

// CandidateLess orders candidates by microsecond start, session, and ordinal.
func CandidateLess(a, b IntervalCandidate) bool {
	if a.Start.UnixMicro() != b.Start.UnixMicro() {
		return a.Start.UnixMicro() < b.Start.UnixMicro()
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
	sort.Slice(out, func(i, j int) bool { return CandidateLess(out[i], out[j]) })
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
		ctx, p, sessions, source, func() ([]UsageRow, error) { return usage, nil }, false, nil,
	)
}

// BuildCandidateArtifactsFromSourceWithSurvivorUsage is the store-facing
// variant for usage rows that have already passed the shared survivor
// selection. It loads usage after the candidate stream ends and skips a second
// deduplication pass.
func BuildCandidateArtifactsFromSourceWithSurvivorUsage(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	usage func() ([]UsageRow, error),
) (CandidateArtifacts, error) {
	return buildCandidateArtifactsFromSource(
		ctx, p, sessions, source, usage, true, nil,
	)
}

func buildCandidateArtifactsFromSource(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	source CandidateSource,
	loadUsage func() ([]UsageRow, error),
	usageIsSurvivorSet bool,
	joint *jointActivityAccumulator,
) (CandidateArtifacts, error) {
	return buildIntervalArtifacts(ctx, p, sessions, func(yield func(interval) error) error {
		return source(ctx, func(candidate IntervalCandidate) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			iv, effective := effectiveCandidateInterval(p, candidate)
			if !effective {
				return nil
			}
			return yield(iv)
		})
	}, loadUsage, usageIsSurvivorSet, joint)
}

// buildIntervalArtifacts sweeps intervals given in start order. A
// session's interval that overlaps its previous one keeps only the part
// after it.
func buildIntervalArtifacts(
	ctx context.Context,
	p Params,
	sessions []SessionMeta,
	each func(yield func(interval) error) error,
	loadUsage func() ([]UsageRow, error),
	usageIsSurvivorSet bool,
	joint *jointActivityAccumulator,
) (CandidateArtifacts, error) {
	windows := rangeWindows(p)
	report := newReport(p, windows)
	report.Buckets = make([]Bucket, len(windows))
	for i, window := range windows {
		report.Buckets[i] = Bucket{
			Start: window.Start.Format(time.RFC3339),
			End:   window.End.Format(time.RFC3339),
		}
	}
	kindBy := sessionKinds(sessions)
	state := candidateSweep{
		report: &report, windows: windows, effectiveEnd: p.EffectiveEnd,
		last: p.RangeStart, kindBy: kindBy,
		sessionEnds: make(map[string]time.Time),
	}
	heap.Init(&state.ends)
	fold := intervalFold{
		windows: windows, membershipWindows: secondPrecisionWindows(windows),
		kindBy: kindBy, words: (len(windows) + 63) / 64,
		aggregates: make(map[string]*sessionIntervalAgg),
		membership: make(map[string]BucketMembership),
		buckets:    make([]minuteSum, len(windows)),
	}
	var previousStart time.Time
	err := each(func(iv interval) error {
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
		if iv.tail.Before(iv.start) {
			iv.tail = iv.start
		}
		fold.add(iv)
		if joint != nil {
			joint.add(iv)
		}
		return nil
	})
	if err != nil {
		return CandidateArtifacts{}, err
	}
	state.advance(p.EffectiveEnd)
	fold.finish(&report)
	usage, err := loadUsage()
	if err != nil {
		return CandidateArtifacts{}, err
	}
	report.Totals.ActiveMinutes = state.active.Minutes()
	report.Totals.IdleMinutes = p.EffectiveEnd.Sub(p.RangeStart).Minutes() -
		report.Totals.ActiveMinutes
	if report.Totals.IdleMinutes < 0 {
		report.Totals.IdleMinutes = 0
	}
	survivors := usage
	if !usageIsSurvivorSet {
		survivors, err = dedupUsage(ctx, p.RangeStart, p.RangeEnd, p.EffectiveEnd, usage)
		if err != nil {
			return CandidateArtifacts{}, err
		}
	}
	allocated, err := AllocateUsageCostsContext(ctx, survivors)
	if err != nil {
		return CandidateArtifacts{}, err
	}
	if err := applyUsageRows(
		&report, windows, survivors, allocated, kindBy,
	); err != nil {
		return CandidateArtifacts{}, err
	}
	if err := buildSessionsTableFromDedupedUsage(
		&report, sessions, fold.aggregates, survivors, allocated,
	); err != nil {
		return CandidateArtifacts{}, err
	}
	report.Intervals = []ReportInterval{}
	rows := report.BySession
	report.BySession = []SessionRow{}
	return CandidateArtifacts{
		Report: report, Sessions: rows, Membership: fold.membership,
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
	return interval{
		sessionID: candidate.SessionID,
		start:     intervalStart,
		end:       intervalEnd,
		tail:      intervalStart,
		model:     candidateModel(candidate),
	}, true
}

// candidateModel is the model a candidate's interval is attributed to.
func candidateModel(candidate IntervalCandidate) string {
	model := candidate.PriorModel
	if candidate.ClosingRole == "assistant" && candidate.ClosingModel != "" {
		model = candidate.ClosingModel
	}
	if model == "" {
		model = "unknown"
	}
	return model
}

// minuteSum adds durations exactly and converts the total to minutes once,
// so the minutes do not depend on the order of the intervals or on how a
// stretch of activity was split into them. Whole seconds and the remaining
// nanoseconds are kept apart, so long ranges with many sessions cannot
// overflow. It assumes non-negative durations.
type minuteSum struct{ sec, nsec int64 }

func (m *minuteSum) add(d time.Duration) {
	m.sec += int64(d / time.Second)
	m.nsec += int64(d % time.Second)
	if m.nsec >= int64(time.Second) {
		m.sec++
		m.nsec -= int64(time.Second)
	}
}

// minutes matches time.Duration.Minutes for totals a Duration can hold.
func (m *minuteSum) minutes() float64 {
	rest := (m.sec%60)*int64(time.Second) + m.nsec
	return float64(m.sec/60) + float64(rest)/(60*1e9)
}

type activeCandidateEnd struct {
	sessionID string
	end       time.Time
	kind      sessionKind
}

type activeCandidateHeap []activeCandidateEnd

func (h *activeCandidateHeap) Len() int { return len(*h) }
func (h *activeCandidateHeap) Less(i, j int) bool {
	return (*h)[i].end.Before((*h)[j].end)
}
func (h *activeCandidateHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
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
	kindBy       map[string]sessionKind
	ends         activeCandidateHeap
	sessionEnds  map[string]time.Time
	bucket       int
	last         time.Time
	live         int
	liveAuto     int
	liveInter    int
	liveSub      int
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
			switch ended.kind {
			case subagentSession:
				s.liveSub--
			case automatedSession:
				s.liveAuto--
			default:
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
	kind := s.kindBy[iv.sessionID]
	s.sessionEnds[iv.sessionID] = iv.end
	heap.Push(&s.ends, activeCandidateEnd{
		sessionID: iv.sessionID, end: iv.end, kind: kind,
	})
	s.live++
	switch kind {
	case subagentSession:
		s.liveSub++
	case automatedSession:
		s.liveAuto++
	default:
		s.liveInter++
	}
	recordPeak(&s.report.Peak, s.live, iv.start)
	recordPeak(&s.report.InteractivePeak, s.liveInter, iv.start)
	recordPeak(&s.report.SubagentPeak, s.liveSub, iv.start)
	recordPeak(&s.report.AutomatedPeak, s.liveAuto, iv.start)
	s.recordBucketPeak()
}

func recordPeak(peak *Peak, count int, at time.Time) {
	if count > peak.Agents {
		peak.Agents = count
		ts := at.Format(time.RFC3339)
		peak.At = &ts
	}
}

func (s *candidateSweep) extend(iv interval) {
	kind := s.kindBy[iv.sessionID]
	s.sessionEnds[iv.sessionID] = iv.end
	heap.Push(&s.ends, activeCandidateEnd{
		sessionID: iv.sessionID, end: iv.end, kind: kind,
	})
}

func (s *candidateSweep) recordBucketPeak() {
	if s.bucket >= len(s.report.Buckets) ||
		!s.windows[s.bucket].Start.Before(s.effectiveEnd) {
		return
	}
	bucket := &s.report.Buckets[s.bucket]
	bucket.MaxInteractiveAgents = max(bucket.MaxInteractiveAgents, s.liveInter)
	bucket.MaxSubagentAgents = max(bucket.MaxSubagentAgents, s.liveSub)
	bucket.MaxAutomatedAgents = max(bucket.MaxAutomatedAgents, s.liveAuto)
	if s.live > bucket.MaxAgents {
		bucket.MaxAgents = s.live
		bucket.AutomatedAtPeak = s.liveAuto
		bucket.InteractiveAtPeak = s.liveInter
		bucket.SubagentAtPeak = s.liveSub
	}
}

// intervalFold adds each swept interval to the report's minutes, the
// per-session aggregates, and bucket membership.
type intervalFold struct {
	windows           []BucketWindow
	membershipWindows []BucketWindow
	kindBy            map[string]sessionKind
	words             int
	aggregates        map[string]*sessionIntervalAgg
	membership        map[string]BucketMembership
	total, subagent   minuteSum
	automated, inter  minuteSum
	buckets           []minuteSum
}

func (f *intervalFold) add(iv interval) {
	duration := iv.end.Sub(iv.start)
	f.total.add(duration)
	switch f.kindBy[iv.sessionID] {
	case subagentSession:
		f.subagent.add(duration)
	case automatedSession:
		f.automated.add(duration)
	default:
		f.inter.add(duration)
	}
	aggregate := f.aggregates[iv.sessionID]
	if aggregate == nil {
		aggregate = &sessionIntervalAgg{modelDuration: make(map[string]time.Duration)}
		f.aggregates[iv.sessionID] = aggregate
	}
	aggregate.duration += duration
	aggregate.modelDuration[iv.model] += duration
	if !aggregate.hasIv || iv.start.Before(aggregate.first) {
		aggregate.first = iv.start
	}
	if !aggregate.hasIv || iv.end.After(aggregate.last) {
		aggregate.last = iv.end
	}
	aggregate.hasIv = true

	windows := f.windows
	for index := max(0, windowIndex(windows, iv.start)); index < len(windows) && windows[index].Start.Before(iv.end); index++ {
		lo := maxTime(iv.start, windows[index].Start)
		hi := minTime(iv.end, windows[index].End)
		if hi.After(lo) {
			f.buckets[index].add(hi.Sub(lo))
		}
	}

	bits := f.membership[iv.sessionID]
	if bits == nil {
		bits = make(BucketMembership, f.words)
		f.membership[iv.sessionID] = bits
	}
	// Membership is a union over the pieces an interval joins. A piece
	// within one second adds the bucket holding that second, even when it
	// is the first second of a bucket the half-open span below misses.
	membershipWindows := f.membershipWindows
	startSecond := iv.start.Truncate(time.Second)
	endSecond := iv.end.Truncate(time.Second)
	if iv.tail.Truncate(time.Second).Equal(endSecond) {
		bits.add(windowIndex(membershipWindows, endSecond))
	}
	for index := max(0, windowIndex(membershipWindows, startSecond)); index < len(membershipWindows) && membershipWindows[index].Start.Before(endSecond); index++ {
		if startSecond.Before(membershipWindows[index].End) && endSecond.After(membershipWindows[index].Start) {
			bits.add(index)
		}
	}
}

// finish writes the summed minutes into the report.
func (f *intervalFold) finish(report *Report) {
	report.Totals.AgentMinutes = f.total.minutes()
	report.Totals.SubagentAgentMinutes = f.subagent.minutes()
	report.Totals.AutomatedAgentMinutes = f.automated.minutes()
	report.Totals.InteractiveAgentMinutes = f.inter.minutes()
	for i := range f.buckets {
		report.Buckets[i].AgentMinutes = f.buckets[i].minutes()
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

// TranscriptMessage is one parseable message of a session. Starts marks a
// message inside the pairing window, which may start a pair.
type TranscriptMessage struct {
	Ordinal     int
	At          time.Time
	Role, Model string
	Starts      bool
	// Counted is storage eligibility, independent of the report window.
	Counted bool
}

// AppendTranscriptPairs pairs each window message of one session with
// the next parseable one. The prior model is the latest assistant model whose
// message is later than the message before it, up to and including the start.
// Times compare at microseconds, as the reports order them.
func AppendTranscriptPairs(
	out []IntervalCandidate, sessionID string, ms []TranscriptMessage,
) []IntervalCandidate {
	prior := "unknown"
	for i, m := range ms {
		if i > 0 && m.Role == "assistant" && m.Model != "" &&
			m.At.UnixMicro() > ms[i-1].At.UnixMicro() {
			prior = m.Model
		}
		if !m.Starts || i+1 == len(ms) {
			continue
		}
		next := ms[i+1]
		out = append(out, IntervalCandidate{
			SessionID: sessionID, StartOrdinal: m.Ordinal, EndOrdinal: next.Ordinal,
			Start: m.At, End: next.At, ClosingRole: next.Role,
			ClosingModel: next.Model, PriorModel: prior,
		})
	}
	return out
}
