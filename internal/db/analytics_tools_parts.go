package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/kept"
)

// Increment when the stored ToolAnalyticsRow shape or meaning changes.
const toolsPartsVersion = 2

var toolsPartsReadObserver func([]string)
var toolsPartsAfterSessionsObserver func()
var toolsPartsBeforeValidationObserver func()

func (db *DB) toolsFromParts(ctx context.Context, identity string, f AnalyticsFilter) (ToolsAnalyticsResponse, bool, error) {
	// These tokens cover session selection and Tools inputs during the fill.
	readTokens := func() ([3]string, error) {
		var tokens [3]string
		err := db.getReader().QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_common'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_tools'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_scope'), '')`).Scan(&tokens[0], &tokens[1], &tokens[2])
		return tokens, err
	}
	before, err := readTokens()
	if err != nil || before[0] == "" || before[1] == "" || before[2] == "" {
		return ToolsAnalyticsResponse{}, false, nil
	}
	sessions, err := db.toolSessions(ctx, f, true)
	if err != nil {
		return ToolsAnalyticsResponse{}, false, nil
	}
	if toolsPartsAfterSessionsObserver != nil {
		toolsPartsAfterSessionsObserver()
	}
	var dirty []string
	for id, session := range sessions {
		if session.toolsGeneration == nil {
			dirty = append(dirty, id)
		}
	}
	if len(dirty) > 0 {
		fingerprints, err := db.stampToolsSessions(ctx, dirty)
		if err != nil || len(fingerprints) != len(dirty) {
			return ToolsAnalyticsResponse{}, false, nil
		}
		for _, id := range dirty {
			session := sessions[id]
			session.fingerprint = fingerprints[id]
			sessions[id] = session
		}
	}
	versions := make(map[string]string, len(sessions))
	for id, session := range sessions {
		versions[id] = session.fingerprint
	}
	key, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return ToolsAnalyticsResponse{}, true, err
	}
	slot := "tools-parts|" + TrendsTimezoneKey(f) + "|" + string(key)
	version := fmt.Sprintf("%s|%d", identity, toolsPartsVersion)
	parts := kept.LoadParts[[]ToolAnalyticsRow](&db.trendsDisk, slot, version)
	changed := kept.StaleParts(parts, versions)
	ids := make([]string, 0, len(changed))
	for id := range changed {
		delete(parts, id)
		if _, ok := versions[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		if toolsPartsReadObserver != nil {
			toolsPartsReadObserver(ids)
		}
		rows, err := db.toolRowsFor(ctx, f, sessions, ids)
		if err != nil {
			return ToolsAnalyticsResponse{}, true, err
		}
		for _, id := range ids {
			parts[id] = kept.Part[[]ToolAnalyticsRow]{ID: id, Fingerprint: versions[id]}
		}
		for _, row := range rows {
			part := parts[row.SessionID]
			part.Value = append(part.Value, row)
			parts[row.SessionID] = part
		}
	}
	if toolsPartsBeforeValidationObserver != nil {
		toolsPartsBeforeValidationObserver()
	}
	latest, err := readTokens()
	if err != nil || before != latest {
		return ToolsAnalyticsResponse{}, false, nil
	}
	if len(changed) > 0 {
		saved := make([]kept.Part[[]ToolAnalyticsRow], 0, len(parts))
		for _, part := range parts {
			for i := range part.Value {
				part.Value[i].Project = ""
				part.Value[i].Ordinal = nil
			}
			saved = append(saved, part)
		}
		kept.SaveParts(&db.trendsDisk, slot, version, saved, func(rows []ToolAnalyticsRow) int64 {
			var size int64
			for _, row := range rows {
				size += int64(256 + len(row.SessionID) + len(row.Agent) + len(row.ToolName) + len(row.Category) + len(row.Date))
			}
			return size
		})
	}
	ids = ids[:0]
	for id := range versions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var rows []ToolAnalyticsRow
	for _, id := range ids {
		rows = append(rows, parts[id].Value...)
	}
	return BuildToolsAnalytics(rows), true, nil
}

// stampToolsSessions assigns a fresh token only to selected dirty sessions.
func (db *DB) stampToolsSessions(ctx context.Context, ids []string) (map[string]string, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET tools_generation=lower(hex(randomblob(16))) WHERE id IN (SELECT value FROM json_each(?)) AND tools_generation IS NULL`, string(encoded)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, `+toolsSessionFingerprintSQL+` FROM sessions WHERE id IN (SELECT value FROM json_each(?)) AND tools_generation IS NOT NULL`, string(encoded))
	if err != nil {
		return nil, err
	}
	fingerprints := make(map[string]string, len(ids))
	for rows.Next() {
		var id, fingerprint string
		if err := rows.Scan(&id, &fingerprint); err != nil {
			_ = rows.Close()
			return nil, err
		}
		fingerprints[id] = fingerprint
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return fingerprints, nil
}
