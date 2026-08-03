package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
	"go.kenn.io/agentsview/internal/secrets"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// Store wraps a DuckDB connection for read-mostly serve mode. path and
// handleMu support live reopening after a mirror rebuild swaps in a new
// file (see WatchMirrorReplacement in mirror_watch.go): handleMu guards
// duck, fileInfo, and aliasPath so an in-flight query never observes a
// handle mid-swap. path is empty for NewStoreFromDB and remote/Quack
// stores, which have no local file to watch. aliasPath is the hardlink
// (<base>.reopen-N inside the mirror's work directory) that duck is
// actually opened against once the Store has reopened at least once (see
// openMirrorAlias); it is "" for the original connection NewStore opens
// directly on path.
type Store struct {
	*db.BunStore

	path      string
	handleMu  sync.RWMutex
	duck      *sql.DB
	bun       *bun.DB
	fileInfo  os.FileInfo
	aliasPath string
	closed    bool
	// retiring tracks in-flight mirror-replacement checks, registered in
	// beginReplacementCheck before a check opens anything and spanning
	// swapHandle's tail cleanup (closing the replaced handle and removing
	// its alias after the write lock is released). Close waits on it so
	// "closed" means no check is still opening or validating a replacement,
	// every retired handle is closed, and every alias this Store created is
	// gone.
	retiring sync.WaitGroup

	quack          *quackClient
	connectionKind duckDBConnectionKind
}

type duckBunBackend struct {
	store *Store
}

var _ db.BunBackend = (*duckBunBackend)(nil)

func (*duckBunBackend) Name() string { return "duckdb" }

func (*duckBunBackend) ReadOnly() bool { return true }

func (*duckBunBackend) Capabilities() db.BackendCapabilities {
	return db.BackendCapabilities{}
}

func (*duckBunBackend) SessionQueryDialect() db.QueryDialect {
	return db.PortableBunSessionQueryDialect()
}

func (*duckBunBackend) SessionVersion(
	ctx context.Context, store bun.IDB, id string,
) (int, int64, error) {
	return db.FileSessionVersion(ctx, store, id)
}

func (b *duckBunBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.store.handleMu.RLock()
	defer b.store.handleMu.RUnlock()
	return fn(b.store.bun)
}

// ConsistentView keeps one coherent database image for a composite read. Local
// serving mirrors are immutable for the lifetime of their guarded handle,
// direct mutable connections use a read transaction, and Quack retries the
// complete callback across a server-side mirror replacement detected through
// its opaque generation token. Callbacks must stage their output because a
// retry can replay them before ConsistentView returns.
func (b *duckBunBackend) ConsistentView(
	ctx context.Context, fn func(bun.IDB) error,
) error {
	b.store.handleMu.RLock()
	defer b.store.handleMu.RUnlock()
	if b.store.connectionKind == duckDBQuackClientConnection {
		return stableDuckDBView(
			ctx,
			func(ctx context.Context) (string, error) {
				return b.store.mirrorReadToken(ctx)
			},
			func() error { return fn(b.store.bun) },
		)
	}
	if b.store.path != "" {
		return fn(b.store.bun)
	}
	return b.store.bun.RunInTx(
		ctx, nil,
		func(_ context.Context, tx bun.Tx) error { return fn(tx) },
	)
}

func (s *Store) mirrorReadToken(ctx context.Context) (string, error) {
	var token string
	err := queryDuckDBRowContext(
		ctx, s.duck, s.connectionKind, s.quack,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		mirrorGenerationMetadataKey,
	).Scan(&token)
	if err != nil {
		return "", fmt.Errorf("reading duckdb mirror consistency token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf(
			"reading duckdb mirror consistency token: %s is empty",
			mirrorGenerationMetadataKey,
		)
	}
	return token, nil
}

const stableDuckDBViewAttempts = 3

