package postgres

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"
	"unique"
	"unsafe"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/stringutil"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// A report owns its maps and slices. Published inputs are immutable; builders
// retain the revision captured before their reads so concurrent writes remain
// visible to the next visit.
type pgActivityInputs struct {
	revision int64
	// sessions holds each session's activity, derived at gapCap.
	sessions   map[string]pgActivitySession
	gapCap     float64
	usage      []pgActivityUsageRow
	candidates map[string]bool
	changed    []string
}

// pgActivitySession is one session's activity over all time: its runs and
// the ascending Unix microsecond times of its counted user and assistant
// messages. None of it depends on the report range.
type pgActivitySession struct {
	runs              activity.SessionRuns
	users, assistants []int64
}

type pgActivityUsageRow struct {
	sessionID, usageSource, model, providerID, agent            unique.Handle[string]
	claudeMessageID, claudeRequestID, sourceUUID, usageDedupKey string
	us, cost, webSearches                                       int64
	input, output, cacheCreate, cacheRead, reasoning            int64
	ordinal, cacheCreate1h                                      int32
	ordinalValid, costValid, copilotReported, pricingTimeAbsent bool
	// costRecorded marks a harness-recorded cost, which prices the row only
	// when the catalog cannot (see db.ReportedCostForPricing).
	costRecorded bool
}

func pgCompactActivityUsage(r pgDailyUsageScanRow, handle func(string) unique.Handle[string]) pgActivityUsageRow {
	input, output, create, read, reasoning := pgDailyUsageRowTokens(r)
	// Both sources enforce int32 ordinals in messages.ordinal and usage_events.message_ordinal (PostgreSQL INT).
	// Message cache counts are clamped to MaxPlausibleTokens before narrowing.
	return pgActivityUsageRow{
		sessionID: handle(r.sessionID), usageSource: handle(r.usageSource), model: handle(r.model), providerID: handle(r.providerID), agent: handle(r.agent),
		claudeMessageID: r.claudeMessageID, claudeRequestID: r.claudeRequestID, sourceUUID: r.sourceUUID, usageDedupKey: r.usageDedupKey,
		us: r.ts.Time.UnixMicro(), cost: r.cost.Int64,
		input: int64(input), output: int64(output), cacheCreate: int64(create), cacheRead: int64(read), reasoning: int64(reasoning),
		ordinal: int32(r.messageOrdinal.Int64), cacheCreate1h: int32(pgUsageRowCacheCreation1hTokens(r.usageSource, r.tokenJSON, create)), webSearches: int64(pgUsageRowWebSearchRequests(r.usageSource, r.tokenJSON)),
		ordinalValid: r.messageOrdinal.Valid, costValid: r.cost.Valid, copilotReported: r.costSource == db.CopilotReportedCostSource, pricingTimeAbsent: !r.pricingTS.Valid,
		costRecorded: r.costStatus == usagefacts.CostStatusRecorded,
	}
}

func pgActivityInputBytes(e pgActivityInputs) int64 {
	size := int64(unsafe.Sizeof(e))
	shared := map[unique.Handle[string]]struct{}{}
	for id, session := range e.sessions {
		size += int64(len(id)) + 80 + int64(unsafe.Sizeof(session)) + session.runs.Bytes() +
			8*int64(cap(session.users)+cap(session.assistants))
	}
	size += int64(cap(e.usage)) * int64(unsafe.Sizeof(pgActivityUsageRow{}))
	for _, r := range e.usage {
		size += int64(len(r.claudeMessageID) + len(r.claudeRequestID) + len(r.sourceUUID) + len(r.usageDedupKey))
		for _, h := range [...]unique.Handle[string]{r.sessionID, r.usageSource, r.model, r.providerID, r.agent} {
			shared[h] = struct{}{}
		}
	}
	for h := range shared {
		// Include canonical headers, weak references, cleanup records, and table storage.
		size += int64(len(h.Value())) + 192
	}
	for id := range e.candidates {
		size += int64(len(id)) + 64
	}
	for _, id := range e.changed {
		size += int64(len(id)) + 16
	}
	return size
}

func (s *Store) activityReportInputs(ctx context.Context, version, lower, upper string) (*pgActivityInputs, error) {
	previous, ok := s.runtimeState().activityInputs.Get(lower+"|"+upper, version)
	after := int64(math.MaxInt64)
	if ok {
		after = previous[0].revision
	}
	revision, changed, err := s.LoadUsageChanges(ctx, after)
	if err != nil || revision == 0 {
		s.runtimeState().activityInputs.DeleteSlots(func(slot string) bool { return slot == lower+"|"+upper })
		return nil, err
	}
	next := &pgActivityInputs{revision: revision, sessions: map[string]pgActivitySession{}, changed: changed}
	if ok && revision >= after && len(changed) <= max(64, len(previous[0].candidates)/4) {
		old := previous[0]
		dirty := make(map[string]bool, len(changed))
		for _, id := range changed {
			dirty[id] = true
		}
		next.gapCap = old.gapCap
		for id, session := range old.sessions {
			if !dirty[id] {
				next.sessions[id] = session
			}
		}
		next.usage = old.usage
		next.candidates = old.candidates
	}
	return next, nil
}

