package db

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/parser"
)

// Increment when dashboard counts, ComputeSessionVelocity, or parser.IsClaudeHookSubtype changes.
const dashboardPartsVersion = 2

var dashboardPartsReadObserver func([]string)

type dashboardPart struct {
	Messages, User, Assistant, Thinking, Tools int
	Hours                                      map[int]int
	Velocity                                   SessionVelocity
	HasResponse                                bool
}

type dashboardSession struct {
	ID, Date, Agent, Fingerprint string
	MessageCount                 int
}

// dashboardPartsMemo keeps the latest timezone's validated parts in memory
// so a selection whose file lacks a session can reuse it from another filter.
type dashboardPartsMemo struct {
	mu            sync.Mutex
	slot, version string
	parts         map[string]kept.Part[dashboardPart]
	bytes         int64
}

// reuse copies parts whose fingerprint matches into parts and returns the ids still to read.
func (m *dashboardPartsMemo) reuse(slot, version string, ids []string, versions map[string]string, parts map[string]kept.Part[dashboardPart]) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slot != slot || m.version != version {
		return ids
	}
	return slices.DeleteFunc(ids, func(id string) bool {
		p, ok := m.parts[id]
		if ok = ok && p.Fingerprint == versions[id]; ok {
			parts[id] = p
		}
		return ok
	})
}

func (m *dashboardPartsMemo) store(slot, version string, parts map[string]kept.Part[dashboardPart]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slot != slot || m.version != version {
		m.slot, m.version, m.parts, m.bytes = slot, version, make(map[string]kept.Part[dashboardPart], len(parts)), 0
	}
	for id, p := range parts {
		m.add(id, p)
	}
}

// add skips oversized parts and clears earlier entries when a fitting part would exceed the limit.
func (m *dashboardPartsMemo) add(id string, p kept.Part[dashboardPart]) {
	if old, ok := m.parts[id]; ok {
		m.bytes -= dashboardPartBytes(old)
		delete(m.parts, id)
	}
	n := dashboardPartBytes(p)
	if n > kept.DefaultMaxBytes {
		return
	}
	if m.bytes+n > kept.DefaultMaxBytes {
		clear(m.parts)
		m.bytes = 0
	}
	m.parts[id] = p
	m.bytes += n
}

func dashboardPartBytes(p kept.Part[dashboardPart]) int64 {
	return int64(len(p.ID)+len(p.Fingerprint)) + dashboardPartSize(p.Value)
}

func dashboardPartSize(p dashboardPart) int64 {
	return int64(128 + len(p.Hours)*16 + len(p.Velocity.TurnCycles)*8)
}

type dashboardPartsResult struct {
	Parts   map[string]kept.Part[dashboardPart]
	Handled bool
}

