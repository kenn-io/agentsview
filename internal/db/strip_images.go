package db

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// StripImagesFilter scopes the existing rows changed by image cleanup.
type StripImagesFilter struct {
	Project string
	Before  string
}

// StripImagesProjectReport contains image cleanup totals for one project.
type StripImagesProjectReport struct {
	Project      string `json:"project"`
	Sessions     int    `json:"sessions"`
	Changed      int    `json:"changed"`
	Payloads     int64  `json:"payloads"`
	StoredBytes  int64  `json:"stored_bytes"`
	DecodedBytes int64  `json:"decoded_bytes"`
}

// StripImagesReport separates stored column bytes from decoded image bytes.
type StripImagesReport struct {
	Sessions     int                        `json:"sessions"`
	Changed      int                        `json:"changed"`
	Payloads     int64                      `json:"payloads"`
	StoredBytes  int64                      `json:"stored_bytes"`
	DecodedBytes int64                      `json:"decoded_bytes"`
	Projects     []StripImagesProjectReport `json:"projects"`
}

type stripImageSession struct {
	id      string
	project string
}

// PreviewStripToolImages reports selected stored rows without writing them.
func (db *DB) PreviewStripToolImages(
	ctx context.Context, filter StripImagesFilter,
) (StripImagesReport, error) {
	return db.scanStripToolImages(ctx, filter, nil)
}

// StripToolImages transforms selected sessions one at a time. The caller owns
// the archive write lock before invoking this method.
func (db *DB) StripToolImages(
	ctx context.Context, filter StripImagesFilter,
) (StripImagesReport, error) {
	if err := db.requireWritable(); err != nil {
		return StripImagesReport{}, err
	}
	return db.scanStripToolImages(ctx, filter, func(
		ctx context.Context, session stripImageSession,
	) (bool, ToolImageStats, error) {
		stats, err := db.stripImageStats(ctx, session.id)
		if err != nil {
			return false, ToolImageStats{}, err
		}
		changed, err := db.stripStoredToolResultRows(ctx, session.id)
		if err != nil {
			return false, ToolImageStats{}, fmt.Errorf(
				"stripping tool results for %s: %w", session.id, err,
			)
		}
		return changed, stats, nil
	})
}

