package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"sort"

	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/parser"
)

const dashboardPartsVersion = 1

var dashboardPartsReadObserver func([]string)
var dashboardPartsBeforeValidationObserver func(context.Context)

type dashboardPart struct {
	Messages, User, Assistant, Thinking, Tools int
	Hours                                      map[int]int
	Velocity                                   SessionVelocity
}

type dashboardSession struct {
	ID, Date, Agent, Fingerprint string
	MessageCount                 int
}
type dashboardPartsResult struct {
	Sessions []dashboardSession
	Parts    map[string]kept.Part[dashboardPart]
	Handled  bool
}

func (db *DB) dashboardSessions(ctx context.Context, f AnalyticsFilter) ([]dashboardSession, map[string]string, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(s.started_at, ''), s.created_at)"
	where, args := f.buildWhereWithDate(dateCol, true, "s.id")
	rows, err := db.getReader().QueryContext(ctx, `SELECT s.id, `+dateCol+`, s.agent, s.message_count, s.transcript_revision || '|' || COALESCE(s.trends_generation, '') || '|' || s.created_at || '|' || COALESCE(s.started_at, '') FROM sessions s WHERE `+where, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var sessions []dashboardSession
	versions := make(map[string]string)
	for rows.Next() {
		var s dashboardSession
		if err := rows.Scan(&s.ID, &s.Date, &s.Agent, &s.MessageCount, &s.Fingerprint); err != nil {
			return nil, nil, err
		}
		if !InDateRange(LocalDate(s.Date, loc), f.From, f.To) {
			continue
		}
		sessions = append(sessions, s)
		versions[s.ID] = s.Fingerprint
	}
	return sessions, versions, rows.Err()
}

func (db *DB) dashboardParts(ctx context.Context, f AnalyticsFilter) ([]dashboardSession, map[string]kept.Part[dashboardPart], bool, error) {
	if db.readOnly || db.usageOnlyStorage() || f.Model != "" || f.HasTimeFilter() || f.Timezone == "Local" || f.ActiveSince != "" || TerminationUsesTime(f.Termination) {
		return nil, nil, false, nil
	}
	db.trendsPartsDisk()
	if !db.trendsDisk.Enabled() {
		return nil, nil, false, nil
	}
	identity, err := db.GetDatabaseID(ctx)
	if err != nil || identity == "" {
		return nil, nil, false, nil
	}
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	_, versions, err := db.dashboardSessions(ctx, f)
	release()
	if err != nil {
		return nil, nil, false, err
	}
	key, err := json.Marshal(versions, json.Deterministic(true))
	if err != nil {
		return nil, nil, false, err
	}
	result, err := db.dashboardFlights.Do(ctx, identity+"|"+TrendsTimezoneKey(f)+"|"+string(key), func(ctx context.Context) (dashboardPartsResult, error) {
		release, err := db.acquireBulkRead(ctx)
		if err != nil {
			return dashboardPartsResult{}, err
		}
		defer release()
		db.dashboardPartsOnce.Do(func() { db.dashboardPartsSlot = make(chan struct{}, 1) })
		select {
		case db.dashboardPartsSlot <- struct{}{}:
			defer func() { <-db.dashboardPartsSlot }()
		case <-ctx.Done():
			return dashboardPartsResult{}, ctx.Err()
		}
		tokens := func() ([2]string, error) {
			var v [2]string
			err := db.getReader().QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_common'), ''), COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_scope'), '')`).Scan(&v[0], &v[1])
			return v, err
		}
		before, err := tokens()
		if err != nil {
			return dashboardPartsResult{}, err
		}
		if before[0] == "" || before[1] == "" {
			return dashboardPartsResult{}, nil
		}
		sessions, current, err := db.dashboardSessions(ctx, f)
		if err != nil {
			return dashboardPartsResult{}, err
		}
		if !maps.Equal(versions, current) {
			return dashboardPartsResult{}, nil
		}
		slot := "dashboard-parts|" + TrendsTimezoneKey(f)
		version := fmt.Sprintf("%s|%d", identity, dashboardPartsVersion)
		parts := kept.LoadParts[dashboardPart](&db.trendsDisk, slot, version)
		var ids []string
		for id, fingerprint := range versions {
			if p, ok := parts[id]; !ok || p.Fingerprint != fingerprint {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			if dashboardPartsReadObserver != nil {
				dashboardPartsReadObserver(ids)
			}
			if err := db.fillDashboardParts(ctx, f, ids, versions, parts); err != nil {
				return dashboardPartsResult{}, err
			}
		}
		if dashboardPartsBeforeValidationObserver != nil {
			dashboardPartsBeforeValidationObserver(ctx)
		}
		after, err := tokens()
		if err != nil {
			return dashboardPartsResult{}, err
		}
		_, latest, err := db.dashboardSessions(ctx, f)
		if err != nil {
			return dashboardPartsResult{}, err
		}
		if before != after || !maps.Equal(versions, latest) {
			return dashboardPartsResult{}, nil
		}
		if len(ids) > 0 {
			saved := make([]kept.Part[dashboardPart], 0, len(parts))
			for _, p := range parts {
				saved = append(saved, p)
			}
			kept.SaveParts(&db.trendsDisk, slot, version, saved, func(p dashboardPart) int64 { return int64(128 + len(p.Hours)*16 + len(p.Velocity.TurnCycles)*8) })
		}
		return dashboardPartsResult{Sessions: sessions, Parts: parts, Handled: true}, nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return result.Sessions, result.Parts, result.Handled, nil
}

func (db *DB) fillDashboardParts(ctx context.Context, f AnalyticsFilter, ids []string, versions map[string]string, parts map[string]kept.Part[dashboardPart]) error {
	loc := f.location()
	for _, id := range ids {
		parts[id] = kept.Part[dashboardPart]{ID: id, Fingerprint: versions[id]}
	}
	return queryChunked(ids, func(chunk []string) error {
		ph, args := InPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `SELECT session_id, role, has_thinking, is_system, COALESCE(source_subtype,''), COALESCE(timestamp,''), content_length FROM messages WHERE session_id IN `+ph+` ORDER BY session_id, ordinal`, args...)
		if err != nil {
			return err
		}
		var current string
		var timings []TimingMessage
		finish := func() {
			if current != "" {
				p := parts[current]
				p.Value.Velocity = ComputeSessionVelocity(timings)
				parts[current] = p
			}
		}
		for rows.Next() {
			var id, role, subtype, ts string
			var thinking, system bool
			var length int
			if err := rows.Scan(&id, &role, &thinking, &system, &subtype, &ts, &length); err != nil {
				rows.Close()
				return err
			}
			if id != current {
				finish()
				current = id
				timings = nil
			}
			p := parts[id]
			v := &p.Value
			v.Messages++
			if role == "user" && !system && subtype != "tool_result" {
				v.User++
			}
			if role == "assistant" {
				v.Assistant++
			}
			if thinking {
				v.Thinking++
			}
			t, valid := LocalTime(ts, loc)
			if valid {
				if v.Hours == nil {
					v.Hours = make(map[int]int)
				}
				v.Hours[((int(t.Weekday())+6)%7)*24+t.Hour()]++
			}
			if !parser.IsClaudeHookSubtype(subtype) {
				timings = append(timings, TimingMessage{Role: role, Time: t, Valid: valid, ContentLength: length})
			}
			parts[id] = p
		}
		finish()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = db.getReader().QueryContext(ctx, `SELECT session_id, COUNT(*) FROM tool_calls WHERE session_id IN `+ph+` GROUP BY session_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			p := parts[id]
			p.Value.Tools = n
			parts[id] = p
		}
		return rows.Err()
	})
}

