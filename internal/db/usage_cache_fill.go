package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/usagefacts"
)

const usageFillMaxAttempts = 3
const usageFillInstallBatchSize = 256
const usageCursorCopyBatchSize = 1_000

var errUsageCacheSourceChanged = errors.New("usage cache source archive changed")

type usageFillResult struct {
	InstallRevision int64
	Deleted         bool
	source          usageSourceVersion
}

type usageFillCall struct {
	done   chan struct{}
	result usageFillResult
	err    error
}

type usageCursorFillCall struct {
	done chan struct{}
	err  error
}

// usageFillObserver exposes lifecycle boundaries to deterministic tests. Its
// callbacks must never be used for production work.
type usageFillObserver struct {
	beforeExtract    func([]usageSourceVersion)
	afterExtract     func([]usageSourceVersion)
	beforeInstall    func([]usageSourceVersion)
	afterMaintenance func()
}

type usageFillCoordinator struct {
	archive *DB
	cache   *usageCache
	ctx     context.Context
	cancel  context.CancelFunc

	mu           sync.Mutex
	calls        map[string]*usageFillCall
	cursorCall   *usageCursorFillCall
	cursorTarget int64
	observer     usageFillObserver
	notify       chan usageFillNotification
	done         chan struct{}
}

type usageFillNotification struct {
	sessionID string
	cursor    bool
}

func newUsageFillCoordinator(
	parent context.Context, archive *DB, cache *usageCache,
) *usageFillCoordinator {
	ctx, cancel := context.WithCancel(parent)
	c := &usageFillCoordinator{
		archive: archive, cache: cache, ctx: ctx, cancel: cancel,
		calls:  make(map[string]*usageFillCall),
		notify: make(chan usageFillNotification, 1024),
		done:   make(chan struct{}),
	}
	go c.runNotifications()
	return c
}

func (c *usageFillCoordinator) Close() {
	c.cancel()
	<-c.done
}

