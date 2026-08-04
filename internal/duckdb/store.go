package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
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
	return db.BackendCapabilities{
		FullText:      duckFullTextCapability{},
		SessionSearch: duckFullTextCapability{},
		Semantic: db.NewVectorSemanticCapability(
			func() db.VectorSearcher { return nil },
			func() error {
				return db.NewSemanticUnavailableError(
					"semantic search is not supported by the DuckDB backend",
				)
			},
		),
	}
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
	if err := validateMirrorGeneration(token); err != nil {
		return "", fmt.Errorf("reading duckdb mirror consistency token: %w", err)
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

type duckFullTextCapability struct{}

type duckSearchHitProjection struct {
	SessionID string  `bun:"session_id"`
	Ordinal   int     `bun:"ordinal"`
	Snippet   string  `bun:"snippet"`
	Rank      float64 `bun:"rank"`
}

func (duckFullTextCapability) Available() bool { return true }

// HasSemantic returns false: the DuckDB store has no VectorSearcher seam
// yet, so SearchContent rejects "semantic"/"hybrid" modes up front with
// db.ErrSemanticUnavailable.
func (s *Store) HasSemantic() bool { return s.bunSearchStore().HasSemantic() }

func (s *Store) bunSearchStore() *db.BunStore {
	if s.BunStore != nil {
		return s.BunStore
	}
	return db.NewBunStore(&duckBunBackend{store: s})
}

func (duckFullTextCapability) Search(
	ctx context.Context, store bun.IDB, f db.SearchFilter,
) ([]db.SearchHit, error) {
	if f.Query == "" {
		return nil, nil
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
		return nil, nil
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
	args = append(args, f.Limit, f.Cursor)
	query := `
		WITH msg_ranked AS (
			SELECT m.session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				m.ordinal, SUBSTRING(m.content, 1, 200) AS snippet,
				CAST(1.0 AS DOUBLE) AS rank, 1 AS match_priority,
				INSTR(LOWER(m.content), LOWER(?)) AS match_pos,
				ROW_NUMBER() OVER (
					PARTITION BY m.session_id
					ORDER BY INSTR(LOWER(m.content), LOWER(?)) ASC,
						m.ordinal ASC, COALESCE(m.id, 0) ASC
				) AS rn
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE ` + msgTermPredicate + `
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND ` + db.DuckDBSystemPrefixSQL("m.content", "m.role") + `
				` + project + `
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
				CAST(1.0 AS DOUBLE) AS rank, 2 AS match_priority, 0 AS match_pos
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE ? ESCAPE '\'
				OR s.first_message ILIKE ? ESCAPE '\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
						AND mx.is_system = FALSE
						AND ` + db.DuckDBSystemPrefixSQL("mx.content", "mx.role") + `
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				` + nameProject + `
		)
		SELECT session_id, ordinal, snippet, rank
		FROM (
			SELECT * FROM msg_matches
			UNION ALL
			SELECT * FROM name_matches
		) combined
		ORDER BY ` + orderBy + `
		LIMIT ? OFFSET ?`
	var rows []duckSearchHitProjection
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("duckdb search: %w", err)
	}
	hits := make([]db.SearchHit, len(rows))
	for i, row := range rows {
		hits[i] = db.SearchHit{
			SessionID: row.SessionID, Ordinal: row.Ordinal,
			Snippet: row.Snippet, Rank: row.Rank,
		}
	}
	return hits, nil
}

func (duckFullTextCapability) SearchSession(
	ctx context.Context, store bun.IDB, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	var rows []struct {
		Ordinal int `bun:"ordinal"`
	}
	err := store.NewRaw(`
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
	).Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("duckdb session search: %w", err)
	}
	out := make([]int, len(rows))
	for i, row := range rows {
		out[i] = row.Ordinal
	}
	return out, nil
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
		return s.bunSearchStore().SearchContent(ctx, f)
	}
	return s.BunStore.SearchContent(ctx, f)
}
