package postgres

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/agentsview/internal/activity"
)

// A report owns its maps and slices. Published inputs are immutable; builders
// retain the revision captured before their reads so concurrent writes remain
// visible to the next visit.
type pgActivityInputs struct {
	revision    int64
	transcripts map[string][]activity.TranscriptMessage
	usage       []pgDailyUsageScanRow
	candidates  map[string]bool
	changed     []string
}

func pgActivityInputBytes(e pgActivityInputs) int64 {
	size := int64(unsafe.Sizeof(e))
	for id, messages := range e.transcripts {
		size += int64(len(id)) + 80 + int64(cap(messages))*int64(unsafe.Sizeof(activity.TranscriptMessage{}))
		for _, m := range messages {
			size += int64(len(m.Role) + len(m.Model))
		}
	}
	size += int64(cap(e.usage)) * int64(unsafe.Sizeof(pgDailyUsageScanRow{}))
	for _, r := range e.usage {
		size += int64(len(r.sessionID) + len(r.usageSource) + len(r.model) + len(r.providerID) + len(r.tokenJSON) + len(r.costSource) + len(r.claudeMessageID) + len(r.claudeRequestID) + len(r.sourceUUID) + len(r.usageDedupKey) + len(r.project) + len(r.agent) + len(r.machine))
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
	next := &pgActivityInputs{revision: revision, transcripts: map[string][]activity.TranscriptMessage{}, changed: changed}
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
// that fills retained inputs. Its callback may mutate only uncached messages.
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
		rows, err := s.pg.QueryContext(ctx, `SELECT session_id, ordinal, role, timestamp, model,
 role IN ('user', 'assistant') AND is_system = false
 AND COALESCE(source_subtype, '') <> 'tool_result'
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
		finish := func(id string, session []activity.TranscriptMessage) {
			if len(session) == 0 {
				return
			}
			if inputs == nil {
				yield(id, session)
			} else {
				inputs.transcripts[id] = slices.Clip(slices.Clone(session))
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
	}
	if inputs != nil {
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			yield(id, inputs.transcripts[id])
		}
	}
	return nil
}

func (s *Store) readActivityUsageInputs(ctx context.Context, ids []string, lowerBound, upperBound string) ([]pgDailyUsageScanRow, error) {
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
	var out []pgDailyUsageScanRow
	for rows.Next() {
		r, err := scanPGDailyUsageRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning activity report usage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) activityUsageInputs(ctx context.Context, ids []string, lower, upper string, inputs *pgActivityInputs) ([]pgDailyUsageScanRow, error) {
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
	freshRows := make(map[pgDailyUsageScanRow]bool, len(fresh))
	for _, r := range fresh {
		freshRows[r] = true
	}
	merged := make([]pgDailyUsageScanRow, 0, len(inputs.usage)+len(fresh))
	for _, r := range inputs.usage {
		if !replaced[r.sessionID] && !freshRows[r] {
			merged = append(merged, r)
		}
	}
	merged = append(merged, fresh...)
	type key struct{ message, request string }
	keys := map[key]bool{}
	for _, r := range merged {
		if selected[r.sessionID] && r.claudeMessageID != "" && r.claudeRequestID != "" {
			keys[key{r.claudeMessageID, r.claudeRequestID}] = true
		}
	}
	merged = slices.DeleteFunc(merged, func(r pgDailyUsageScanRow) bool {
		return !selected[r.sessionID] && !keys[key{r.claudeMessageID, r.claudeRequestID}]
	})
	inputs.usage = slices.Clip(slices.Clone(merged))
	inputs.candidates = selected
	inputs.changed = nil
	return merged, nil
}
