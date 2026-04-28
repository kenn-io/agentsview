package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/sessionwatch"
	"github.com/wesm/agentsview/internal/signals"
	"github.com/wesm/agentsview/internal/sync"
	"github.com/wesm/agentsview/internal/timeutil"
)

// directBackend implements SessionService by wrapping a db.Store
// and, optionally, a *sync.Engine + local *db.DB for on-demand
// syncs. When local or engine is nil (e.g. the `pg serve` read
// daemon), Sync returns db.ErrReadOnly.
//
// The db field services all read methods through the Store
// interface, so the same type works for both SQLite and the PG
// read store. The local field holds the same *db.DB when present
// and exposes file_path-keyed helpers that aren't on the Store
// interface (GetSessionFilePath, Reader). Structural nil checks
// on local+engine replace runtime type assertions.
type directBackend struct {
	db     db.Store
	local  *db.DB
	engine *sync.Engine
}

// NewDirectBackend returns a full read/write SessionService
// backed by a local SQLite store and optional sync engine. When
// engine is nil, Sync returns db.ErrReadOnly but reads still
// work. Use NewReadOnlyBackend for stores that are not *db.DB
// (e.g. a PostgreSQL reader).
func NewDirectBackend(d *db.DB, engine *sync.Engine) SessionService {
	return &directBackend{db: d, local: d, engine: engine}
}

// NewReadOnlyBackend returns a read-only SessionService over any
// db.Store (e.g. a PostgreSQL reader used by `pg serve`). Sync
// returns db.ErrReadOnly unconditionally.
func NewReadOnlyBackend(d db.Store) SessionService {
	return &directBackend{db: d}
}

func (b *directBackend) Get(
	ctx context.Context, id string,
) (*SessionDetail, error) {
	s, err := b.db.GetSession(ctx, id)
	if err != nil || s == nil {
		return nil, err
	}
	return buildSessionDetail(s), nil
}

// buildSessionDetail wraps a db.Session with its computed health
// breakdown. The same shape is returned by GET /api/v1/sessions/{id}.
func buildSessionDetail(s *db.Session) *SessionDetail {
	detail := &SessionDetail{Session: *s}
	if s.HealthScore != nil {
		result := signals.ComputeHealthScore(signals.ScoreInput{
			Outcome:                s.Outcome,
			OutcomeConfidence:      s.OutcomeConfidence,
			HasToolCalls:           s.HasToolCalls,
			FailureSignalCount:     s.ToolFailureSignalCount,
			RetryCount:             s.ToolRetryCount,
			EditChurnCount:         s.EditChurnCount,
			ConsecutiveFailMax:     s.ConsecutiveFailureMax,
			HasContextData:         s.HasContextData,
			CompactionCount:        s.CompactionCount,
			MidTaskCompactionCount: s.MidTaskCompactionCount,
			PressureMax:            s.ContextPressureMax,
		})
		detail.HealthScoreBasis = result.Basis
		detail.HealthPenalties = result.Penalties
	}
	return detail
}

func (b *directBackend) List(
	ctx context.Context, f ListFilter,
) (*SessionList, error) {
	for _, d := range []string{f.Date, f.DateFrom, f.DateTo} {
		if d != "" && !timeutil.IsValidDate(d) {
			return nil, fmt.Errorf(
				"list: invalid date %q: use YYYY-MM-DD", d,
			)
		}
	}
	if f.DateFrom != "" && f.DateTo != "" && f.DateFrom > f.DateTo {
		return nil, errors.New(
			"list: date_from must not be after date_to",
		)
	}
	if f.ActiveSince != "" && !timeutil.IsValidTimestamp(f.ActiveSince) {
		return nil, fmt.Errorf(
			"list: invalid active_since %q: use RFC3339", f.ActiveSince,
		)
	}
	// Match the HTTP handler's clampLimit semantics: values over
	// MaxSessionLimit clamp to the max, not reset to the default.
	if f.Limit > db.MaxSessionLimit {
		f.Limit = db.MaxSessionLimit
	}
	if f.Limit <= 0 {
		f.Limit = db.DefaultSessionLimit
	}

	page, err := b.db.ListSessions(ctx, listFilterToDB(f))
	if err != nil {
		return nil, err
	}
	return &SessionList{
		Sessions:   page.Sessions,
		NextCursor: page.NextCursor,
		Total:      page.Total,
	}, nil
}

// listFilterToDB mirrors the query-parameter mapping in
// internal/server/sessions.go:handleListSessions so both
// transports produce identical SessionFilter values.
func listFilterToDB(f ListFilter) db.SessionFilter {
	filter := db.SessionFilter{
		Project:          f.Project,
		ExcludeProject:   f.ExcludeProject,
		Machine:          f.Machine,
		Agent:            f.Agent,
		Date:             f.Date,
		DateFrom:         f.DateFrom,
		DateTo:           f.DateTo,
		ActiveSince:      f.ActiveSince,
		MinMessages:      f.MinMessages,
		MaxMessages:      f.MaxMessages,
		MinUserMessages:  f.MinUserMessages,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
		Cursor:           f.Cursor,
		Limit:            f.Limit,
		MinToolFailures:  f.MinToolFailures,
	}
	if f.Outcome != "" {
		filter.Outcome = strings.Split(f.Outcome, ",")
	}
	if f.HealthGrade != "" {
		filter.HealthGrade = strings.Split(f.HealthGrade, ",")
	}
	if f.Termination != "" {
		filter.Termination = f.Termination
	}
	return filter
}