func stableDuckDBView(
	ctx context.Context,
	readToken func(context.Context) (string, error),
	view func() error,
) error {
	for range stableDuckDBViewAttempts {
		before, err := readToken(ctx)
		if err != nil {
			return err
		}
		viewErr := view()
		after, err := readToken(ctx)
		if err != nil {
			return err
		}
		if before == after {
			return viewErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("duckdb mirror changed during %d read attempts", stableDuckDBViewAttempts)
}

func (*duckBunBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return db.ErrReadOnly
}

func (s *Store) initializeBun() {
	var options []bun.DBOption
	if s.connectionKind == duckDBQuackClientConnection {
		options = append(options, bun.WithConnResolver(newQuackBunResolver(
			s.duck,
			func(ctx context.Context, query string) (*sql.Rows, error) {
				return s.quack.queryRemote(ctx, query, true)
			},
		)))
	}
	s.bun = bun.NewDB(s.duck, bundialect.New(), options...)
	s.BunStore = db.NewBunStore(&duckBunBackend{store: s})
}

func (s *Store) viewBun(
	ctx context.Context, fn func(bun.IDB) error,
) error {
	return (&duckBunBackend{store: s}).View(ctx, fn)
}

// NewStore opens a local DuckDB mirror file as a db.Store. The handle is
// read-only, so a serving Store coexists with other read-only opens (other
// serve processes, push probes) and never blocks a push's probe; the Store's
// db.Store surface is read-only anyway (see ReadOnly).
//
// The file identity is captured BEFORE the mirror is opened, matching
// checkMirrorReplacement's stat-then-open order. If a rebuild swaps the
// file inside the stat/open window, the connection serves the new
// generation while fileInfo still describes the old one, so the
// replacement watcher fires once and reopens onto the file it is already
// serving — a harmless extra reopen. The reverse order inverts the race:
// the connection serves the old generation while fileInfo describes the
// new one, so the watcher never sees a change and the Store serves stale
// data until the next rebuild.
func NewStore(path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("statting duckdb mirror %s: %w", path, err)
	}
	PrimeFileIdentity(info)
	conn, err := OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	store := &Store{path: path, duck: conn, fileInfo: info}
	store.initializeBun()
	return store, nil
}

// NewStoreFromDB wraps an existing DuckDB connection.
func NewStoreFromDB(conn *sql.DB) *Store {
	store := &Store{duck: conn}
	store.initializeBun()
	return store
}

// DB returns the current handle under a read lock. Callers that hold onto
// the returned *sql.DB across a mirror replacement keep using the old
// handle until they call DB() again; this is acceptable for the existing
// callers, which only use DB() once at startup for compat checks.
func (s *Store) DB() *sql.DB {
	s.handleMu.RLock()
	defer s.handleMu.RUnlock()
	return s.duck
}

// Close closes the current handle and removes its backing alias, then waits
// for any in-flight mirror-replacement check to finish — from opening and
// validating a candidate replacement (see beginReplacementCheck) through
// retiring the handle a swap replaced (see swapHandle). Without the wait,
// Close could return while a check still held a freshly opened handle and
// its reopen alias on disk. Close is idempotent; the closed flag also
// rejects checks that have not started yet and tells a swap that loses the
// race to Close to discard its freshly opened handle instead of installing
// it.
func (s *Store) Close() error {
	s.handleMu.Lock()
	if s.closed {
		s.handleMu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.duck
	alias := s.aliasPath
	s.handleMu.Unlock()

	err := conn.Close()
	removeMirrorAlias(alias)
	s.retiring.Wait()
	return err
}

// queryContext runs a read query against the current handle. The read
// lock is held across the query START, not just a handle snapshot: a
// snapshot taken under a released lock could be Close()d by
// WatchMirrorReplacement's swapHandle before QueryContext begins,
// surfacing as intermittent "sql: database is closed" errors during
// mirror adoption. Once QueryContext returns, the *sql.Rows holds a
// checked-out connection that database/sql keeps alive across DB.Close
// (busy connections are only closed when returned to the pool), so
// iterating the rows after the lock is released is safe. The quack-remote
// path performs an HTTP round trip under this read lock; that is
// acceptable because replacement swaps only occur for local mirrors.
func (s *Store) queryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	s.handleMu.RLock()
	defer s.handleMu.RUnlock()
	return queryDuckDBContext(ctx, s.duck, s.connectionKind, s.quack, query, args...)
}

// queryRowContext holds the read lock across the query start for the same
// reason as queryContext. sql.DB.QueryRowContext executes the query
// eagerly (the returned row only carries the already-fetched result), so
// releasing the lock before Scan is safe.
func (s *Store) queryRowContext(
	ctx context.Context, query string, args ...any,
) interface{ Scan(...any) error } {
	s.handleMu.RLock()
	defer s.handleMu.RUnlock()
	return queryDuckDBRowContext(ctx, s.duck, s.connectionKind, s.quack, query, args...)
}

func queryDuckDBContext(
	ctx context.Context,
	duck *sql.DB,
	connectionKind duckDBConnectionKind,
	quack *quackClient,
	query string,
	args ...any,
) (*sql.Rows, error) {
	if connectionKind != duckDBQuackClientConnection {
		return duck.QueryContext(ctx, query, args...)
	}
	sqlText, err := duckSQLWithArgs(query, args...)
	if err != nil {
		return nil, err
	}
	if quack != nil {
		return quack.queryRemote(ctx, sqlText, true)
	}
	return duck.QueryContext(
		ctx,
		"SELECT * FROM "+quackAttachmentName+".query(?)",
		sqlText,
	)
}

func queryDuckDBRowContext(
	ctx context.Context,
	duck *sql.DB,
	connectionKind duckDBConnectionKind,
	quack *quackClient,
	query string,
	args ...any,
) interface{ Scan(...any) error } {
	if connectionKind != duckDBQuackClientConnection {
		return duck.QueryRowContext(ctx, query, args...)
	}
	rows, err := queryDuckDBContext(ctx, duck, connectionKind, quack, query, args...)
	return duckSingleRow{rows: rows, err: err}
}

type duckSingleRow struct {
	rows *sql.Rows
	err  error
}

func (r duckSingleRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Err()
}

func (s *Store) ReadOnly() bool { return true }

func formatDBTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func (s *Store) HasFTS() bool { return true }

// HasSemantic returns false: the DuckDB store has no VectorSearcher seam
// yet, so SearchContent rejects "semantic"/"hybrid" modes up front with
// db.ErrSemanticUnavailable.
func (s *Store) HasSemantic() bool { return false }

func (s *Store) Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}
	if f.Query == "" {
		return db.SearchPage{}, nil
	}
	// plainTerm is the de-quoted query joined back into one string. It feeds the
	// name-branch ILIKE (matching the typed text against the short session name)
	// and centers the message snippet via match_pos, mirroring SQLite's
	// plainQuery and PostgreSQL's plainTerm. terms is the per-term
	// decomposition: every term must appear in the message content (AND),
	// matching SQLite FTS5's implicit AND so the same user query behaves
	// identically across backends. An explicit exact phrase (user-supplied
	// leading quote) collapses to a single term, preserving the exact-phrase
	// opt-in.
	plainTerm := db.StripFTSQuotes(f.Query)
	terms := db.FTSTerms(f.Query)
	if plainTerm == "" || len(terms) == 0 {
		return db.SearchPage{}, nil
	}
	// firstTerm anchors INSTR-based ordering and snippet centering.
	firstTerm := terms[0]
	namePattern := "%" + db.EscapeLikePattern(plainTerm) + "%"
	project := ""
	nameProject := ""
	args := []any{firstTerm, firstTerm}

	// Message branch matches every term (AND). Each term gets its own escaped
	// ILIKE placeholder so a multi-word query requires all terms to be present
	// without demanding they be contiguous, exactly like SQLite FTS5.
	termClauses := make([]string, len(terms))
	for i, t := range terms {
		termClauses[i] = "m.content ILIKE ? ESCAPE '\\'"
		args = append(args, "%"+db.EscapeLikePattern(t)+"%")
	}
	msgTermPredicate := strings.Join(termClauses, "\n\t\t\t\tAND ")
	if f.Project != "" {
		project = "AND s.project = ?"
		args = append(args, f.Project)
		nameProject = "AND s.project = ?"
	}
	args = append(args, namePattern, namePattern, namePattern, namePattern)
	if f.Project != "" {
		args = append(args, f.Project)
	}
	orderBy := "match_priority ASC, match_pos ASC, session_ended_at DESC, session_id ASC"
	if f.Sort == "recency" {
		orderBy = "session_ended_at DESC, session_id ASC"
	}
	args = append(args, f.Limit+1, f.Cursor)
	rows, err := s.queryContext(ctx, `
		WITH msg_ranked AS (
			SELECT m.session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				m.ordinal, SUBSTRING(m.content, 1, 200) AS snippet,
				1.0 AS rank, 1 AS match_priority,
				INSTR(LOWER(m.content), LOWER(?)) AS match_pos,
				ROW_NUMBER() OVER (
					PARTITION BY m.session_id
					ORDER BY INSTR(LOWER(m.content), LOWER(?)) ASC,
						m.ordinal ASC, COALESCE(m.id, 0) ASC
				) AS rn
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE `+msgTermPredicate+`
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND `+db.DuckDBSystemPrefixSQL("m.content", "m.role")+`
				`+project+`
		),
		msg_matches AS (
			SELECT session_id, project, agent, name, session_ended_at,
				ordinal, snippet, rank, match_priority, match_pos
			FROM msg_ranked
			WHERE rn = 1
		),
		name_matches AS (
			SELECT s.id AS session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				-1 AS ordinal,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) ILIKE ? ESCAPE '\'
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message ILIKE ? ESCAPE '\'
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet,
				1.0 AS rank, 2 AS match_priority, 0 AS match_pos
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE ? ESCAPE '\'
				OR s.first_message ILIKE ? ESCAPE '\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
						AND mx.is_system = FALSE
						AND `+db.DuckDBSystemPrefixSQL("mx.content", "mx.role")+`
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				`+nameProject+`
		)
		SELECT session_id, project, agent, name,
			session_ended_at, ordinal, snippet, rank
		FROM (
			SELECT * FROM msg_matches
			UNION ALL
			SELECT * FROM name_matches
		) combined
		ORDER BY `+orderBy+`
		LIMIT ? OFFSET ?`,
		args...,
	)
	if err != nil {
		return db.SearchPage{}, fmt.Errorf("duckdb search: %w", err)
	}
	defer rows.Close()
	var results []db.SearchResult
	for rows.Next() {
		var r db.SearchResult
		var ended any
		if err := rows.Scan(&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&ended, &r.Ordinal, &r.Snippet, &r.Rank); err != nil {
			return db.SearchPage{}, err
		}
		r.SessionEndedAt = formatDBTime(ended)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return db.SearchPage{}, err
	}
	page := db.SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func (s *Store) SearchSession(ctx context.Context, sessionID, query string) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	rows, err := s.queryContext(ctx, `
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_id = m.id
		LEFT JOIN tool_result_events tre
			ON tre.session_id = tc.session_id
			AND tre.tool_call_message_ordinal = m.ordinal
			AND tre.call_index = tc.call_index
		WHERE m.session_id = ?
			AND m.is_system = FALSE
			AND `+db.DuckDBSystemPrefixSQL("m.content", "m.role")+`
			AND (m.content ILIKE ? ESCAPE '\'
				OR tc.result_content ILIKE ? ESCAPE '\'
				OR tre.content ILIKE ? ESCAPE '\')
		ORDER BY m.ordinal ASC`,
		sessionID, "%"+db.EscapeLikePattern(query)+"%",
		"%"+db.EscapeLikePattern(query)+"%",
		"%"+db.EscapeLikePattern(query)+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("duckdb session search: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			return nil, err
		}
		out = append(out, ordinal)
	}
	return out, rows.Err()
}