// Ensure makes the requested source versions and Cursor prefix available in
// the cache. Fill work uses the coordinator context, so cancelling a waiter
// detaches that waiter without cancelling shared progress.
func (c *usageFillCoordinator) Ensure(
	ctx context.Context, versions []usageSourceVersion, cursorHighWater int64,
) (map[string]usageFillResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make(map[string]usageFillResult, len(versions))
	waiting := make(map[string]*usageFillCall)
	owned := make([]usageSourceVersion, 0, len(versions))
	for _, version := range versions {
		cached, ok, err := c.cachedSession(version.SessionID)
		if err != nil {
			return nil, err
		}
		if ok && cached.source.Equal(version) {
			results[version.SessionID] = cached
			continue
		}

		c.mu.Lock()
		key := usageFillCallKey(version)
		call := c.calls[key]
		if call == nil {
			call = &usageFillCall{done: make(chan struct{})}
			c.calls[key] = call
			owned = append(owned, version)
		}
		waiting[version.SessionID] = call
		c.mu.Unlock()
	}
	if len(owned) > 0 {
		go c.fillSessions(owned)
	}

	for id, call := range waiting {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			results[id] = call.result
		}
	}
	for _, version := range versions {
		result, ok := results[version.SessionID]
		if ok && !result.Deleted && !result.source.Equal(version) {
			return nil, fmt.Errorf(
				"%w while filling session %s",
				errUsageCacheSourceChanged, version.SessionID,
			)
		}
	}
	if cursorHighWater > 0 {
		if err := c.ensureCursor(ctx, cursorHighWater); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func usageFillCallKey(version usageSourceVersion) string {
	return strings.Join([]string{
		version.SessionID, version.SyncMarker, version.TranscriptRevision,
		version.UsageEventFingerprint,
	}, "\x00")
}

func (c *usageFillCoordinator) cachedSession(
	sessionID string,
) (usageFillResult, bool, error) {
	var result usageFillResult
	err := c.cache.db.QueryRow(`
		SELECT source_sync_marker, source_transcript_rev,
		       usage_event_fingerprint, install_revision
		FROM usage_cached_sessions WHERE session_id = ?`, sessionID).Scan(
		&result.source.SyncMarker, &result.source.TranscriptRevision,
		&result.source.UsageEventFingerprint, &result.InstallRevision,
	)
	result.source.SessionID = sessionID
	if errors.Is(err, sql.ErrNoRows) {
		return usageFillResult{}, false, nil
	}
	if err != nil {
		return usageFillResult{}, false,
			fmt.Errorf("reading cached usage session %s: %w", sessionID, err)
	}
	return result, true, nil
}

func (c *usageFillCoordinator) fillSessions(versions []usageSourceVersion) {
	results, err := c.fillSessionsDetached(c.ctx, versions)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, version := range versions {
		key := usageFillCallKey(version)
		call := c.calls[key]
		if call == nil {
			continue
		}
		call.result = results[version.SessionID]
		call.err = err
		delete(c.calls, key)
		close(call.done)
	}
}

func (c *usageFillCoordinator) fillSessionsDetached(
	ctx context.Context,
	versions []usageSourceVersion,
) (map[string]usageFillResult, error) {
	pending := append([]usageSourceVersion(nil), versions...)
	results := make(map[string]usageFillResult, len(versions))
	var spool *usageFactSpool
	for attempt := 1; len(pending) > 0 && attempt <= usageFillMaxAttempts; attempt++ {
		if spool == nil {
			if c.observer.beforeExtract != nil {
				c.observer.beforeExtract(append([]usageSourceVersion(nil), pending...))
			}
			var err error
			spool, err = c.extractSessions(ctx, pending)
			if err != nil {
				return nil, err
			}
			if c.observer.afterExtract != nil {
				c.observer.afterExtract(append([]usageSourceVersion(nil), pending...))
			}
		}
		if c.observer.beforeInstall != nil {
			c.observer.beforeInstall(append([]usageSourceVersion(nil), pending...))
		}
		installed, retry, err := c.installSpool(ctx, spool, pending)
		if err != nil && isUsageCacheBusy(err) {
			continue
		}
		closeErr := spool.Close()
		spool = nil
		if err != nil {
			return nil, errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		maps.Copy(results, installed)
		pending = retry
	}
	if spool != nil {
		_ = spool.Close()
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf(
			"usage cache fill could not verify archive state after %d attempts for %d sessions",
			usageFillMaxAttempts, len(pending),
		)
	}
	return results, nil
}

// FillBackground performs cancellable, non-single-flight work for the daemon
// coverage pass. Foreground requests therefore never inherit the background
// owner's cancellation and can independently prioritize the sessions they
// need.
func (c *usageFillCoordinator) FillBackground(
	ctx context.Context, versions []usageSourceVersion, cursorHighWater int64,
) (map[string]usageFillResult, error) {
	results := make(map[string]usageFillResult, len(versions))
	pending := make([]usageSourceVersion, 0, len(versions))
	for _, version := range versions {
		cached, ok, err := c.cachedSession(version.SessionID)
		if err != nil {
			return nil, err
		}
		if ok && cached.source.Equal(version) {
			results[version.SessionID] = cached
			continue
		}
		pending = append(pending, version)
	}
	installed, err := c.fillSessionsDetached(ctx, pending)
	if err != nil {
		return nil, err
	}
	maps.Copy(results, installed)
	if cursorHighWater > 0 {
		if err := c.copyCursorThrough(ctx, cursorHighWater); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func isUsageCacheBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "sqlite_busy")
}

type usageFactSpool struct {
	db   *sql.DB
	path string
}

func newUsageFactSpool() (*usageFactSpool, error) {
	file, err := os.CreateTemp("", "agentsview-usage-facts-spool-*.db")
	if err != nil {
		return nil, fmt.Errorf("creating usage fact spool: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("securing usage fact spool: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("closing usage fact spool: %w", err)
	}
	database, err := sql.Open("sqlite3", makeDSN(path, false))
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`
		PRAGMA journal_mode=MEMORY;
		PRAGMA synchronous=OFF;
		PRAGMA temp_store=MEMORY`); err != nil {
		_ = database.Close()
		_ = removeUsageCacheFiles(path)
		return nil, fmt.Errorf("configuring usage fact spool: %w", err)
	}
	if _, err := database.Exec(`
		CREATE TABLE facts (
			session_id TEXT NOT NULL, fact_index INTEGER NOT NULL,
			source TEXT NOT NULL, message_ordinal INTEGER,
			timestamp_ms INTEGER, timestamp_ns INTEGER, raw_timestamp TEXT NOT NULL,
			uses_session_start INTEGER NOT NULL, model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			cache_creation_tokens INTEGER NOT NULL,
			cache_read_tokens INTEGER NOT NULL,
			web_search_requests INTEGER NOT NULL,
			reported_cost_microdollars INTEGER, cost_source TEXT NOT NULL,
			request_scoped INTEGER NOT NULL,
			claude_message_id TEXT NOT NULL, claude_request_id TEXT NOT NULL,
			source_uuid TEXT NOT NULL, usage_dedup_key TEXT NOT NULL,
			token_eligible INTEGER NOT NULL, activity_eligible INTEGER NOT NULL,
			PRIMARY KEY(session_id, fact_index)
		) WITHOUT ROWID`); err != nil {
		_ = database.Close()
		_ = removeUsageCacheFiles(path)
		return nil, fmt.Errorf("initializing usage fact spool: %w", err)
	}
	return &usageFactSpool{db: database, path: path}, nil
}

func (s *usageFactSpool) Close() error {
	return errors.Join(s.db.Close(), removeUsageCacheFiles(s.path))
}

func (c *usageFillCoordinator) extractSessions(
	ctx context.Context, versions []usageSourceVersion,
) (*usageFactSpool, error) {
	spool, err := newUsageFactSpool()
	if err != nil {
		return nil, err
	}
	fail := true
	defer func() {
		if fail {
			_ = spool.Close()
		}
	}()
	conn, err := c.archive.getReader().Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinning archive usage fill connection: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting archive usage fill snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE IF NOT EXISTS usage_fill_sessions(
			session_id TEXT PRIMARY KEY
		) WITHOUT ROWID;
		DELETE FROM usage_fill_sessions`,
	); err != nil {
		return nil, fmt.Errorf("creating usage fill session set: %w", err)
	}
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO usage_fill_sessions(session_id) VALUES (?)`)
	if err != nil {
		return nil, err
	}
	for _, version := range versions {
		if _, err := insert.ExecContext(ctx, version.SessionID); err != nil {
			_ = insert.Close()
			return nil, err
		}
	}
	if err := insert.Close(); err != nil {
		return nil, err
	}

	spoolTx, err := spool.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = spoolTx.Rollback() }()
	indexes := make(map[string]int, len(versions))
	spoolWriter := usageFactSpoolWriter{ctx: ctx, tx: spoolTx}
	if err := extractUsageMessageFacts(ctx, tx, &spoolWriter, indexes); err != nil {
		return nil, err
	}
	if err := extractUsageActivityFacts(ctx, tx, &spoolWriter, indexes); err != nil {
		return nil, err
	}
	if err := extractUsageEventFacts(ctx, tx, &spoolWriter, indexes); err != nil {
		return nil, err
	}
	if err := spoolWriter.Flush(); err != nil {
		return nil, err
	}
	if err := spoolTx.Commit(); err != nil {
		return nil, fmt.Errorf("committing usage fact spool: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("closing archive usage fill snapshot: %w", err)
	}
	fail = false
	return spool, nil
}

func extractUsageMessageFacts(
	ctx context.Context, archive *sql.Tx, spool *usageFactSpoolWriter,
	indexes map[string]int,
) error {
	rows, err := archive.QueryContext(ctx, usageFillMessageFactsSQL)
	if err != nil {
		return fmt.Errorf("extracting token usage messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var input usagefacts.MessageInput
		if err := rows.Scan(
			&sessionID, &input.Ordinal, &input.Role, &input.Timestamp,
			&input.Model, &input.TokenUsage, &input.ClaudeMessageID,
			&input.ClaudeRequestID, &input.SourceUUID,
		); err != nil {
			return err
		}
		fact, ok := usagefacts.FromMessage(input)
		if ok {
			if err := spool.Add(sessionID, indexes[sessionID], fact); err != nil {
				return err
			}
			indexes[sessionID]++
		}
	}
	return rows.Err()
}

func extractUsageActivityFacts(
	ctx context.Context, archive *sql.Tx, spool *usageFactSpoolWriter,
	indexes map[string]int,
) error {
	rows, err := archive.QueryContext(ctx, usageFillActivityFactsSQL)
	if err != nil {
		return fmt.Errorf("extracting relaxed activity messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var input usagefacts.MessageInput
		if err := rows.Scan(
			&sessionID, &input.Ordinal, &input.Role, &input.Timestamp,
			&input.Model,
		); err != nil {
			return err
		}
		fact, ok := usagefacts.FromMessage(input)
		if ok {
			if err := spool.Add(sessionID, indexes[sessionID], fact); err != nil {
				return err
			}
			indexes[sessionID]++
		}
	}
	return rows.Err()
}

func extractUsageEventFacts(
	ctx context.Context, archive *sql.Tx, spool *usageFactSpoolWriter,
	indexes map[string]int,
) error {
	rows, err := archive.QueryContext(ctx, usageFillEventFactsSQL)
	if err != nil {
		return fmt.Errorf("extracting usage events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var ordinal, cost sql.NullInt64
		var eventID int64
		var rawDedupKey string
		var input usagefacts.EventInput
		if err := rows.Scan(
			&eventID, &sessionID, &ordinal, &input.Source, &input.Model,
			&input.InputTokens, &input.OutputTokens, &input.CacheCreationTokens,
			&input.CacheReadTokens, &input.ReasoningTokens, &cost,
			&input.CostSource, &input.Timestamp, &rawDedupKey,
		); err != nil {
			return err
		}
		input.DedupKey = usagefacts.EventDedupKey(
			sessionID, input.Source, rawDedupKey, eventID,
		)
		if ordinal.Valid {
			value := int(ordinal.Int64)
			input.MessageOrdinal = &value
		}
		if cost.Valid {
			value := cost.Int64
			input.ReportedCostMicrodollars = &value
		}
		fact, ok := usagefacts.FromEvent(input)
		if ok {
			if err := spool.Add(sessionID, indexes[sessionID], fact); err != nil {
				return err
			}
			indexes[sessionID]++
		}
	}
	return rows.Err()
}

const usageFactSpoolBatchSize = 1_000

type usageFactSpoolRow struct {
	sessionID string
	index     int
	fact      usagefacts.Fact
}

type usageFactSpoolWriter struct {
	ctx  context.Context
	tx   *sql.Tx
	rows []usageFactSpoolRow
}

func (w *usageFactSpoolWriter) Add(
	sessionID string, index int, fact usagefacts.Fact,
) error {
	w.rows = append(w.rows, usageFactSpoolRow{
		sessionID: sessionID, index: index, fact: fact,
	})
	if len(w.rows) < usageFactSpoolBatchSize {
		return nil
	}
	return w.Flush()
}

func (w *usageFactSpoolWriter) Flush() error {
	if len(w.rows) == 0 {
		return nil
	}
	const prefix = `INSERT INTO facts(
		session_id, fact_index, source, message_ordinal, timestamp_ms, timestamp_ns,
		raw_timestamp, uses_session_start, model, input_tokens, output_tokens,
		reasoning_tokens, cache_creation_tokens, cache_read_tokens,
		web_search_requests, reported_cost_microdollars, cost_source,
		request_scoped, claude_message_id, claude_request_id, source_uuid,
		usage_dedup_key, token_eligible, activity_eligible
	) VALUES `
	const placeholders = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	var query strings.Builder
	query.Grow(len(prefix) + len(w.rows)*(len(placeholders)+1))
	query.WriteString(prefix)
	args := make([]any, 0, len(w.rows)*24)
	for rowIndex, row := range w.rows {
		if rowIndex > 0 {
			query.WriteByte(',')
		}
		query.WriteString(placeholders)
		fact := row.fact
		args = append(args,
			row.sessionID, row.index, fact.Source, fact.MessageOrdinal,
			fact.TimestampMillis, fact.TimestampNanos, fact.RawTimestamp,
			boolInt(fact.UsesSessionStart),
			fact.Model, fact.InputTokens, fact.OutputTokens, fact.ReasoningTokens,
			fact.CacheCreationTokens, fact.CacheReadTokens, fact.WebSearchRequests,
			fact.ReportedCostMicrodollars, fact.CostSource,
			boolInt(fact.RequestScoped), fact.ClaudeMessageID,
			fact.ClaudeRequestID, fact.SourceUUID, fact.UsageDedupKey,
			boolInt(fact.TokenEligible), boolInt(fact.ActivityEligible),
		)
	}
	_, err := w.tx.ExecContext(w.ctx, query.String(), args...)
	if err != nil {
		return fmt.Errorf("spooling %d usage facts: %w", len(w.rows), err)
	}
	w.rows = w.rows[:0]
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

const usageFillMessageFactsSQL = `
	SELECT m.session_id, m.ordinal, m.role, COALESCE(m.timestamp, ''),
	       m.model, m.token_usage, m.claude_message_id,
	       m.claude_request_id, m.source_uuid
	FROM usage_fill_sessions f
	CROSS JOIN messages m INDEXED BY idx_messages_usage_session_covering
	  ON m.session_id = f.session_id
	WHERE m.token_usage != '' AND m.model != '' AND m.model != '<synthetic>'
	ORDER BY m.timestamp, m.session_id, m.ordinal`

const usageFillActivityFactsSQL = `
	SELECT m.session_id, m.ordinal, m.role, COALESCE(m.timestamp, ''), m.model
	FROM usage_fill_sessions f
	CROSS JOIN messages m INDEXED BY idx_messages_session_role
	  ON m.session_id = f.session_id
	WHERE m.role = 'assistant' AND m.model != '<synthetic>'
	  AND NOT (m.token_usage != '' AND m.model != '')
	ORDER BY m.session_id, m.ordinal`

const usageFillEventFactsSQL = `
	SELECT e.id, e.session_id, e.message_ordinal, e.source, e.model,
	       e.input_tokens, e.output_tokens, e.cache_creation_input_tokens,
	       e.cache_read_input_tokens, e.reasoning_tokens,
	       e.cost_microdollars, e.cost_source, COALESCE(e.occurred_at, ''),
	       e.dedup_key
	FROM usage_fill_sessions f
	CROSS JOIN usage_events e INDEXED BY idx_usage_events_session
	  ON e.session_id = f.session_id
	WHERE e.model != ''
	ORDER BY e.session_id, COALESCE(e.occurred_at, ''), e.id`

func (c *usageFillCoordinator) installSpool(
	ctx context.Context, spool *usageFactSpool, versions []usageSourceVersion,
) (map[string]usageFillResult, []usageSourceVersion, error) {
	installed := make(map[string]usageFillResult, len(versions))
	var retry []usageSourceVersion
	for start := 0; start < len(versions); start += usageFillInstallBatchSize {
		end := min(start+usageFillInstallBatchSize, len(versions))
		batchInstalled, batchRetry, err := c.installSpoolBatch(
			ctx, spool, versions[start:end],
		)
		if err != nil {
			return nil, versions, err
		}
		maps.Copy(installed, batchInstalled)
		retry = append(retry, batchRetry...)
		if end < len(versions) {
			c.maintainBetweenInstallBatches(ctx)
		}
	}
	return installed, retry, nil
}

func (c *usageFillCoordinator) installSpoolBatch(
	ctx context.Context, spool *usageFactSpool, versions []usageSourceVersion,
) (map[string]usageFillResult, []usageSourceVersion, error) {
	conn, err := c.cache.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx,
		`ATTACH DATABASE ? AS usage_fill_spool`, spool.path,
	); err != nil {
		return nil, nil, fmt.Errorf("attaching usage fact spool: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(),
			`DETACH DATABASE usage_fill_spool`)
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, versions, fmt.Errorf("locking usage cache for fill: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	results := make(map[string]usageFillResult, len(versions))
	var retry []usageSourceVersion
	stable := make([]usageSourceVersion, 0, len(versions))
	currentVersions, err := c.recheckSourceVersions(ctx, versions)
	if err != nil {
		return nil, versions, err
	}
	for _, observed := range versions {
		current, exists := currentVersions[observed.SessionID]
		if !exists {
			if _, err := conn.ExecContext(ctx,
				`DELETE FROM usage_cached_sessions WHERE session_id = ?`,
				observed.SessionID,
			); err != nil {
				return nil, versions, err
			}
			results[observed.SessionID] = usageFillResult{Deleted: true}
			continue
		}
		if !current.Equal(observed) {
			retry = append(retry, current)
			continue
		}
		stable = append(stable, current)
	}
	stableResults, err := installSpoolSessions(ctx, conn, stable)
	if err != nil {
		return nil, versions, err
	}
	maps.Copy(results, stableResults)
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, versions, fmt.Errorf("committing usage cache fill: %w", err)
	}
	committed = true
	return results, retry, nil
}

func (c *usageFillCoordinator) recheckSourceVersions(
	ctx context.Context, observed []usageSourceVersion,
) (map[string]usageSourceVersion, error) {
	conn, err := c.archive.getReader().Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var priorBusyTimeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(
		&priorBusyTimeout); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(),
			`PRAGMA busy_timeout=`+strconv.Itoa(priorBusyTimeout))
	}()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var databaseID string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&databaseID); err != nil {
		return nil, err
	}
	if databaseID != c.cache.databaseID {
		return nil, fmt.Errorf("%w during session fill", errUsageCacheSourceChanged)
	}
	ids := make([]string, 0, len(observed))
	for _, version := range observed {
		ids = append(ids, version.SessionID)
	}
	current := make(map[string]usageSourceVersion, len(ids))
	if err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT id, COALESCE(sync_marker, ''),
			       COALESCE(transcript_revision, '0')
			FROM sessions WHERE deleted_at IS NULL AND id IN `+placeholders,
			args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var version usageSourceVersion
			if scanErr := rows.Scan(&version.SessionID, &version.SyncMarker,
				&version.TranscriptRevision); scanErr != nil {
				return scanErr
			}
			current[version.SessionID] = version
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	foundIDs := make([]string, 0, len(current))
	for _, id := range ids {
		if _, ok := current[id]; ok {
			foundIDs = append(foundIDs, id)
		}
	}
	fingerprints, err := usageEventFingerprintsWithQuerier(
		ctx, tx, foundIDs,
	)
	if err != nil {
		return nil, err
	}
	for id, version := range current {
		version.UsageEventFingerprint = fingerprints[id]
		current[id] = version
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return current, nil
}

func installSpoolSessions(
	ctx context.Context, cacheConn *sql.Conn, versions []usageSourceVersion,
) (map[string]usageFillResult, error) {
	results := make(map[string]usageFillResult, len(versions))
	if len(versions) == 0 {
		return results, nil
	}
	var revisionText string
	if err := cacheConn.QueryRowContext(ctx,
		`SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataNextInstallRevision,
	).Scan(&revisionText); err != nil {
		return nil, err
	}
	revision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing usage install revision: %w", err)
	}
	if _, err := cacheConn.ExecContext(ctx, `
		CREATE TEMP TABLE IF NOT EXISTS usage_install_sessions(
			session_id TEXT PRIMARY KEY, source_sync_marker TEXT NOT NULL,
			source_transcript_rev TEXT NOT NULL,
			usage_event_fingerprint TEXT NOT NULL,
			install_revision INTEGER NOT NULL, fact_count INTEGER NOT NULL DEFAULT 0,
			min_fact_timestamp_ms INTEGER, max_fact_timestamp_ms INTEGER
		) WITHOUT ROWID;
		DELETE FROM usage_install_sessions`); err != nil {
		return nil, err
	}
	var statement strings.Builder
	statement.WriteString(`INSERT INTO usage_install_sessions(
		session_id, source_sync_marker, source_transcript_rev,
		usage_event_fingerprint, install_revision) VALUES `)
	args := make([]any, 0, len(versions)*5)
	for i, version := range versions {
		if i > 0 {
			statement.WriteByte(',')
		}
		statement.WriteString(`(?, ?, ?, ?, ?)`)
		installRevision := revision + int64(i)
		args = append(args, version.SessionID, version.SyncMarker,
			version.TranscriptRevision, version.UsageEventFingerprint,
			installRevision)
		results[version.SessionID] = usageFillResult{
			InstallRevision: installRevision, source: version,
		}
	}
	if _, err := cacheConn.ExecContext(ctx, statement.String(), args...); err != nil {
		return nil, err
	}
	if _, err := cacheConn.ExecContext(ctx, `
		UPDATE usage_install_sessions AS install SET
			fact_count = (SELECT COUNT(*) FROM usage_fill_spool.facts f
			              WHERE f.session_id = install.session_id),
			min_fact_timestamp_ms = (SELECT MIN(timestamp_ms)
			              FROM usage_fill_spool.facts f
			              WHERE f.session_id = install.session_id),
			max_fact_timestamp_ms = (SELECT MAX(timestamp_ms)
			              FROM usage_fill_spool.facts f
			              WHERE f.session_id = install.session_id)`); err != nil {
		return nil, err
	}
	if _, err := cacheConn.ExecContext(ctx, `
		INSERT INTO usage_cached_sessions(
			session_id, source_sync_marker, source_transcript_rev,
			usage_event_fingerprint, install_revision, fact_count,
			min_fact_timestamp_ms, max_fact_timestamp_ms
		)
		SELECT session_id, source_sync_marker, source_transcript_rev,
		       usage_event_fingerprint, install_revision, fact_count,
		       min_fact_timestamp_ms, max_fact_timestamp_ms
		FROM usage_install_sessions WHERE 1
		ON CONFLICT(session_id) DO UPDATE SET
			source_sync_marker=excluded.source_sync_marker,
			source_transcript_rev=excluded.source_transcript_rev,
			usage_event_fingerprint=excluded.usage_event_fingerprint,
			install_revision=excluded.install_revision,
			fact_count=excluded.fact_count,
			min_fact_timestamp_ms=excluded.min_fact_timestamp_ms,
			max_fact_timestamp_ms=excluded.max_fact_timestamp_ms`); err != nil {
		return nil, err
	}
	if _, err := cacheConn.ExecContext(ctx, `
		DELETE FROM usage_facts WHERE cached_session_id IN (
			SELECT cached.id FROM usage_cached_sessions cached
			JOIN usage_install_sessions install
			  ON install.session_id = cached.session_id
		)`); err != nil {
		return nil, err
	}
	if _, err := cacheConn.ExecContext(ctx, `INSERT INTO usage_facts(
		cached_session_id, fact_index, source, message_ordinal, timestamp_ms,
		timestamp_ns, raw_timestamp, uses_session_start, model, input_tokens, output_tokens,
		reasoning_tokens, cache_creation_tokens, cache_read_tokens,
		web_search_requests, reported_cost_microdollars, cost_source,
		request_scoped, claude_message_id, claude_request_id, source_uuid,
		usage_dedup_key, token_eligible, activity_eligible
	)
	SELECT cached.id, facts.fact_index, facts.source, facts.message_ordinal,
	       facts.timestamp_ms, facts.timestamp_ns,
	       raw_timestamp, uses_session_start, model, input_tokens, output_tokens,
	       reasoning_tokens, cache_creation_tokens, cache_read_tokens,
	       web_search_requests, reported_cost_microdollars, cost_source,
	       request_scoped, claude_message_id, claude_request_id, source_uuid,
	       usage_dedup_key, token_eligible, activity_eligible
	FROM usage_fill_spool.facts facts
	JOIN usage_install_sessions install
	  ON install.session_id = facts.session_id
	JOIN usage_cached_sessions cached
	  ON cached.session_id = install.session_id
	ORDER BY cached.id, facts.fact_index`,
	); err != nil {
		return nil, err
	}
	if _, err := cacheConn.ExecContext(ctx, `
		UPDATE usage_cache_metadata SET value = ? WHERE key = ?`,
		strconv.FormatInt(revision+int64(len(versions)), 10),
		usageCacheMetadataNextInstallRevision,
	); err != nil {
		return nil, err
	}
	return results, nil
}