// activityTranscriptSource streams the sessions' timestamped messages in
// ordinal order. Its callback may mutate the messages.
func (s *Store) activityTranscriptSource(ctx context.Context, ids []string, yield func(string, []activity.TranscriptMessage)) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT session_id, ordinal, role, timestamp, model,
 role IN ('user', 'assistant') AND is_system = false
 AND source_subtype <> 'tool_result'
 FROM messages WHERE session_id = ANY($1) AND timestamp IS NOT NULL
 ORDER BY session_id, ordinal`, pgx.QueryExecModeDescribeExec, ids)
	if err != nil {
		return fmt.Errorf("querying pg activity report candidates: %w", err)
	}
	defer rows.Close()
	finish := func(id string, session []activity.TranscriptMessage) {
		if len(session) > 0 {
			yield(id, session)
		}
	}
	var id string
	var session []activity.TranscriptMessage
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var next string
		var m activity.TranscriptMessage
		if err := rows.Scan(&next, &m.Ordinal, &m.Role, &m.At, &m.Model, &m.Counted); err != nil {
			return fmt.Errorf("scanning pg activity report message: %w", err)
		}
		if id != next {
			finish(id, session)
			id, session = next, session[:0]
		}
		m.At = m.At.UTC()
		session = append(session, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	finish(id, session)
	return nil
}

// pgActivityAllTime bounds a pairing that leaves out no candidate. The
// candidates a report range leaves out of its pairing are ones the
// aggregator rejects for that range anyway.
var pgActivityAllTime = activity.Query{
	RangeStart:   time.Date(2, time.January, 1, 0, 0, 0, 0, time.UTC),
	EffectiveEnd: time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC),
}

// activityReportRuns returns the sessions' runs clipped to q, in start
// order. It derives the sessions inputs does not hold from one pairing scan
// over all time and adds them to inputs.
func (s *Store) activityReportRuns(
	ctx context.Context, ids []string, q activity.Query, inputs *pgActivityInputs,
) ([]activity.Run, error) {
	if inputs.gapCap != q.GapCapSeconds {
		inputs.sessions, inputs.gapCap = map[string]pgActivitySession{}, q.GapCapSeconds
	}
	var missing []string
	for _, id := range ids {
		if _, ok := inputs.sessions[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		all := pgActivityAllTime
		all.GapCapSeconds = q.GapCapSeconds
		terminal, pairs, counted, err := s.readActivityCandidates(ctx, missing, all, true)
		if err != nil {
			return nil, err
		}
		derived, err := activity.DeriveSessionRunsInParallel(ctx, missing, q.GapCapSeconds,
			activity.GroupSessionCandidates(missing, pairs, terminal))
		if err != nil {
			return nil, err
		}
		index := make(map[string]int, len(missing))
		sessions := make([]pgActivitySession, len(missing))
		for i, id := range missing {
			index[id] = i
			sessions[i].runs = derived[id]
		}
		for _, m := range counted {
			if i, ok := index[m.sessionID]; ok {
				if m.user {
					sessions[i].users = append(sessions[i].users, m.timestamp)
				} else {
					sessions[i].assistants = append(sessions[i].assistants, m.timestamp)
				}
			}
		}
		for i, id := range missing {
			session := sessions[i]
			slices.Sort(session.users)
			slices.Sort(session.assistants)
			session.users, session.assistants = slices.Clip(session.users), slices.Clip(session.assistants)
			inputs.sessions[id] = session
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The candidate stream leaves out candidates that start at or after the
	// effective end at PostgreSQL's microsecond precision.
	before := time.UnixMicro(q.EffectiveEnd.UnixMicro()).UTC()
	n := 0
	for _, id := range ids {
		n += inputs.sessions[id].runs.Runs()
	}
	runs := make([]activity.Run, 0, n)
	for _, id := range ids {
		runs = inputs.sessions[id].runs.AppendClippedBefore(runs, id, q.RangeStart, q.EffectiveEnd, before)
	}
	activity.SortRuns(runs)
	return runs, nil
}

func (s *Store) readActivityUsageInputs(ctx context.Context, ids []string, lowerBound, upperBound string) ([]pgActivityUsageRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pb := &paramBuilder{}
	candidateSessions := pb.add(ids)
	peerSessions := pb.add(ids)
	peerLower := pb.add(lowerBound)
	peerUpper := pb.add(upperBound)
	eventSessions := pb.add(ids)
	lower := pb.add(lowerBound)
	upper := pb.add(upperBound)
	rowsSQL := pgDailyUsageRowsSQLWithWhere(
		"("+pgUsageMessageEligibility+" AND m.session_id = ANY("+candidateSessions+") OR ("+
			activityReportClaudePeerPredicate(peerSessions, peerLower, peerUpper)+"))",
		pgUsageEventEligibility+" AND ue.session_id = ANY("+eventSessions+")")
	query := pgDailyUsageRowSelectFromRows(rowsSQL) + ` AND u.ts >= ` + lower + `::timestamptz AND u.ts <= ` + upper + `::timestamptz`
	rows, err := s.pg.QueryContext(ctx, query, append([]any{pgx.QueryExecModeDescribeExec}, pb.args...)...)
	if err != nil {
		return nil, fmt.Errorf("querying activity report usage: %w", err)
	}
	defer rows.Close()
	handles := stringutil.Interner{}
	var out []pgActivityUsageRow
	for rows.Next() {
		r, err := scanPGDailyUsageRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning activity report usage: %w", err)
		}
		out = append(out, pgCompactActivityUsage(r, handles.Intern))
	}
	return out, rows.Err()
}

func (s *Store) activityUsageInputs(ctx context.Context, ids []string, lower, upper string, inputs *pgActivityInputs) ([]pgActivityUsageRow, error) {
	if inputs == nil {
		return s.readActivityUsageInputs(ctx, ids, lower, upper)
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	if len(inputs.changed) == 0 && maps.Equal(selected, inputs.candidates) {
		return inputs.usage, nil
	}
	read := ids
	if inputs.candidates != nil {
		dirty := make(map[string]bool, len(inputs.changed))
		for _, id := range inputs.changed {
			dirty[id] = true
		}
		for _, id := range ids {
			if !inputs.candidates[id] {
				dirty[id] = true
			}
		}
		read = slices.Sorted(maps.Keys(dirty))
	}
	fresh, err := s.readActivityUsageInputs(ctx, read, lower, upper)
	if err != nil {
		return nil, err
	}
	if inputs.candidates == nil {
		inputs.usage = slices.Clip(slices.Clone(fresh))
		inputs.candidates = selected
		inputs.changed = nil
		return inputs.usage, nil
	}
	replaced := make(map[string]bool, len(read))
	for _, id := range read {
		replaced[id] = true
	}
	// Fresh reads can return unchanged snapshot peers. Remove their prior copies
	// while preserving multiple identical rows within the fresh result itself.
	freshRows := make(map[pgActivityUsageRow]bool, len(fresh))
	for _, r := range fresh {
		freshRows[r] = true
	}
	merged := make([]pgActivityUsageRow, 0, len(inputs.usage)+len(fresh))
	for _, r := range inputs.usage {
		if !replaced[r.sessionID.Value()] && !freshRows[r] {
			merged = append(merged, r)
		}
	}
	merged = append(merged, fresh...)
	type key struct{ message, request string }
	keys := map[key]bool{}
	for _, r := range merged {
		if selected[r.sessionID.Value()] && r.claudeMessageID != "" {
			keys[key{r.claudeMessageID, r.claudeRequestID}] = true
		}
	}
	merged = slices.DeleteFunc(merged, func(r pgActivityUsageRow) bool {
		return !selected[r.sessionID.Value()] && !keys[key{r.claudeMessageID, r.claudeRequestID}]
	})
	inputs.usage = slices.Clip(slices.Clone(merged))
	inputs.candidates = selected
	inputs.changed = nil
	return merged, nil
}

// activityMessageSliceSessions is the fewest sessions worth a message read
// of their own; see readActivityCandidates.
const activityMessageSliceSessions = 4096

// activityMessageSlices caps the message reads one pairing runs at once.
// The terminal read and the report's usage read run beside them on the
// pool of 5 connections, which leaves one for other requests.
const activityMessageSlices = 2

// readActivityMessagePairs pairs the messages of the sessions in ids, one
// session at a time, so each session's pairs sit together in the result.
// With withCounts, it also returns each counted message from the range
// start to the effective end.
func (s *Store) readActivityMessagePairs(
	ctx context.Context, ids []string, q activity.Query, withCounts bool,
) ([]activity.IntervalCandidate, []pgActivityCountedMessage, error) {
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
	// pgx truncates SQL timestamp bounds to microseconds.
	lowerMicro, upperMicro := lower.UnixMicro(), q.EffectiveEnd.UnixMicro()
	countLowerMicro := q.RangeStart.UnixMicro()
	var pairs []activity.IntervalCandidate
	var counted []pgActivityCountedMessage
	err := s.activityTranscriptSource(ctx, ids, func(id string, session []activity.TranscriptMessage) {
		for i := range session {
			us := session[i].At.UnixMicro()
			if withCounts && session[i].Counted && us >= countLowerMicro && us < upperMicro {
				counted = append(counted, pgActivityCountedMessage{id, us, session[i].Role == "user"})
			}
			session[i].Starts = us >= lowerMicro && us < upperMicro
		}
		pairs = activity.AppendTranscriptPairs(pairs, id, session)
	})
	if err != nil {
		return nil, nil, err
	}
	return pairs, counted, nil
}
