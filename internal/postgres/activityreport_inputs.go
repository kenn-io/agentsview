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
)

// A report owns its maps and slices. Published inputs are immutable; builders
// retain the revision captured before their reads so concurrent writes remain
// visible to the next visit.
type pgActivityInputs struct {
	revision    int64
	transcripts map[string][]pgActivityMessage
	usage       []pgActivityUsageRow
	candidates  map[string]bool
	changed     []string
}

// Retained rows keep only the fields Activity consumes. Repeated values use
// shared handles, and timestamps keep PostgreSQL's microsecond precision.
type pgActivityMessage struct {
	ordinal     int32
	us          int64
	role, model unique.Handle[string]
}

type pgActivityUsageRow struct {
	sessionID, usageSource, model, providerID, agent            unique.Handle[string]
	claudeMessageID, claudeRequestID, sourceUUID, usageDedupKey string
	us, cost, webSearches                                       int64
	input, output, cacheCreate, cacheRead, reasoning            int64
	ordinal, cacheCreate1h                                      int32
	ordinalValid, costValid, copilotReported, pricingTimeAbsent bool
}

func pgCompactActivityUsage(r pgDailyUsageScanRow, handle func(string) unique.Handle[string]) pgActivityUsageRow {
	input, output, create, read, reasoning := pgDailyUsageRowTokens(r)
	// Message cache counts are clamped to MaxPlausibleTokens before narrowing.
	return pgActivityUsageRow{
		sessionID: handle(r.sessionID), usageSource: handle(r.usageSource), model: handle(r.model), providerID: handle(r.providerID), agent: handle(r.agent),
		claudeMessageID: r.claudeMessageID, claudeRequestID: r.claudeRequestID, sourceUUID: r.sourceUUID, usageDedupKey: r.usageDedupKey,
		us: r.ts.Time.UnixMicro(), cost: r.cost.Int64,
		input: int64(input), output: int64(output), cacheCreate: int64(create), cacheRead: int64(read), reasoning: int64(reasoning),
		ordinal: int32(r.messageOrdinal.Int64), cacheCreate1h: int32(pgUsageRowCacheCreation1hTokens(r.usageSource, r.tokenJSON, create)), webSearches: int64(pgUsageRowWebSearchRequests(r.usageSource, r.tokenJSON)),
		ordinalValid: r.messageOrdinal.Valid, costValid: r.cost.Valid, copilotReported: r.costSource == db.CopilotReportedCostSource, pricingTimeAbsent: !r.pricingTS.Valid,
	}
}

func pgActivityInputBytes(e pgActivityInputs) int64 {
	size := int64(unsafe.Sizeof(e))
	shared := map[unique.Handle[string]]struct{}{}
	for id, messages := range e.transcripts {
		size += int64(len(id)) + 80 + int64(cap(messages))*int64(unsafe.Sizeof(pgActivityMessage{}))
		for _, m := range messages {
			shared[m.role], shared[m.model] = struct{}{}, struct{}{}
		}
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
	next := &pgActivityInputs{revision: revision, transcripts: map[string][]pgActivityMessage{}, changed: changed}
	if ok && revision >= after && len(changed) <= max(64, len(previous[0].candidates)/4) {
		old := previous[0]
		dirty := make(map[string]bool, len(changed))
		for _, id := range changed {
			dirty[id] = true
		}
		for id, messages := range old.transcripts {
			if !dirty[id] {
				next.transcripts[id] = messages
			}
		}
		next.usage = old.usage
		next.candidates = old.candidates
	}
	return next, nil
}

// activityTranscriptSource streams uncached sessions through the same reader
// that fills retained inputs. Its callback may mutate the expanded messages.
func (s *Store) activityTranscriptSource(ctx context.Context, ids []string, inputs *pgActivityInputs, yield func(string, []activity.TranscriptMessage)) error {
	missing := ids
	if inputs != nil {
		missing = make([]string, 0, len(ids))
		for _, id := range ids {
			if _, ok := inputs.transcripts[id]; !ok {
				missing = append(missing, id)
			}
		}
	}
	if len(missing) > 0 {
		rows, err := s.pg.QueryContext(ctx, `SELECT session_id, ordinal, role, timestamp, model
 FROM messages WHERE session_id = ANY($1) AND timestamp IS NOT NULL
 ORDER BY session_id, ordinal`, pgx.QueryExecModeDescribeExec, missing)
		if err != nil {
			return fmt.Errorf("querying pg activity report candidates: %w", err)
		}
		defer rows.Close()
		if inputs != nil {
			for _, id := range missing {
				inputs.transcripts[id] = nil
			}
		}
		handles := stringutil.Interner{}
		finish := func(id string, session []activity.TranscriptMessage) {
			if len(session) == 0 {
				return
			}
			if inputs == nil {
				yield(id, session)
			} else {
				compact := make([]pgActivityMessage, len(session))
				for i, m := range session {
					compact[i] = pgActivityMessage{ordinal: int32(m.Ordinal), us: m.At.UnixMicro(), role: handles.Intern(m.Role), model: handles.Intern(m.Model)}
				}
				inputs.transcripts[id] = compact
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
			if err := rows.Scan(&next, &m.Ordinal, &m.Role, &m.At, &m.Model); err != nil {
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
	}
	if inputs != nil {
		var expanded []activity.TranscriptMessage
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			messages := inputs.transcripts[id]
			expanded = slices.Grow(expanded[:0], len(messages))[:len(messages)]
			for i, m := range messages {
				expanded[i] = activity.TranscriptMessage{Ordinal: int(m.ordinal), At: time.UnixMicro(m.us).UTC(), Role: m.role.Value(), Model: m.model.Value()}
			}
			yield(id, expanded)
		}
	}
	return nil
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
		if selected[r.sessionID.Value()] && r.claudeMessageID != "" && r.claudeRequestID != "" {
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