// stripStoredToolResultRows projects both stored tool-result tables in one
// transaction. Direct row updates preserve event coordinates and metadata.
func (db *DB) stripStoredToolResultRows(
	ctx context.Context, sessionID string,
) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning tool result strip: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queueGenerationBefore, queueExistedBefore, err := artifactExportGenerationTx(
		tx, sessionID,
	)
	if err != nil {
		return false, err
	}
	var pendingRecallRevocations recallEvidenceRevocationEvents

	type storedToolResultKey struct {
		messageOrdinal int
		callIndex      int
	}
	type contentUpdate struct {
		id      int64
		content string
		length  int
		key     storedToolResultKey
	}
	calls := make([]contentUpdate, 0)
	allCalls := make([]contentUpdate, 0)
	eventContents := make(map[storedToolResultKey][]string)
	callRows, err := tx.QueryContext(ctx, `
		SELECT tc.id, COALESCE(tc.result_content, ''),
		       COALESCE(tc.result_content_length, 0),
		       COALESCE((SELECT ordinal FROM messages WHERE id = tc.message_id), -1),
		       COALESCE(tc.call_index, 0)
		FROM tool_calls tc
		WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading tool calls: %w", err)
	}
	for callRows.Next() {
		var update contentUpdate
		if err := callRows.Scan(
			&update.id, &update.content, &update.length,
			&update.key.messageOrdinal, &update.key.callIndex,
		); err != nil {
			callRows.Close()
			return false, fmt.Errorf("scanning tool call: %w", err)
		}
		allCalls = append(allCalls, update)
		projected, _ := StripToolResultImages(update.content)
		if projected != update.content {
			update.content = projected
			update.length = ResolveResultContentLength(projected, update.length)
			calls = append(calls, update)
		}
	}
	if err := callRows.Err(); err != nil {
		callRows.Close()
		return false, fmt.Errorf("iterating tool calls: %w", err)
	}
	callRows.Close()

	eventRows, err := tx.QueryContext(ctx, `
		SELECT rowid, tool_call_message_ordinal, call_index,
		       COALESCE(content, ''), COALESCE(content_length, 0)
		FROM tool_result_events
		WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading tool result events: %w", err)
	}
	events := make([]contentUpdate, 0)
	for eventRows.Next() {
		var update contentUpdate
		if err := eventRows.Scan(
			&update.id, &update.key.messageOrdinal, &update.key.callIndex,
			&update.content, &update.length,
		); err != nil {
			eventRows.Close()
			return false, fmt.Errorf("scanning tool result event: %w", err)
		}
		projected, _ := StripToolResultImages(update.content)
		eventContents[update.key] = append(
			eventContents[update.key], projected,
		)
		if projected != update.content {
			update.content = projected
			update.length = ResolveResultContentLength(projected, update.length)
			events = append(events, update)
		}
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		return false, fmt.Errorf("iterating tool result events: %w", err)
	}
	eventRows.Close()
	for i := range calls {
		if calls[i].content == "" {
			continue
		}
		eventsForCall := eventContents[calls[i].key]
		if len(eventsForCall) == 1 && calls[i].content == eventsForCall[0] {
			calls[i].length = len(calls[i].content)
			calls[i].content = ""
		}
	}
	callIndexes := make(map[int64]int, len(calls))
	for i := range calls {
		callIndexes[calls[i].id] = i
	}
	for _, call := range allCalls {
		eventsForCall := eventContents[call.key]
		if call.content != "" || len(eventsForCall) != 1 {
			continue
		}
		length := len(eventsForCall[0])
		if call.length == length {
			continue
		}
		call.length = length
		if i, ok := callIndexes[call.id]; ok {
			calls[i] = call
		} else {
			callIndexes[call.id] = len(calls)
			calls = append(calls, call)
		}
	}

	if len(calls) == 0 && len(events) == 0 {
		return false, nil
	}
	for _, update := range calls {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tool_calls
			SET result_content = ?, result_content_length = ?
			WHERE id = ?`, update.content, update.length, update.id); err != nil {
			return false, fmt.Errorf("updating orphaned tool call: %w", err)
		}
	}
	for _, update := range events {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tool_result_events
			SET content = ?, content_length = ?
			WHERE rowid = ?`, update.content, update.length, update.id); err != nil {
			return false, fmt.Errorf("updating orphaned tool result event: %w", err)
		}
	}
	if err := bumpTranscriptRevisionTx(tx, sessionID); err != nil {
		return false, err
	}
	if err := reconcileRecallEvidenceForSessionTx(
		ctx, tx, sessionID, &pendingRecallRevocations,
	); err != nil {
		return false, err
	}
	if err := resetIncrementalMarkerTx(tx, sessionID); err != nil {
		return false, err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return false, err
	}
	if err := replaceSecretFindingsTx(tx, sessionID, nil, 0, ""); err != nil {
		return false, err
	}
	if err := invalidateSessionSignalsTx(tx, sessionID); err != nil {
		return false, err
	}
	if err := enqueueArtifactExportIfGenerationUnchangedTx(
		tx, sessionID, queueGenerationBefore, queueExistedBefore,
	); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing tool result strip: %w", err)
	}
	db.notifyUsageSessions([]string{sessionID})
	pendingRecallRevocations.flush()
	return true, nil
}