func (c *usageFillCoordinator) ensureCursor(ctx context.Context, target int64) error {
	for {
		current, err := c.cursorHighWater(ctx)
		if err != nil || current >= target {
			return err
		}
		c.mu.Lock()
		if target > c.cursorTarget {
			c.cursorTarget = target
		}
		call := c.cursorCall
		if call == nil {
			call = &usageCursorFillCall{done: make(chan struct{})}
			c.cursorCall = call
			go c.fillCursor()
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			if call.err != nil {
				return call.err
			}
		}
	}
}

func (c *usageFillCoordinator) cursorHighWater(ctx context.Context) (int64, error) {
	var text string
	if err := c.cache.db.QueryRowContext(ctx,
		`SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataCursorHighWaterMark,
	).Scan(&text); err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
}

func (c *usageFillCoordinator) fillCursor() {
	var err error
	for err == nil {
		c.mu.Lock()
		target := c.cursorTarget
		c.mu.Unlock()
		err = c.copyCursorThrough(c.ctx, target)
		c.mu.Lock()
		if err != nil || c.cursorTarget == target {
			call := c.cursorCall
			c.cursorCall = nil
			c.cursorTarget = 0
			call.err = err
			close(call.done)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
}

type usageCursorInstall struct {
	id, charged, fee  int64
	timestamp         string
	model, kind       string
	userID, userEmail string
	dedup             string
	headless          int
	fact              usagefacts.Fact
}

func (c *usageFillCoordinator) copyCursorThrough(ctx context.Context, target int64) error {
	from, err := c.cursorHighWater(ctx)
	if err != nil || from >= target {
		return err
	}
	for from < target {
		installs, complete, err := c.readCursorBatch(ctx, from, target)
		if err != nil {
			return err
		}
		committedThrough := from
		if len(installs) > 0 {
			committedThrough = installs[len(installs)-1].id
		}
		if complete {
			committedThrough = target
		}
		if err := c.installCursorBatch(ctx, installs, committedThrough); err != nil {
			return err
		}
		from = committedThrough
	}
	return nil
}

func (c *usageFillCoordinator) readCursorBatch(
	ctx context.Context, from, target int64,
) ([]usageCursorInstall, bool, error) {
	archiveTx, err := c.archive.getReader().BeginTx(
		ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("starting Cursor usage snapshot: %w", err)
	}
	defer func() { _ = archiveTx.Rollback() }()
	var databaseID string
	if err := archiveTx.QueryRowContext(ctx, `
		SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&databaseID); err != nil {
		return nil, false, fmt.Errorf("reading Cursor source database ID: %w", err)
	}
	if databaseID != c.cache.databaseID {
		return nil, false,
			fmt.Errorf("%w while copying Cursor usage", errUsageCacheSourceChanged)
	}
	rows, err := archiveTx.QueryContext(ctx, `
		SELECT id, occurred_at, model, kind, input_tokens, output_tokens,
		       cache_write_tokens, cache_read_tokens, charged_microdollars,
		       cursor_token_fee_microdollars, user_id, user_email,
		       is_headless, dedup_key
		FROM cursor_usage_events WHERE id > ? AND id <= ?
		ORDER BY id LIMIT ?`, from, target, usageCursorCopyBatchSize)
	if err != nil {
		return nil, false, fmt.Errorf("extracting Cursor usage facts: %w", err)
	}
	installs := make([]usageCursorInstall, 0, usageCursorCopyBatchSize)
	for rows.Next() {
		var install usageCursorInstall
		var input, output, cacheWrite, cacheRead int64
		if err := rows.Scan(
			&install.id, &install.timestamp, &install.model, &install.kind,
			&input, &output, &cacheWrite, &cacheRead, &install.charged,
			&install.fee, &install.userID, &install.userEmail,
			&install.headless, &install.dedup,
		); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		install.fact, _ = usagefacts.FromEvent(usagefacts.EventInput{
			Source: "cursor", Timestamp: install.timestamp, Model: install.model,
			InputTokens: input, OutputTokens: output,
			CacheCreationTokens: cacheWrite, CacheReadTokens: cacheRead,
		})
		installs = append(installs, install)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := archiveTx.Commit(); err != nil {
		return nil, false, fmt.Errorf("closing Cursor usage snapshot: %w", err)
	}
	return installs, len(installs) < usageCursorCopyBatchSize, nil
}

func (c *usageFillCoordinator) installCursorBatch(
	ctx context.Context, installs []usageCursorInstall, committedThrough int64,
) error {
	tx, err := c.cache.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, install := range installs {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO cursor_usage_facts(
			source_id, timestamp_ms, timestamp_ns, raw_timestamp, model, kind,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			charged_microdollars, cursor_token_fee_microdollars,
			user_id, user_email, is_headless, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			install.id, install.fact.TimestampMillis, install.fact.TimestampNanos,
			install.timestamp, install.model, install.kind,
			install.fact.InputTokens, install.fact.OutputTokens,
			install.fact.CacheCreationTokens, install.fact.CacheReadTokens,
			install.charged, install.fee, install.userID, install.userEmail,
			install.headless, install.dedup,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_cache_metadata
		SET value = CAST(MAX(CAST(value AS INTEGER), ?) AS TEXT)
		WHERE key = ?`, committedThrough, usageCacheMetadataCursorHighWaterMark,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *usageCacheManager) NotifySessions(sessionIDs []string) {
	m.mu.Lock()
	coordinators := make([]*usageFillCoordinator, 0, len(m.generations))
	for _, cache := range m.generations {
		if cache.fill != nil {
			coordinators = append(coordinators, cache.fill)
		}
	}
	m.mu.Unlock()
	for _, coordinator := range coordinators {
		for _, id := range sessionIDs {
			select {
			case coordinator.notify <- usageFillNotification{sessionID: id}:
			default:
			}
		}
	}
}

func (m *usageCacheManager) NotifyCursorUsage() {
	m.mu.Lock()
	coordinators := make([]*usageFillCoordinator, 0, len(m.generations))
	for _, cache := range m.generations {
		if cache.fill != nil {
			coordinators = append(coordinators, cache.fill)
		}
	}
	m.mu.Unlock()
	for _, coordinator := range coordinators {
		select {
		case coordinator.notify <- usageFillNotification{cursor: true}:
		default:
		}
	}
}

func (db *DB) notifyUsageSessions(sessionIDs []string) {
	if db.usageCache != nil && len(sessionIDs) > 0 {
		db.usageCache.NotifySessions(sessionIDs)
	}
}

func (db *DB) notifyCursorUsage() {
	if db.usageCache != nil {
		db.usageCache.NotifyCursorUsage()
	}
}

func (c *usageFillCoordinator) runNotifications() {
	defer close(c.done)
	for {
		select {
		case <-c.ctx.Done():
			return
		case notification := <-c.notify:
			if notification.cursor {
				var target int64
				if err := c.archive.getReader().QueryRowContext(c.ctx,
					`SELECT COALESCE(MAX(id), 0) FROM cursor_usage_events`,
				).Scan(&target); err == nil && target > 0 {
					_, _ = c.Ensure(c.ctx, nil, target)
				}
				continue
			}
			versions, err := c.recheckSourceVersions(c.ctx,
				[]usageSourceVersion{{SessionID: notification.sessionID}})
			version, exists := versions[notification.sessionID]
			if err == nil && exists {
				_, _ = c.Ensure(c.ctx, []usageSourceVersion{version}, 0)
			} else if err == nil {
				_, _ = c.cache.db.ExecContext(c.ctx,
					`DELETE FROM usage_cached_sessions WHERE session_id = ?`,
					notification.sessionID)
			}
		}
	}
}
