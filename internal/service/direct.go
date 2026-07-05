package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/sessionwatch"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/timeutil"
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
	hideStaleSecretCount(s, secrets.ActiveRulesVersions())
	return buildSessionDetail(s), nil
}

func (b *directBackend) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	return b.db.FindSessionIDsByPartial(ctx, partial, limit)
}

// buildSessionDetail wraps a db.Session with its computed health
// breakdown. The same shape is returned by GET /api/v1/sessions/{id}.
func buildSessionDetail(s *db.Session) *SessionDetail {
	detail := &SessionDetail{
		Session: *s,
		// Derive-on-read: no persisted column. Computed once here from the
		// session's agent and source_version so MarshalJSON just passes the
		// field through and the HTTP backend round-trips it.
		DecodeConfidence: parser.DecodeConfidence(s.Agent, s.SourceVersion),
	}
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
			Heuristics:             persistedHeuristics(s),
		})
		detail.HealthScoreBasis = result.Basis
		detail.HealthPenalties = result.Penalties
	}
	return detail
}

func persistedHeuristics(s *db.Session) signals.HeuristicSignals {
	qs := s.StoredQualitySignals()
	if qs == nil {
		return signals.HeuristicSignals{}
	}
	return signals.HeuristicSignals{
		ShortPromptCount:            qs.ShortPromptCount,
		UnstructuredStart:           qs.UnstructuredStart,
		MissingSuccessCriteriaCount: qs.MissingSuccessCriteriaCount,
		MissingVerificationCount:    qs.MissingVerificationCount,
		DuplicatePromptCount:        qs.DuplicatePromptCount,
		NoCodeContextCount:          qs.NoCodeContextCount,
		RunawayToolLoopCount:        qs.RunawayToolLoopCount,
	}
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
	if _, err := db.ParseSortSpec(f.OrderBy); err != nil {
		return nil, fmt.Errorf(
			"list: invalid sort %q: %v (valid keys: %s)",
			f.OrderBy, err, strings.Join(db.SortKeys(), ", "),
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

	filter := listFilterToDB(f)
	page, err := b.db.ListSessions(ctx, filter)
	if err != nil {
		return nil, err
	}
	hideStaleSecretCounts(page.Sessions, filter.SecretsRulesVersions)
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
		Project:              f.Project,
		ExcludeProject:       f.ExcludeProject,
		Machine:              f.Machine,
		GitBranch:            f.GitBranch,
		Agent:                f.Agent,
		Date:                 f.Date,
		DateFrom:             f.DateFrom,
		DateTo:               f.DateTo,
		ActiveSince:          f.ActiveSince,
		MinMessages:          f.MinMessages,
		MaxMessages:          f.MaxMessages,
		MinUserMessages:      f.MinUserMessages,
		ExcludeOneShot:       !f.IncludeOneShot,
		ExcludeAutomated:     !f.IncludeAutomated,
		IncludeChildren:      f.IncludeChildren,
		Cursor:               f.Cursor,
		Limit:                f.Limit,
		MinToolFailures:      f.MinToolFailures,
		HasSecret:            f.HasSecret,
		Starred:              f.Starred,
		SecretsRulesVersions: secrets.ActiveRulesVersions(),
	}
	// Parse the public sort spec into the structured, per-key form. The spec is
	// validated in List before this runs, so a parse error here is treated
	// defensively as the default sort. The legacy Descending param fills the
	// direction of any term that carries no explicit :asc/:desc suffix; it is
	// also carried through so an empty order_by + descending still flips the
	// implicit default recent key.
	if keys, err := db.ParseSortSpec(f.OrderBy); err == nil {
		filter.Sort = db.ApplyFallbackDirection(keys, f.Descending)
	}
	filter.Descending = f.Descending
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

func hideStaleSecretCounts(sessions []db.Session, activeVersions []string) {
	for i := range sessions {
		hideStaleSecretCount(&sessions[i], activeVersions)
	}
}

func hideStaleSecretCount(s *db.Session, activeVersions []string) {
	if s.SecretLeakCount == 0 {
		return
	}
	if slices.Contains(activeVersions, s.SecretsRulesVersion) {
		return
	}
	s.SecretLeakCount = 0
}

// defaultAroundSpan is the number of messages returned on each side of the
// anchor when Around is set but Before/After are omitted.
const defaultAroundSpan = 5

func (b *directBackend) Messages(
	ctx context.Context, id string, f MessageFilter,
) (*MessageList, error) {
	if f.Around != nil && (f.From != nil || f.Direction != "") {
		return nil, ErrAroundMutuallyExclusive
	}
	if (f.Before != nil || f.After != nil) && f.Around == nil {
		return nil, ErrBeforeAfterRequireAround
	}
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

	w := db.MessageWindow{Limit: limit, Asc: asc, Roles: f.Roles}
	if f.Around != nil {
		w.Around = f.Around
		w.Before = defaultAroundSpan
		if f.Before != nil {
			w.Before = *f.Before
		}
		w.After = defaultAroundSpan
		if f.After != nil {
			w.After = *f.After
		}
	} else {
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
		w.From = &from
	}

	msgs, err := b.db.GetMessagesWindow(ctx, id, w)
	if err != nil {
		return nil, err
	}
	list := &MessageList{Messages: msgs, Count: len(msgs)}
	if len(msgs) > 0 {
		first := msgs[0].Ordinal
		last := msgs[len(msgs)-1].Ordinal
		list.FirstOrdinal = &first
		list.LastOrdinal = &last
	}
	return list, nil
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
		storedPath := b.local.GetSessionFilePath(in.ID)
		if storedPath == "" {
			return nil, fmt.Errorf(
				"sync: no file_path recorded for session %q", in.ID,
			)
		}
		// Visual Studio Copilot stores file_path as a
		// <traceFile>#<conversationID> virtual key. Stripping it to the
		// physical trace and syncing the path would reparse every conversation
		// in that trace, lose the requested conversation's scope, and do
		// nothing if the representative trace was deleted while the
		// conversation lives on in a sibling. The single-session path keeps the
		// conversation scope and follows it across sibling trace files.
		if _, _, ok :=
			parser.SplitVisualStudioCopilotVirtualPath(storedPath); ok {
			if err := b.engine.SyncSingleSessionContext(
				ctx, in.ID,
			); err != nil {
				return nil, err
			}
			return b.Get(ctx, in.ID)
		}
		path = parser.ResolveSourceFilePath(storedPath)
	}

	b.engine.SyncPaths([]string{path})

	id := in.ID
	if id == "" {
		resolved, err := b.resolveSessionIDByPath(ctx, path)
		if err != nil {
			return nil, err
		}
		id = resolved
		return b.Get(ctx, id)
	}

	detail, err := b.Get(ctx, id)
	if err != nil || detail != nil {
		return detail, err
	}
	// The requested session is gone after sync. Vibe is the only agent that
	// reassigns a session's canonical ID for an unchanged source file: a
	// fallback ID is promoted when meta.json appears, and a canonical ID is
	// demoted to the directory-name fallback when meta.json is removed or
	// becomes unparseable. For a Vibe ID, resolve the file's current session
	// and confirm it is the replacement before returning it.
	if !isVibeSessionID(id) {
		return nil, syncSessionNotFoundError(id)
	}
	resolved, err := b.resolveSessionIDByPath(ctx, path)
	if err != nil {
		return nil, err
	}
	detail, err = b.Get(ctx, resolved)
	if err != nil || detail == nil {
		if err != nil {
			return nil, err
		}
		return nil, syncSessionNotFoundError(id)
	}
	if !isVibeReplacement(id, detail) {
		return nil, syncSessionNotFoundError(id)
	}
	return detail, nil
}

func syncSessionNotFoundError(id string) error {
	return fmt.Errorf("sync: session %q was not found after sync", id)
}

func isVibeSessionID(id string) bool {
	return strings.HasPrefix(id, "vibe:")
}

// isVibeReplacement reports whether detail is the Vibe session that now owns
// the requested session's source file after a canonical-ID reassignment. The
// caller resolves detail strictly by the requested session's file_path, so a
// different-ID Vibe session is the replacement regardless of direction
// (fallback->canonical when meta.json appears, canonical->fallback when it is
// removed or unparseable).
func isVibeReplacement(requestedID string, detail *SessionDetail) bool {
	return detail != nil &&
		detail.Agent == "vibe" &&
		requestedID != detail.ID
}

// resolveSessionIDByPath returns the single session id whose
// file_path equals the given absolute path or a virtual key backed
// by it. When a physical file produces multiple sessions (e.g.
// Claude forked transcripts), sync returns an ambiguity error
// instead of picking arbitrarily, so the caller can disambiguate via
// `session sync <id>`.
// Only called from Sync after it has verified b.local != nil.
func (b *directBackend) resolveSessionIDByPath(
	ctx context.Context, path string,
) (string, error) {
	q := `SELECT id FROM sessions
		WHERE file_path = ?
		ORDER BY created_at DESC`
	queryArgs := []any{path}
	// Visual Studio Copilot stores file_path as a virtual sync key
	// <traceFile>#<conversationID>, so an exact match on the physical
	// container path never resolves. Also match every conversation
	// synced from that container; multiple matches fall through to the
	// ambiguity error below, exactly like a multi-session JSONL file.
	if isVisualStudioCopilotVirtualContainerPath(path) {
		q = `SELECT id FROM sessions
			WHERE file_path = ? OR file_path LIKE ? ESCAPE '\'
			ORDER BY created_at DESC`
		queryArgs = append(queryArgs, db.EscapeLikePattern(path)+"#%")
	}
	rows, err := b.local.Reader().QueryContext(ctx, q, queryArgs...)
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

func isVisualStudioCopilotVirtualContainerPath(path string) bool {
	if parser.IsVisualStudioCopilotTraceFile(path) {
		return true
	}
	_, _, ok := parser.SplitVisualStudioCopilotVirtualPath(
		parser.VisualStudioCopilotVirtualPath(path, filepath.Base(path)),
	)
	return ok
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

// Search runs a full-text session search, mirroring the logic in
// internal/server.humaSearch so both transports return identical
// results: the raw query is normalized via db.PrepareFTSQuery, the
// limit is clamped to [1, db.MaxSearchLimit] (defaulting to
// db.DefaultSearchLimit), and a store without an FTS index yields
// ErrSearchUnavailable rather than an opaque failure.
func (b *directBackend) Search(
	ctx context.Context, req SearchRequest,
) (*SessionSearchResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, &db.SearchInputError{Msg: "search: query required"}
	}
	if !b.db.HasFTS() {
		return nil, ErrSearchUnavailable
	}
	// Match the HTTP handler's clampLimit semantics: <=0 -> default,
	// over-max -> max. (db.Search would otherwise snap an over-max
	// value to the default; pre-clamping keeps parity with the REST
	// path, which clamps before calling the store.)
	limit := req.Limit
	if limit <= 0 {
		limit = db.DefaultSearchLimit
	} else if limit > db.MaxSearchLimit {
		limit = db.MaxSearchLimit
	}
	page, err := b.db.Search(ctx, db.SearchFilter{
		Query:   db.PrepareFTSQuery(query),
		Project: req.Project,
		Sort:    req.Sort,
		Cursor:  req.Cursor,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	results := page.Results
	if results == nil {
		results = []db.SearchResult{}
	}
	return &SessionSearchResult{
		Results:    results,
		NextCursor: page.NextCursor,
	}, nil
}

// UsageSummary validates the request, runs the daily-usage query
// through the store, and folds the per-day breakdowns into range-wide
// totals. It works over SQLite and the PG read store because
// db.GetDailyUsage is on db.Store; a read store that cannot serve usage
// returns db.ErrReadOnly, which callers surface as 501.
func (b *directBackend) UsageSummary(
	ctx context.Context, req UsageRequest,
) (*UsageSummaryResult, error) {
	f, err := BuildUsageFilter(req)
	if err != nil {
		return nil, err
	}
	result, err := b.db.GetDailyUsage(ctx, f)
	if err != nil {
		return nil, err
	}
	summary := buildUsageSummary(f, result)
	if parser.AgentFilterLacksPerMessageTokenData(f.Agent) &&
		db.NoTokenData(result.Totals) {
		matchingSessions, err := b.db.GetUsageMatchingSessionCount(ctx, f)
		if err != nil {
			return nil, err
		}
		if matchingSessions > 0 {
			summary.UnsupportedUsage = &UnsupportedUsage{
				Kind: UnsupportedUsageKindForAgentFilter(f.Agent),
			}
		}
	}
	return summary, nil
}

func (b *directBackend) UsagePairwiseComparison(
	ctx context.Context, req UsagePairwiseComparisonRequest,
) (*UsagePairwiseComparisonResponse, error) {
	leftFilter, leftEmpty, rightFilter, rightEmpty, err := BuildUsagePairwiseFilters(req)
	if err != nil {
		return nil, err
	}
	leftFilter.Breakdowns = false
	rightFilter.Breakdowns = false
	leftFilter.SkipSessionCounts = false
	rightFilter.SkipSessionCounts = false

	var leftResult db.DailyUsageResult
	if !leftEmpty {
		leftResult, err = b.db.GetDailyUsage(ctx, leftFilter)
		if err != nil {
			return nil, err
		}
	}
	var rightResult db.DailyUsageResult
	if !rightEmpty {
		rightResult, err = b.db.GetDailyUsage(ctx, rightFilter)
		if err != nil {
			return nil, err
		}
	}

	out := BuildUsagePairwiseComparisonResult(leftResult, rightResult)
	return &out, nil
}

// maxContentSearchContext is the largest --context value SearchContent
// accepts. Larger requests are rejected rather than silently clamped, so a
// caller who asks for more context than the store will give is told, rather
// than getting a page that quietly carries less than requested.
const maxContentSearchContext = 10

// SearchContent maps the transport-neutral request to a
// db.ContentSearchFilter, calls the store, and redacts secret-shaped
// spans from each snippet unless Reveal is set. When Context > 0, each
// match is additionally enriched with ContextBefore/ContextAfter (see
// enrichContentContext).
func (b *directBackend) SearchContent(
	ctx context.Context, req ContentSearchRequest,
) (*ContentSearchResult, error) {
	if req.Mode == "fts" {
		for _, s := range req.Sources {
			if s != "messages" {
				return nil, &db.SearchInputError{Msg: fmt.Sprintf(
					"search: --fts searches messages only (got source %q)", s)}
			}
		}
		req.Sources = []string{"messages"}
	}
	// Context < 0 (not user-reachable via the clamped CLI/HTTP flag, but
	// possible from a direct SessionService caller) is treated as "off"
	// rather than rejected; only exceeding the max is a hard error.
	if req.Context < 0 {
		req.Context = 0
	}
	if req.Context > maxContentSearchContext {
		return nil, &db.SearchInputError{Msg: "context: maximum is 10"}
	}
	page, err := b.db.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:          req.Pattern,
		Mode:             req.Mode,
		Sources:          req.Sources,
		ExcludeSystem:    req.ExcludeSystem,
		Project:          req.Project,
		ExcludeProject:   req.ExcludeProject,
		Machine:          req.Machine,
		GitBranch:        req.GitBranch,
		Agent:            req.Agent,
		Date:             req.Date,
		DateFrom:         req.DateFrom,
		DateTo:           req.DateTo,
		ActiveSince:      req.ActiveSince,
		IncludeChildren:  req.IncludeChildren,
		IncludeAutomated: req.IncludeAutomated,
		IncludeOneShot:   req.IncludeOneShot,
		// The store builds snippets from the full source field and redacts
		// secrets (including ones straddling the snippet window) unless reveal
		// is set. Redacting the pre-truncated snippet here would miss those.
		RevealSecrets: req.Reveal,
		Limit:         req.Limit,
		Cursor:        req.Cursor,
	})
	if err != nil {
		return nil, err
	}
	if req.Context > 0 {
		if err := b.enrichContentContext(ctx, page.Matches, req.Context); err != nil {
			return nil, err
		}
	}
	return &ContentSearchResult{
		Matches:    page.Matches,
		NextCursor: page.NextCursor,
	}, nil
}

// enrichContentContext populates ContextBefore/ContextAfter on each match
// with a non-negative Ordinal, fetching n messages of context on each side
// of the match's ordinal and splitting the returned (ascending, anchor
// included) window on the anchor -- the anchor row is dropped from both
// slices since it duplicates the match already in the page. Matches with a
// negative ordinal are left unenriched: no current search path produces
// one, but a defensive skip here means a future one (e.g. a name-only
// match with no message row) degrades gracefully instead of panicking on
// the *w.Around dereference or fetching the wrong window.
func (b *directBackend) enrichContentContext(
	ctx context.Context, matches []db.ContentMatch, n int,
) error {
	for i := range matches {
		m := &matches[i]
		if m.Ordinal < 0 {
			continue
		}
		anchor := m.Ordinal
		msgs, err := b.db.GetMessagesWindow(ctx, m.SessionID, db.MessageWindow{
			Around: &anchor, Before: n, After: n,
		})
		if err != nil {
			return fmt.Errorf("content search context: %w", err)
		}
		for _, msg := range msgs {
			switch {
			case msg.Ordinal < anchor:
				m.ContextBefore = append(m.ContextBefore, msg)
			case msg.Ordinal > anchor:
				m.ContextAfter = append(m.ContextAfter, msg)
			}
		}
	}
	return nil
}

const secretSourceChanged = "source changed; cannot reveal"

func (b *directBackend) ListSecrets(
	ctx context.Context, f SecretListFilter,
) (*SecretFindingList, error) {
	// Unspecified confidence shows definite findings only. Candidates
	// (e.g. high-entropy assignments) are FP-prone investigative material
	// and must be opted into explicitly with confidence "candidate" or
	// "all". This matches the product meaning of has_secret and
	// secret_leak_count, which count definite findings only.
	confidence := f.Confidence
	if confidence == "" {
		confidence = secrets.ConfidenceDefinite
	}
	page, err := b.db.ListSecretFindings(ctx, db.SecretFindingFilter{
		Project: f.Project, Agent: f.Agent,
		DateFrom: f.DateFrom, DateTo: f.DateTo,
		Rule: f.Rule, Confidence: confidence,
		RulesVersions: secrets.ActiveRulesVersions(),
		Limit:         f.Limit, Cursor: f.Cursor,
	})
	if err != nil {
		return nil, err
	}
	if f.Reveal {
		for i := range page.Findings {
			page.Findings[i].RedactedMatch = b.revealFinding(ctx, page.Findings[i])
		}
	}
	return &SecretFindingList{
		Findings: page.Findings, NextCursor: page.NextCursor,
	}, nil
}

func (b *directBackend) ScanSecrets(
	ctx context.Context, in SecretScanInput,
	progress func(SecretScanProgress),
) (*SecretScanSummary, error) {
	if b.engine == nil {
		return nil, db.ErrReadOnly
	}
	sum, err := b.engine.ScanSecrets(ctx, sync.SecretScanInput{
		Backfill: in.Backfill, Project: in.Project, Agent: in.Agent,
		DateFrom: in.DateFrom, DateTo: in.DateTo,
	}, func(p sync.SecretScanProgress) {
		if progress != nil {
			progress(SecretScanProgress{Scanned: p.Scanned, Total: p.Total})
		}
	})
	if err != nil {
		return nil, err
	}
	return &SecretScanSummary{
		Scanned:           sum.Scanned,
		WithSecrets:       sum.WithSecrets,
		TotalFindings:     sum.TotalFindings,
		DefiniteFindings:  sum.DefiniteFindings,
		CandidateFindings: sum.CandidateFindings,
	}, nil
}

// revealFinding re-reads the finding's source by its stored coordinates and
// returns the full value only if the same rule still matches there; otherwise
// a marker. It never logs or stores the value.
func (b *directBackend) revealFinding(
	ctx context.Context, f db.SecretFindingRow,
) string {
	src, ok, err := b.db.SecretFindingSource(ctx, f.SecretFinding)
	if err != nil || !ok {
		return secretSourceChanged
	}
	if !secrets.Verify(f.RuleName, src, f.MatchStart, f.MatchEnd) {
		return secretSourceChanged
	}
	return src[f.MatchStart:f.MatchEnd]
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
	f.Agent = normalizeStatsAgentFilter(f.Agent)
	stats, err := b.local.GetSessionStats(ctx, db.StatsFilter{
		Since:                 f.Since,
		Until:                 f.Until,
		Agent:                 f.Agent,
		IncludeProjects:       f.IncludeProjects,
		ExcludeProjects:       f.ExcludeProjects,
		Timezone:              f.Timezone,
		IncludeGitOutcomes:    f.IncludeGitOutcomes,
		IncludeGitHubOutcomes: f.IncludeGitHubOutcomes,
		GHToken:               f.GHToken,
	})
	if err != nil {
		return nil, err
	}
	stats.CodeAttribution = collectCodeAttribution(f, stats)
	return stats, nil
}

func collectCodeAttribution(
	f StatsFilter,
	stats *SessionStats,
) *db.CodeAttribution {
	if stats == nil {
		return nil
	}
	sources := []db.CodeAttributionSource{}
	if source, ok := collectCursorAttribution(f, stats); ok {
		sources = append(sources, source)
	}
	if len(sources) == 0 {
		return nil
	}
	slices.SortFunc(sources, func(a, b db.CodeAttributionSource) int {
		if n := strings.Compare(a.Provider, b.Provider); n != 0 {
			return n
		}
		if n := strings.Compare(a.Scope, b.Scope); n != 0 {
			return n
		}
		return strings.Compare(a.Status, b.Status)
	})
	return &db.CodeAttribution{Sources: sources}
}

func collectCursorAttribution(
	f StatsFilter,
	stats *SessionStats,
) (db.CodeAttributionSource, bool) {
	switch cursorAttributionDecision(f) {
	case cursorAttributionSkip:
		return db.CodeAttributionSource{}, false
	case cursorAttributionUnsupportedProjectFilter:
		return cursorAttributionSource(
			"unsupported_filter",
			"Cursor attribution is machine-local and cannot be scoped by project filters",
		), true
	}
	from, err := time.Parse(time.RFC3339, stats.Window.Since)
	if err != nil {
		return cursorAttributionSource(
			"error",
			"failed to parse stats window for Cursor attribution",
		), true
	}
	to, err := time.Parse(time.RFC3339, stats.Window.Until)
	if err != nil {
		return cursorAttributionSource(
			"error",
			"failed to parse stats window for Cursor attribution",
		), true
	}
	attr, status, err := parser.LoadCursorAttribution(from, to)
	if err != nil {
		return cursorAttributionSource(
			"error",
			"failed to load Cursor attribution: "+err.Error(),
		), true
	}
	return mapCursorAttributionSource(attr, status), true
}

func normalizeStatsAgentFilter(raw string) string {
	parts := strings.Split(raw, ",")
	filtered := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "all" {
			continue
		}
		filtered = append(filtered, part)
	}
	return strings.Join(filtered, ",")
}

type cursorAttributionLoadDecision int

const (
	cursorAttributionLoad cursorAttributionLoadDecision = iota
	cursorAttributionSkip
	cursorAttributionUnsupportedProjectFilter
	cursorAttributionScopeMachineLocal = "machine_local"
)

func cursorAttributionDecision(f StatsFilter) cursorAttributionLoadDecision {
	agents := strings.Split(normalizeStatsAgentFilter(f.Agent), ",")
	seen := 0
	hasCursor := false
	for _, agent := range agents {
		agent = strings.TrimSpace(agent)
		if agent == "" {
			continue
		}
		seen++
		if agent == "cursor" {
			hasCursor = true
		}
	}
	if seen > 0 && !hasCursor {
		return cursorAttributionSkip
	}
	if len(f.IncludeProjects) > 0 || len(f.ExcludeProjects) > 0 {
		return cursorAttributionUnsupportedProjectFilter
	}
	return cursorAttributionLoad
}

func mapCursorAttributionSource(
	attr *parser.CursorAttribution,
	status parser.CursorAttributionStatus,
) db.CodeAttributionSource {
	if attr == nil {
		out := cursorAttributionSource(string(status), "")
		if status == parser.CursorAttributionUnavailable {
			out.Warnings = []string{
				"Cursor attribution database is unavailable on this machine",
			}
		}
		return out
	}
	out := cursorAttributionSource(string(status), "")
	out.Metrics = &db.CursorAttributionMetrics{
		ScoredCommits:        attr.ScoredCommits,
		LinesAdded:           attr.LinesAdded,
		LinesDeleted:         attr.LinesDeleted,
		TabLinesAdded:        attr.TabLinesAdded,
		TabLinesDeleted:      attr.TabLinesDeleted,
		ComposerLinesAdded:   attr.ComposerLinesAdded,
		ComposerLinesDeleted: attr.ComposerLinesDeleted,
		HumanLinesAdded:      attr.HumanLinesAdded,
		HumanLinesDeleted:    attr.HumanLinesDeleted,
		BlankLinesAdded:      attr.BlankLinesAdded,
		BlankLinesDeleted:    attr.BlankLinesDeleted,
		AIAuthoredPct:        attr.AIAuthoredPct,
	}
	if len(attr.ConversationCounts) == 0 {
		return out
	}
	out.Metrics.ConversationCounts = make(
		[]db.CursorConversationCount,
		0,
		len(attr.ConversationCounts),
	)
	for _, entry := range attr.ConversationCounts {
		out.Metrics.ConversationCounts = append(
			out.Metrics.ConversationCounts,
			db.CursorConversationCount{
				Model: entry.Model,
				Mode:  entry.Mode,
				Count: entry.Count,
			},
		)
	}
	return out
}

func cursorAttributionSource(status, warning string) db.CodeAttributionSource {
	out := db.CodeAttributionSource{
		Provider: "cursor",
		Status:   status,
		Scope:    cursorAttributionScopeMachineLocal,
	}
	if warning != "" {
		out.Warnings = []string{warning}
	}
	return out
}