func (s *Store) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return db.ContentSearchPage{}, nil
	}

	// Semantic and hybrid validate Sources themselves (messages only) ahead
	// of the substring/regex/fts source-set default just below, which fills
	// in tool_input/tool_result that neither mode supports -- mirroring
	// internal/db's SearchContent so an empty Sources field is not defaulted
	// out from under ValidateSemanticFilter's empty-or-messages-only check.
	if f.Mode == "semantic" || f.Mode == "hybrid" {
		// Validate input the same way SQLite's semantic/hybrid paths do
		// before reporting the capability gate: an invalid request (bad
		// cursor, non-messages source) must return the same 400
		// SearchInputError on every backend rather than a 501 here and a
		// 400 on SQLite (backend parity, see AGENTS.md).
		if err := db.ValidateSemanticFilter(f); err != nil {
			return db.ContentSearchPage{}, err
		}
		// No VectorSearcher seam on the DuckDB store yet (HasSemantic always
		// false): gate after input validation.
		return db.ContentSearchPage{}, db.NewSemanticUnavailableError(
			"semantic search is not supported by the DuckDB backend",
		)
	}

	if len(f.Sources) == 0 {
		f.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, source := range f.Sources {
		if source != "messages" && source != "tool_input" && source != "tool_result" {
			return db.ContentSearchPage{},
				&db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	switch f.Mode {
	case "", "substring", "regex":
	case "fts":
		f.Sources = []string{"messages"}
	default:
		return db.ContentSearchPage{},
			&db.SearchInputError{Msg: fmt.Sprintf("search: invalid mode %q", f.Mode)}
	}
	matches, err := s.collectContentMatches(ctx, f)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	page := db.ContentSearchPage{Matches: matches}
	if len(matches) > f.Limit {
		page.Matches = matches[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	// Post-truncation derivation, O(page): every lexical match gets its
	// conversation-unit OrdinalRange and lineage fields via the shared
	// batched pass, matching the SQLite and PG backends.
	if err := s.deriveLexicalUnitsDuck(ctx, page.Matches); err != nil {
		return db.ContentSearchPage{}, err
	}
	return page, nil
}

func (s *Store) collectContentMatches(ctx context.Context, f db.ContentSearchFilter) ([]db.ContentMatch, error) {
	if f.Mode != "regex" {
		return s.collectContentSubstringMatches(ctx, f)
	}
	scopeWhere, scopeArgs := db.BuildSessionFilterSQL(contentSessionFilter(f), db.DuckDBQueryDialect())
	pattern := ""
	if f.Mode != "regex" {
		pattern = "%" + db.EscapeLikePattern(f.Pattern) + "%"
	}
	var all []duckContentCandidate
	for _, source := range f.Sources {
		matches, err := s.collectContentSource(ctx, source, scopeWhere, scopeArgs, pattern, f)
		if err != nil {
			return nil, err
		}
		all = append(all, matches...)
	}
	if f.Mode == "regex" {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: invalid regex: %v", err)}
		}
		filtered := all[:0]
		for _, m := range all {
			loc := re.FindStringIndex(m.body)
			if loc != nil {
				m.match.Snippet = duckContentSnippet(f, m.body, loc[0], loc[1])
				filtered = append(filtered, m)
			}
		}
		all = filtered
	}
	sortContentCandidates(all)
	if f.Cursor > 0 {
		if f.Cursor >= len(all) {
			return nil, nil
		}
		all = all[f.Cursor:]
	}
	if len(all) > f.Limit+1 {
		all = all[:f.Limit+1]
	}
	return contentCandidateMatches(all), nil
}

func (s *Store) collectContentSubstringMatches(
	ctx context.Context, f db.ContentSearchFilter,
) ([]db.ContentMatch, error) {
	scopeWhere, scopeArgs := db.BuildSessionFilterSQL(contentSessionFilter(f), db.DuckDBQueryDialect())
	var branches []string
	var args []any
	addSearchArgs := func(column string) string {
		predicate := duckContentSearchPredicate(column, f, &args)
		args = append(args, scopeArgs...)
		return predicate
	}
	for _, source := range f.Sources {
		switch source {
		case "messages":
			sysPred := "TRUE"
			if f.ExcludeSystem {
				sysPred = "m.is_system = FALSE AND " + db.DuckDBSystemPrefixSQL("m.content", "m.role")
			}
			contentPred := addSearchArgs("m.content")
			branches = append(branches, `
				SELECT m.session_id, s.project, s.agent, 'message' AS location,
					m.role, '' AS tool_name, m.ordinal,
					m.timestamp AS ts,
					m.content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					0 AS src, COALESCE(m.id, 0) AS row_id,
					0 AS call_index, 0 AS event_index
				FROM messages m JOIN sessions s ON s.id = m.session_id
				WHERE `+contentPred+`
					AND `+sysPred+`
					AND m.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		case "tool_input":
			inputPred := addSearchArgs("tc.input_json")
			branches = append(branches, `
				SELECT tc.session_id, s.project, s.agent, 'tool_input' AS location,
					'assistant' AS role, tc.tool_name, m.ordinal,
					m.timestamp AS ts,
					tc.input_json AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					1 AS src, COALESCE(tc.id, 0) AS row_id,
					tc.call_index AS call_index, 0 AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				JOIN messages m ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE `+inputPred+`
					AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		case "tool_result":
			contentPred := addSearchArgs("tc.result_content")
			branches = append(branches, `
					SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, tc.tool_name, m.ordinal,
						m.timestamp AS ts,
						tc.result_content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						2 AS src, COALESCE(tc.id, 0) AS row_id,
						tc.call_index AS call_index, 0 AS event_index
					FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
					JOIN messages m ON m.session_id = tc.session_id
						AND m.id = tc.message_id
					WHERE `+contentPred+`
						AND NOT EXISTS (
							SELECT 1 FROM tool_result_events tre
							WHERE tre.session_id = tc.session_id
								AND tre.tool_use_id = tc.tool_use_id
								AND tc.tool_use_id <> ''
						)
						AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			eventPred := addSearchArgs("tre.content")
			branches = append(branches, `
					SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, '' AS tool_name,
						tre.tool_call_message_ordinal AS ordinal,
						tre.timestamp AS ts,
						tre.content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						3 AS src, COALESCE(tre.id, 0) AS row_id,
						tre.call_index AS call_index,
						tre.event_index AS event_index
					FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
					WHERE `+eventPred+`
						AND tre.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
		default:
			return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	if len(branches) == 0 {
		return nil, nil
	}
	query := `
		SELECT session_id, project, agent, location, role, tool_name,
			ordinal, ts, body
		FROM (` + strings.Join(branches, " UNION ALL ") + `)
		ORDER BY sort_ts DESC, session_id ASC, ordinal ASC,
			src ASC, row_id ASC, call_index ASC, event_index ASC
		LIMIT ? OFFSET ?`
	args = append(args, f.Limit+1, f.Cursor)
	return s.scanContentMatches(ctx, query, args, func(body string) string {
		if f.Mode == "fts" {
			start, end := db.FTSSnippetRange(f.Pattern, body)
			return duckContentSnippet(f, body, start, end)
		}
		off := max(db.CaseInsensitiveIndex(body, f.Pattern), 0)
		return duckContentSnippet(f, body, off, min(off+len(f.Pattern), len(body)))
	})
}

func duckContentSearchPredicate(
	column string, f db.ContentSearchFilter, args *[]any,
) string {
	if f.Mode != "fts" {
		*args = append(*args, "%"+db.EscapeLikePattern(f.Pattern)+"%")
		return column + ` ILIKE ? ESCAPE '\'`
	}

	terms := db.FTSTerms(db.PrepareFTSQuery(f.Pattern))
	clauses := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" {
			continue
		}
		*args = append(*args, "%"+db.EscapeLikePattern(term)+"%")
		clauses = append(clauses, column+` ILIKE ? ESCAPE '\'`)
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return strings.Join(clauses, " AND ")
}

func duckContentSnippet(f db.ContentSearchFilter, body string, start, end int) string {
	lo, hi := duckSnippetBounds(body, start, end, 60)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

func duckSnippetBounds(text string, start, end, radius int) (int, int) {
	lo := max(start-radius, 0)
	hi := min(end+radius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

type duckContentCandidate struct {
	match      db.ContentMatch
	body       string
	sortTS     string
	sortTime   time.Time
	hasSort    bool
	sourceRank int
	rowID      int64
	callIndex  int
	eventIndex int
}

func sortContentCandidates(all []duckContentCandidate) {
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].hasSort && all[j].hasSort && !all[i].sortTime.Equal(all[j].sortTime) {
			return all[i].sortTime.After(all[j].sortTime)
		}
		if all[i].hasSort != all[j].hasSort {
			return all[i].hasSort
		}
		if all[i].sortTS != all[j].sortTS {
			return all[i].sortTS > all[j].sortTS
		}
		if all[i].match.SessionID != all[j].match.SessionID {
			return all[i].match.SessionID < all[j].match.SessionID
		}
		if all[i].match.Ordinal != all[j].match.Ordinal {
			return all[i].match.Ordinal < all[j].match.Ordinal
		}
		if all[i].sourceRank != all[j].sourceRank {
			return all[i].sourceRank < all[j].sourceRank
		}
		if all[i].rowID != all[j].rowID {
			return all[i].rowID < all[j].rowID
		}
		if all[i].callIndex != all[j].callIndex {
			return all[i].callIndex < all[j].callIndex
		}
		if all[i].eventIndex != all[j].eventIndex {
			return all[i].eventIndex < all[j].eventIndex
		}
		if all[i].match.Location != all[j].match.Location {
			return all[i].match.Location < all[j].match.Location
		}
		if all[i].match.ToolName != all[j].match.ToolName {
			return all[i].match.ToolName < all[j].match.ToolName
		}
		if all[i].match.Role != all[j].match.Role {
			return all[i].match.Role < all[j].match.Role
		}
		if all[i].match.Timestamp != all[j].match.Timestamp {
			return all[i].match.Timestamp < all[j].match.Timestamp
		}
		if all[i].match.Project != all[j].match.Project {
			return all[i].match.Project < all[j].match.Project
		}
		if all[i].match.Agent != all[j].match.Agent {
			return all[i].match.Agent < all[j].match.Agent
		}
		return all[i].match.Snippet < all[j].match.Snippet
	})
}

func contentCandidateMatches(candidates []duckContentCandidate) []db.ContentMatch {
	out := make([]db.ContentMatch, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.match
	}
	return out
}

func contentSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	return db.SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, GitBranch: f.GitBranch, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		Timezone:         f.Timezone,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

func (s *Store) collectContentSource(
	ctx context.Context, source, scopeWhere string, scopeArgs []any,
	pattern string, f db.ContentSearchFilter,
) ([]duckContentCandidate, error) {
	var query string
	var orderBy string
	args := append([]any{}, scopeArgs...)
	switch source {
	case "messages":
		query = `SELECT m.session_id, s.project, s.agent, 'message',
			m.role, '', m.ordinal, m.timestamp,
			m.content,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
			0 AS src, COALESCE(m.id, 0) AS row_id,
			0 AS call_index, 0 AS event_index
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE m.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)`
		if f.Mode != "regex" {
			query += ` AND m.content ILIKE ? ESCAPE '\'`
			args = append(args, pattern)
		}
		if f.ExcludeSystem {
			query += " AND m.is_system = FALSE AND " + db.DuckDBSystemPrefixSQL("m.content", "m.role")
		}
		orderBy = "m.session_id, m.ordinal, COALESCE(m.id, 0)"
	case "tool_input":
		query = `SELECT tc.session_id, s.project, s.agent, 'tool_input',
				'assistant', tc.tool_name, m.ordinal, m.timestamp,
				COALESCE(tc.input_json, ''),
				COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
				1 AS src, COALESCE(tc.id, 0) AS row_id,
				tc.call_index AS call_index, 0 AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				JOIN messages m ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)`
		if f.Mode != "regex" {
			query += ` AND tc.input_json ILIKE ? ESCAPE '\'`
			args = append(args, pattern)
		}
		orderBy = "tc.session_id, m.ordinal, COALESCE(tc.id, 0), tc.call_index"
	case "tool_result":
		args = append([]any{}, scopeArgs...)
		contentPred := "TRUE"
		eventPred := "TRUE"
		if f.Mode != "regex" {
			contentPred = `tc.result_content ILIKE ? ESCAPE '\'`
			eventPred = `tre.content ILIKE ? ESCAPE '\'`
		}
		query = `SELECT session_id, project, agent, location, role,
				tool_name, ordinal, ts, body, sort_ts, src, row_id, call_index, event_index
				FROM (
					SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, tc.tool_name, m.ordinal,
						m.timestamp AS ts,
						COALESCE(tc.result_content, '') AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						2 AS src,
						COALESCE(tc.id, 0) AS row_id,
						tc.call_index AS call_index, 0 AS event_index
					FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
					JOIN messages m ON m.session_id = tc.session_id
						AND m.id = tc.message_id
					WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
						AND ` + contentPred + `
						AND NOT EXISTS (
							SELECT 1 FROM tool_result_events tre
							WHERE tre.session_id = tc.session_id
								AND tre.tool_use_id = tc.tool_use_id
								AND tc.tool_use_id <> ''
						)
					UNION ALL
					SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, '' AS tool_name,
						tre.tool_call_message_ordinal AS ordinal,
						tre.timestamp AS ts,
						tre.content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						3 AS src,
						COALESCE(tre.id, 0) AS row_id,
						tre.call_index AS call_index, tre.event_index
					FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
					WHERE tre.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
						AND ` + eventPred + `
				)`
		if f.Mode != "regex" {
			args = append(args, pattern)
		}
		args = append(args, scopeArgs...)
		if f.Mode != "regex" {
			args = append(args, pattern)
		}
		orderBy = "session_id, ordinal, src, row_id, call_index, event_index"
	default:
		return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
	}
	query += ` ORDER BY ` + orderBy
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("duckdb content search: %w", err)
	}
	return scanDuckContentCandidateRows(rows)
}

func (s *Store) scanContentMatches(
	ctx context.Context, query string, args []any, makeSnippet func(string) string,
) ([]db.ContentMatch, error) {
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("duckdb content search: %w", err)
	}
	return scanDuckContentRows(rows, makeSnippet)
}

func scanDuckContentRows(rows *sql.Rows, makeSnippet func(string) string) ([]db.ContentMatch, error) {
	defer rows.Close()
	var out []db.ContentMatch
	for rows.Next() {
		var m db.ContentMatch
		var body string
		var ts any
		if err := rows.Scan(&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&ts, &body); err != nil {
			return nil, err
		}
		m.Timestamp = formatDBTime(ts)
		m.Snippet = makeSnippet(body)
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanDuckContentCandidateRows(rows *sql.Rows) ([]duckContentCandidate, error) {
	defer rows.Close()
	var out []duckContentCandidate
	for rows.Next() {
		var candidate duckContentCandidate
		var sortTS any
		var ts any
		if err := rows.Scan(
			&candidate.match.SessionID, &candidate.match.Project,
			&candidate.match.Agent, &candidate.match.Location,
			&candidate.match.Role, &candidate.match.ToolName,
			&candidate.match.Ordinal, &ts,
			&candidate.body, &sortTS, &candidate.sourceRank,
			&candidate.rowID, &candidate.callIndex, &candidate.eventIndex,
		); err != nil {
			return nil, err
		}
		candidate.match.Timestamp = formatDBTime(ts)
		candidate.sortTS = formatDBTime(sortTS)
		candidate.sortTime, candidate.hasSort = parseAnalyticsTime(candidate.sortTS)
		candidate.match.Snippet = candidate.body
		out = append(out, candidate)
	}
	return out, rows.Err()
}