func (b *directBackend) Messages(
	ctx context.Context, id string, f MessageFilter,
) (*MessageList, error) {
	switch f.Direction {
	case "", "asc", "desc":
	default:
		return nil, fmt.Errorf(
			"messages: invalid direction %q: must be asc or desc",
			f.Direction,
		)
	}
	asc := f.Direction != "desc"
	limit := f.Limit
	if limit <= 0 {
		limit = db.DefaultMessageLimit
	}
	if limit > db.MaxMessageLimit {
		limit = db.MaxMessageLimit
	}
	// An omitted From means "newest" in descending mode and 0 in
	// ascending mode. An explicit 0 is a real ordinal and must be
	// honored in both directions.
	var from int
	switch {
	case f.From != nil:
		from = *f.From
	case !asc:
		from = math.MaxInt32
	}
	msgs, err := b.db.GetMessages(ctx, id, from, limit, asc)
	if err != nil {
		return nil, err
	}
	return &MessageList{Messages: msgs, Count: len(msgs)}, nil
}

func (b *directBackend) ToolCalls(
	ctx context.Context, id string,
) (*ToolCallList, error) {
	msgs, err := b.db.GetAllMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []ToolCall{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			out = append(out, ToolCall{
				Ordinal:           m.Ordinal,
				Timestamp:         m.Timestamp,
				ToolUseID:         tc.ToolUseID,
				ToolName:          tc.ToolName,
				Category:          tc.Category,
				InputJSON:         tc.InputJSON,
				SkillName:         tc.SkillName,
				SubagentSessionID: tc.SubagentSessionID,
				ResultLength:      tc.ResultContentLength,
			})
		}
	}
	return &ToolCallList{ToolCalls: out, Count: len(out)}, nil
}

// Sync runs a one-off sync for the file path associated with the
// given session (or an explicit path in SyncInput.Path) and
// returns the resulting session detail. Returns db.ErrReadOnly
// when this backend was constructed without a sync engine or
// local *db.DB (i.e. NewReadOnlyBackend).
func (b *directBackend) Sync(
	ctx context.Context, in SyncInput,
) (*SessionDetail, error) {
	if b.local == nil || b.engine == nil {
		return nil, db.ErrReadOnly
	}
	if in.Path == "" && in.ID == "" {
		return nil, errors.New("sync: path or id required")
	}
	if in.Path != "" && in.ID != "" {
		return nil, errors.New("sync: only one of path or id allowed")
	}

	path := in.Path
	if path == "" {
		path = b.local.GetSessionFilePath(in.ID)
		if path == "" {
			return nil, fmt.Errorf(
				"sync: no file_path recorded for session %q", in.ID,
			)
		}
	}

	b.engine.SyncPaths([]string{path})

	id := in.ID
	if id == "" {
		resolved, err := b.resolveSessionIDByPath(ctx, path)
		if err != nil {
			return nil, err
		}
		id = resolved
	}
	return b.Get(ctx, id)
}

// resolveSessionIDByPath returns the single session id whose
// file_path equals the given absolute path. When a JSONL file
// produces multiple sessions (e.g. Claude forked transcripts),
// sync returns an ambiguity error instead of picking arbitrarily,
// so the caller can disambiguate via `session sync <id>`.
// Only called from Sync after it has verified b.local != nil.
func (b *directBackend) resolveSessionIDByPath(
	ctx context.Context, path string,
) (string, error) {
	const q = `SELECT id FROM sessions
		WHERE file_path = ?
		ORDER BY created_at DESC`
	rows, err := b.local.Reader().QueryContext(ctx, q, path)
	if err != nil {
		return "", fmt.Errorf(
			"sync: resolving session for path %q: %w", path, err,
		)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf(
				"sync: scanning session row for path %q: %w",
				path, err,
			)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf(
			"sync: iterating sessions for path %q: %w", path, err,
		)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf(
			"sync: no session found for path %q", path,
		)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf(
			"sync: %d sessions found for path %q: %v; "+
				"pass one via `session sync <id>` to disambiguate",
			len(ids), path, ids,
		)
	}
}

// Watch returns a stream of events for the given session,
// emitting "session_updated" whenever the session's DB state
// changes and periodic "heartbeat" events so callers can detect
// a live channel. The returned channel is closed when ctx is
// cancelled. Returns an error if the session does not exist so a
// typo fails fast instead of producing an indefinite heartbeat
// stream.
func (b *directBackend) Watch(
	ctx context.Context, id string,
) (<-chan Event, error) {
	s, err := b.db.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("watch: looking up session %q: %w", id, err)
	}
	if s == nil {
		return nil, fmt.Errorf("watch: session not found: %s", id)
	}
	w := sessionwatch.New(b.db, b.engine)
	ticks := w.Events(ctx, id)
	out := make(chan Event)
	go func() {
		defer close(out)
		heartbeat := time.NewTicker(
			sessionwatch.PollInterval * sessionwatch.HeartbeatTicks,
		)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				select {
				case out <- Event{Event: "session_updated", Data: id}:
				case <-ctx.Done():
					return
				}
			case <-heartbeat.C:
				select {
				case out <- Event{Event: "heartbeat", Data: time.Now().UTC().Format(time.RFC3339)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Stats delegates to db.GetSessionStats on the underlying *db.DB.
// Requires a local *db.DB (not a generic db.Store) because the v1
// stats computation reaches into SQLite-specific helpers; read-only
// backends constructed via NewReadOnlyBackend return db.ErrReadOnly.
func (b *directBackend) Stats(
	ctx context.Context, f StatsFilter,
) (*SessionStats, error) {
	if b.local == nil {
		return nil, db.ErrReadOnly
	}
	return b.local.GetSessionStats(ctx, db.StatsFilter{
		Since:           f.Since,
		Until:           f.Until,
		Agent:           f.Agent,
		IncludeProjects: f.IncludeProjects,
		ExcludeProjects: f.ExcludeProjects,
		Timezone:        f.Timezone,
		GHToken:         f.GHToken,
	})
}