func (db *DB) scanStripToolImages(
	ctx context.Context,
	filter StripImagesFilter,
	apply func(context.Context, stripImageSession) (bool, ToolImageStats, error),
) (StripImagesReport, error) {
	sessions, err := db.stripImageSessions(ctx, filter)
	if err != nil {
		return StripImagesReport{}, err
	}
	report := StripImagesReport{
		Projects: make([]StripImagesProjectReport, 0),
	}
	byProject := make(map[string]*StripImagesProjectReport)
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return StripImagesReport{}, err
		}
		stats, err := db.stripImageStats(ctx, session.id)
		if err != nil {
			return StripImagesReport{}, err
		}
		if stats.Payloads == 0 {
			continue
		}
		project := byProject[session.project]
		if project == nil {
			report.Projects = append(report.Projects, StripImagesProjectReport{
				Project: session.project,
			})
			project = &report.Projects[len(report.Projects)-1]
			byProject[session.project] = project
		}
		project.Sessions++
		report.Sessions++
		if apply != nil {
			changed, _, err := apply(ctx, session)
			if err != nil {
				return StripImagesReport{}, err
			}
			if changed {
				project.Changed++
				report.Changed++
			}
		} else if stats.Payloads > 0 {
			project.Changed++
			report.Changed++
		}
		project.Payloads += stats.Payloads
		project.StoredBytes += stats.StoredBytes
		project.DecodedBytes += stats.DecodedBytes
		report.Payloads += stats.Payloads
		report.StoredBytes += stats.StoredBytes
		report.DecodedBytes += stats.DecodedBytes
	}
	sort.Slice(report.Projects, func(i, j int) bool {
		return report.Projects[i].Project < report.Projects[j].Project
	})
	return report, nil
}

func (db *DB) stripImageSessions(
	ctx context.Context, filter StripImagesFilter,
) ([]stripImageSession, error) {
	if filter.Before != "" {
		if _, err := time.Parse("2006-01-02", filter.Before); err != nil {
			return nil, fmt.Errorf(
				"invalid --before date %q, expected YYYY-MM-DD",
				filter.Before,
			)
		}
	}
	where := `(EXISTS (SELECT 1 FROM tool_calls tc
		WHERE tc.session_id = s.id
		  AND tc.result_content IS NOT NULL
		  AND tc.result_content <> '')
		OR EXISTS (SELECT 1 FROM tool_result_events ev
		WHERE ev.session_id = s.id
		  AND ev.content IS NOT NULL
		  AND ev.content <> ''))`
	args := []any{}
	if filter.Project != "" {
		where += ` AND s.project LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(filter.Project)+"%")
	}
	if filter.Before != "" {
		where += " AND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) < ?"
		args = append(args, filter.Before)
	}
	query := `SELECT s.id, s.project
		FROM sessions s
		WHERE ` + where + `
		ORDER BY s.project, s.id`
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("finding image cleanup sessions: %w", err)
	}
	defer rows.Close()
	var sessions []stripImageSession
	for rows.Next() {
		var session stripImageSession
		if err := rows.Scan(&session.id, &session.project); err != nil {
			return nil, fmt.Errorf("scanning image cleanup session: %w", err)
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (db *DB) stripImageStats(
	ctx context.Context, sessionID string,
) (ToolImageStats, error) {
	var stats ToolImageStats
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT COALESCE(result_content, '')
		FROM tool_calls
		WHERE session_id = ?
		UNION ALL
		SELECT COALESCE(content, '')
		FROM tool_result_events
		WHERE session_id = ?`, sessionID, sessionID)
	if err != nil {
		return ToolImageStats{}, fmt.Errorf("reading tool result bytes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return ToolImageStats{}, fmt.Errorf("scanning tool result bytes: %w", err)
		}
		_, found := StripToolResultImages(content)
		stats.Payloads += found.Payloads
		stats.StoredBytes += found.StoredBytes
		stats.DecodedBytes += found.DecodedBytes
	}
	if err := rows.Err(); err != nil {
		return ToolImageStats{}, fmt.Errorf("iterating tool result bytes: %w", err)
	}
	return stats, nil
}