func (db *DB) dashboardSessions(ctx context.Context, f AnalyticsFilter) ([]dashboardSession, map[string]string, error) {
	loc := f.location()
	dateCol := "COALESCE(NULLIF(s.started_at, ''), s.created_at)"
	where, args := f.buildWhereWithDate(dateCol, true, "s.id")
	rows, err := db.getReader().QueryContext(ctx, `SELECT s.id, `+dateCol+`, s.agent, s.message_count, s.transcript_revision || '|' || COALESCE(s.trends_generation, '') || '|' || s.message_count, s.tools_generation, `+toolsSessionFingerprintSQL+` FROM sessions s WHERE `+where+` ORDER BY s.id`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var sessions []dashboardSession
	var dirty []string
	versions := make(map[string]string)
	for rows.Next() {
		var s dashboardSession
		var generation *string
		var toolsFingerprint string
		if err := rows.Scan(&s.ID, &s.Date, &s.Agent, &s.MessageCount, &s.Fingerprint, &generation, &toolsFingerprint); err != nil {
			return nil, nil, err
		}
		if !InDateRange(LocalDate(s.Date, loc), f.From, f.To) {
			continue
		}
		if generation == nil {
			dirty = append(dirty, s.ID)
		} else {
			s.Fingerprint += "|" + toolsFingerprint
		}
		sessions = append(sessions, s)
		versions[s.ID] = s.Fingerprint
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if len(dirty) > 0 {
		fingerprints, err := db.stampToolsSessions(ctx, dirty)
		if err != nil {
			return nil, nil, err
		}
		for i := range sessions {
			if fingerprint, ok := fingerprints[sessions[i].ID]; ok {
				sessions[i].Fingerprint += "|" + fingerprint
				versions[sessions[i].ID] = sessions[i].Fingerprint
			}
		}
	}
	return sessions, versions, nil
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
	if err != nil {
		return nil, nil, false, err
	}
	if identity == "" {
		return nil, nil, false, nil
	}
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	sessions, versions, err := db.dashboardSessions(ctx, f)
	release()
	if errors.Is(err, ErrWriterClosed) {
		// The maintenance worker owns the writer; direct reads can still use the reader.
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	filter, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return nil, nil, false, err
	}
	slot := "dashboard-parts|" + TrendsTimezoneKey(f) + "|" + string(filter)
	version := fmt.Sprintf("%s|%d", identity, dashboardPartsVersion)
	hash := sha256.New()
	for _, session := range sessions {
		fmt.Fprintf(hash, "%d:%s%d:%s", len(session.ID), session.ID, len(session.Fingerprint), session.Fingerprint)
	}
	result, err := db.dashboardFlights.Do(ctx, fmt.Sprintf("%s|%s|%x", slot, identity, hash.Sum(nil)), func(ctx context.Context) (dashboardPartsResult, error) {
		release, err := db.acquireBulkRead(ctx)
		if err != nil {
			return dashboardPartsResult{}, err
		}
		defer release()
		parts := kept.LoadParts[dashboardPart](&db.trendsDisk, slot, version)
		changed := kept.StaleParts(parts, versions)
		var ids []string
		for id := range changed {
			delete(parts, id)
			if _, ok := versions[id]; ok {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		ids = db.dashboardMemo.reuse("dashboard-parts|"+TrendsTimezoneKey(f), version, ids, versions, parts)
		if len(ids) > 0 {
			if dashboardPartsReadObserver != nil {
				dashboardPartsReadObserver(ids)
			}
			if err := db.fillDashboardParts(ctx, f, ids, versions, parts); err != nil {
				return dashboardPartsResult{}, err
			}
		}
		if len(changed) > 0 {
			saveDashboardParts(db, f, slot, version, parts)
		} else {
			// A file loaded after a restart must seed the memo for other filters.
			db.dashboardMemo.store("dashboard-parts|"+TrendsTimezoneKey(f), version, parts)
		}
		return dashboardPartsResult{Parts: parts, Handled: true}, nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return sessions, result.Parts, result.Handled, nil
}

func saveDashboardParts(db *DB, f AnalyticsFilter, slot, version string, parts map[string]kept.Part[dashboardPart]) {
	db.dashboardMemo.store("dashboard-parts|"+TrendsTimezoneKey(f), version, parts)
	saved := make([]kept.Part[dashboardPart], 0, len(parts))
	for _, p := range parts {
		saved = append(saved, p)
	}
	kept.SaveParts(&db.trendsDisk, slot, version, saved, dashboardPartSize)
}

func (db *DB) fillDashboardParts(ctx context.Context, f AnalyticsFilter, ids []string, versions map[string]string, parts map[string]kept.Part[dashboardPart]) error {
	for _, id := range ids {
		parts[id] = kept.Part[dashboardPart]{ID: id, Fingerprint: versions[id]}
	}
	return queryChunked(ids, func(chunk []string) error {
		ph, args := InPlaceholders(chunk)
		if err := db.fillDashboardMessageParts(ctx, f, ph, args, parts); err != nil {
			return err
		}
		return db.fillDashboardToolParts(ctx, ph, args, parts)
	})
}

func (db *DB) fillDashboardMessageParts(ctx context.Context, f AnalyticsFilter, ph string, args []any, parts map[string]kept.Part[dashboardPart]) error {
	rows, err := db.getReader().QueryContext(ctx, `SELECT session_id, role, has_thinking, is_system, COALESCE(source_subtype,''), COALESCE(timestamp,''), content_length FROM messages WHERE session_id IN `+ph+` ORDER BY session_id, ordinal`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	loc := f.location()
	var current string
	var timings []TimingMessage
	finish := func() {
		if current != "" {
			p := parts[current]
			p.Value.Velocity = ComputeSessionVelocity(timings)
			p.Value.HasResponse = p.Value.Velocity.FirstResponse != nil
			parts[current] = p
		}
	}
	for rows.Next() {
		var id, role, subtype, ts string
		var thinking, system bool
		var length int
		if err := rows.Scan(&id, &role, &thinking, &system, &subtype, &ts, &length); err != nil {
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
		user, assistant, thought := activityMessageCounts(role, thinking, system, subtype, 1)
		v.User += user
		v.Assistant += assistant
		v.Thinking += thought
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
	return rows.Err()
}

func (db *DB) fillDashboardToolParts(ctx context.Context, ph string, args []any, parts map[string]kept.Part[dashboardPart]) error {
	rows, err := db.getReader().QueryContext(ctx, `SELECT session_id, COUNT(*) FROM tool_calls WHERE session_id IN `+ph+` GROUP BY session_id`, args...)
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
		p := parts[s.ID].Value
		if p.HasResponse && p.Velocity.FirstResponse == nil {
			p.Velocity.FirstResponse = new(float64)
		}
		velocities[s.ID] = p.Velocity
		tools[s.ID] = parts[s.ID].Value.Tools
	}
	return BuildVelocityResponseFromSessions(ids, metadata, velocities, tools)
}
