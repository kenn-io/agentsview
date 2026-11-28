package db

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"sync"

	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/parser"
)

// Increment when dashboard counts, ComputeSessionVelocity, or parser.IsClaudeHookSubtype changes.
const dashboardPartsVersion = 3

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
	boundedLRU[string, kept.Part[dashboardPart]]
	maxBytes int64
}

func (m *dashboardPartsMemo) reuseSelection(slot, version string, sessions []dashboardSession, versions map[string]string, parts map[string]kept.Part[dashboardPart]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slot != slot || m.version != version {
		return
	}
	for _, session := range sessions {
		p, ok := m.get(session.ID)
		if !ok {
			continue
		}
		fingerprint, current := versions[session.ID]
		if current && p.Fingerprint == fingerprint {
			parts[session.ID] = p
		} else {
			m.remove(session.ID)
		}
	}
}

func (m *dashboardPartsMemo) storeParts(slot, version string, parts map[string]kept.Part[dashboardPart]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	maxBytes := m.maxBytes
	if maxBytes == 0 {
		maxBytes = kept.DefaultMaxBytes
	}
	if m.slot != slot || m.version != version {
		m.slot, m.version = slot, version
		m.reset()
	}
	var pending []string
	var selectedBytes int64
	for id, p := range parts {
		cached, ok := m.peek(id)
		if !ok {
			pending = append(pending, id)
			continue
		}
		if cached.Fingerprint != p.Fingerprint {
			m.remove(id)
			pending = append(pending, id)
			continue
		}
		selectedBytes += dashboardPartBytes(cached)
	}
	if len(pending) == 0 {
		return
	}
	slices.Sort(pending)
	var oldest []string
	next := 0
	for _, id := range pending {
		p := parts[id]
		size := dashboardPartBytes(p)
		if size > maxBytes-selectedBytes {
			continue
		}
		// Retain selected parts and evict unrelated entries in LRU order.
		if m.bytes+size > maxBytes {
			if oldest == nil {
				oldest = m.keys()
			}
			for next < len(oldest) && m.bytes+size > maxBytes {
				old := oldest[next]
				next++
				if _, selected := parts[old]; !selected {
					m.remove(old)
				}
			}
		}
		if m.bytes+size <= maxBytes {
			m.add(id, p, size)
			selectedBytes += size
		}
	}
}

func dashboardPartBytes(p kept.Part[dashboardPart]) int64 {
	// The multiplier covers retained allocations; 256 covers the entry, list node, and map slot.
	return int64(len(p.ID)+len(p.Fingerprint)) + 2*dashboardPartSize(p.Value) + 256
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
	rows, err := db.getReader().QueryContext(ctx, `SELECT s.id, `+dateCol+`, s.agent, s.message_count, s.transcript_revision || '|' || COALESCE(s.trends_generation, '') || '|' || s.message_count FROM sessions s WHERE `+where+` ORDER BY s.id`, args...)
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
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
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
		memoSlot := "dashboard-parts|" + TrendsTimezoneKey(f)
		db.dashboardMemo.reuseSelection(memoSlot, version, sessions, versions, parts)
		ids = slices.DeleteFunc(ids, func(id string) bool {
			_, reused := parts[id]
			return reused
		})
		if len(ids) > 0 {
			if dashboardPartsReadObserver != nil {
				dashboardPartsReadObserver(ids)
			}
			if err := db.fillDashboardParts(ctx, f, ids, versions, parts); err != nil {
				return dashboardPartsResult{}, err
			}
		}
		db.dashboardMemo.storeParts(memoSlot, version, parts)
		if len(changed) > 0 {
			saveDashboardParts(db, slot, version, parts)
		}
		return dashboardPartsResult{Parts: parts, Handled: true}, nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return sessions, result.Parts, result.Handled, nil
}

func saveDashboardParts(db *DB, slot, version string, parts map[string]kept.Part[dashboardPart]) {
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