func dashboardActivity(sessions []dashboardSession, parts map[string]kept.Part[dashboardPart], f AnalyticsFilter, granularity string) ActivityResponse {
	buckets := make(map[string]*ActivityEntry)
	loc := f.location()
	for _, s := range sessions {
		date := BucketDate(LocalDate(s.Date, loc), granularity)
		e := buckets[date]
		if e == nil {
			e = &ActivityEntry{Date: date, ByAgent: map[string]int{}}
			buckets[date] = e
		}
		p := parts[s.ID].Value
		e.Sessions++
		e.Messages += p.Messages
		e.UserMessages += p.User
		e.AssistantMessages += p.Assistant
		e.ThinkingMessages += p.Thinking
		e.ToolCalls += p.Tools
		if p.Messages > 0 {
			e.ByAgent[s.Agent] += p.Messages
		}
	}
	series := make([]ActivityEntry, 0, len(buckets))
	for _, e := range buckets {
		series = append(series, *e)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Date < series[j].Date })
	return ActivityResponse{Granularity: granularity, Series: series}
}

func dashboardHours(sessions []dashboardSession, parts map[string]kept.Part[dashboardPart]) HourOfWeekResponse {
	var grid [7][24]int
	for _, s := range sessions {
		for h, n := range parts[s.ID].Value.Hours {
			grid[h/24][h%24] += n
		}
	}
	return HourOfWeekResponseFromGrid(grid)
}

func dashboardVelocity(sessions []dashboardSession, parts map[string]kept.Part[dashboardPart]) VelocityResponse {
	ids := make([]string, 0, len(sessions))
	metadata := make(map[string]VelocitySession)
	velocities := make(map[string]SessionVelocity)
	tools := make(map[string]int)
	for _, s := range sessions {
		ids = append(ids, s.ID)
		metadata[s.ID] = VelocitySession{Agent: s.Agent, MessageCount: s.MessageCount}
		velocities[s.ID] = parts[s.ID].Value.Velocity
		tools[s.ID] = parts[s.ID].Value.Tools
	}
	return BuildVelocityResponseFromSessions(ids, metadata, velocities, tools)
}
