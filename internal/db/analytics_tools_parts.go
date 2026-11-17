package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"sort"
	"sync"

	"go.kenn.io/agentsview/internal/kept"
)

const toolsPartsVersion = 1

var toolsPartsObserver struct {
	sync.Mutex
	read func([]string)
}

// SetToolsPartsReadObserver installs an observer for tests and returns its reset function.
func SetToolsPartsReadObserver(observe func([]string)) func() {
	toolsPartsObserver.Lock()
	toolsPartsObserver.read = observe
	toolsPartsObserver.Unlock()
	return func() { toolsPartsObserver.Lock(); toolsPartsObserver.read = nil; toolsPartsObserver.Unlock() }
}

func (db *DB) toolsFromParts(ctx context.Context, identity string, f AnalyticsFilter) (ToolsAnalyticsResponse, bool, error) {
	if f.ActiveSince != "" || TerminationUsesTime(f.Termination) || f.Timezone == "Local" {
		return ToolsAnalyticsResponse{}, false, nil
	}
	sessions, err := db.toolSessions(ctx, f)
	if err != nil {
		return ToolsAnalyticsResponse{}, true, err
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
		toolsPartsObserver.Lock()
		if toolsPartsObserver.read != nil {
			toolsPartsObserver.read(ids)
		}
		toolsPartsObserver.Unlock()
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
	if len(changed) > 0 {
		latest, err := db.toolSessions(ctx, f)
		if err != nil {
			return ToolsAnalyticsResponse{}, true, err
		}
		current := make(map[string]string, len(latest))
		for id, session := range latest {
			current[id] = session.fingerprint
		}
		if !maps.Equal(versions, current) {
			return ToolsAnalyticsResponse{}, false, nil
		}
		saved := make([]kept.Part[[]ToolAnalyticsRow], 0, len(parts))
		for _, part := range parts {
			saved = append(saved, part)
		}
		kept.SaveParts(&db.trendsDisk, slot, version, saved, func(rows []ToolAnalyticsRow) int64 {
			var size int64
			for _, row := range rows {
				size += int64(256 + len(row.SessionID) + len(row.Project) + len(row.Agent) + len(row.ToolName) + len(row.Category) + len(row.Date))
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
