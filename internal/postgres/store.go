package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// deletionCauseSourceMissing mirrors the recoverable watcher-tombstone cause
// written by the SQLite archive (internal/db). Kept package-local because the
// db package does not export it.
const deletionCauseSourceMissing = "source_missing"

// NewStore opens a PostgreSQL connection using the shared Open()
// helper and returns a Store.
// When allowInsecure is true, non-loopback connections without
// TLS produce a warning instead of failing.
func NewStore(
	pgURL, schema string, allowInsecure bool,
) (*Store, error) {
	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return nil, err
	}
	return &Store{pg: pg}, nil
}

// DB returns the underlying *sql.DB for operations that need
// direct access (e.g. schema compatibility checks).
func (s *Store) DB() *sql.DB { return s.pg }

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.pg.Close()
}

func (s *Store) SetCustomPricing(p map[string]config.CustomModelRate) {
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	s.customPricing = p
	s.forgetPricingLoad()
}

// SetCursorSecret sets the HMAC key used for cursor signing.
func (s *Store) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

// ReadOnly returns true because PG serve still treats the remote
// session store as remote; local file, upload, and batch-ingest
// paths stay blocked while dashboard curation uses dedicated methods.
func (s *Store) ReadOnly() bool { return true }

// InsightGenerationAvailable reports whether startup proved this PG role can
// persist generated insights.
func (s *Store) InsightGenerationAvailable() bool {
	s.insightCapabilityMu.RLock()
	defer s.insightCapabilityMu.RUnlock()
	return s.insightGenerationAvailable
}

func (s *Store) setInsightGenerationAvailable(available bool) {
	s.insightCapabilityMu.Lock()
	defer s.insightCapabilityMu.Unlock()
	s.insightGenerationAvailable = available
}

// DetectInsightGenerationAvailability probes whether this PG connection can
// insert into insights. PG serve uses it to expose generate routes only when
// the configured role can actually persist the result.
func (s *Store) DetectInsightGenerationAvailability(
	ctx context.Context,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf(
			"beginning insight generation capability probe: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	available, err := probeInsightGenerationAvailabilityTx(
		ctx, tx,
	)
	if err != nil {
		return err
	}
	s.setInsightGenerationAvailable(available)
	return nil
}

func probeInsightGenerationAvailabilityTx(
	ctx context.Context, tx *sql.Tx,
) (bool, error) {
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO insights DEFAULT VALUES RETURNING id`,
	).Scan(&id); err != nil {
		if IsReadOnlyError(err) {
			return false, nil
		}
		return false, fmt.Errorf(
			"probing insight generation capability: %w", err,
		)
	}
	return true, nil
}

// GetSessionVersion returns the message count and a compact version
// marker for SSE change detection.
func (s *Store) GetSessionVersion(
	id string,
) (int, int64, bool) {
	var count int
	var updatedAt time.Time
	err := s.pg.QueryRow(
		`SELECT message_count, COALESCE(updated_at, created_at)
		 FROM sessions WHERE id = $1`,
		id,
	).Scan(&count, &updatedAt)
	if err != nil {
		return 0, 0, false
	}
	return count, db.SessionVersionMarker(FormatISO8601(updatedAt)), true
}

// ------------------------------------------------------------
// Unsupported write stubs (return db.ErrReadOnly)
// ------------------------------------------------------------

const maxPGInsights = 500

type pgInsightRowScanner interface {
	Scan(...any) error
}

func scanPGInsight(rs pgInsightRowScanner) (db.Insight, error) {
	var s db.Insight
	var project, model, prompt sql.NullString
	var createdAt time.Time
	err := rs.Scan(
		&s.ID, &s.Type, &s.DateFrom, &s.DateTo,
		&project, &s.Agent, &model, &prompt, &s.Content,
		&s.Kind, &s.SchemaVersion, &s.TemplateID,
		&s.TemplateVersion, &s.AggregateHash, &s.CacheKey,
		&s.CacheStatus, &s.ProvenanceJSON, &s.StructuredJSON,
		&createdAt,
	)
	if err != nil {
		return s, err
	}
	if project.Valid {
		s.Project = &project.String
	}
	if model.Valid {
		s.Model = &model.String
	}
	if prompt.Valid {
		s.Prompt = &prompt.String
	}
	s.CreatedAt = FormatISO8601(createdAt)
	return s, nil
}

func mapPGWriteError(action string, err error) error {
	if err == nil {
		return nil
	}
	if IsReadOnlyError(err) {
		return fmt.Errorf("%s: %w: %w", action, db.ErrReadOnly, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func buildPGInsightFilter(
	f db.InsightFilter, pb *paramBuilder,
) string {
	var preds []string
	add := func(expr string, val any) {
		preds = append(preds, expr+" = "+pb.add(val))
	}
	if f.Type != "" {
		add("type", f.Type)
	}
	if f.GlobalOnly {
		preds = append(preds, "project IS NULL")
	} else if f.Project != "" {
		add("project", f.Project)
	}
	if f.DateFrom != "" {
		preds = append(preds, "date_from >= "+pb.add(f.DateFrom))
	}
	if f.DateTo != "" {
		preds = append(preds, "date_to <= "+pb.add(f.DateTo))
	}
	if len(preds) == 0 {
		return "TRUE"
	}
	return strings.Join(preds, " AND ")
}

// InsertInsight stores a dashboard insight in PG.
func (s *Store) InsertInsight(insight db.Insight) (int64, error) {
	var id int64
	err := s.pg.QueryRow(
		`INSERT INTO insights (
			type, date_from, date_to, project,
			agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14,
			$15, $16, $17
		) RETURNING id`,
		insight.Type, insight.DateFrom, insight.DateTo, insight.Project,
		insight.Agent, insight.Model, insight.Prompt, insight.Content,
		insight.Kind, insight.SchemaVersion, insight.TemplateID,
		insight.TemplateVersion, insight.AggregateHash, insight.CacheKey,
		insight.CacheStatus, insight.ProvenanceJSON, insight.StructuredJSON,
	).Scan(&id)
	if err != nil {
		return 0, mapPGWriteError("inserting insight", err)
	}
	return id, nil
}

// DeleteInsight removes a dashboard insight from PG.
func (s *Store) DeleteInsight(id int64) error {
	_, err := s.pg.Exec(
		"DELETE FROM insights WHERE id = $1",
		id,
	)
	if err != nil {
		return mapPGWriteError(fmt.Sprintf("deleting insight %d", id), err)
	}
	return nil
}

// ListInsights returns dashboard insights in created_at order.
func (s *Store) ListInsights(
	ctx context.Context, f db.InsightFilter,
) ([]db.Insight, error) {
	pb := &paramBuilder{}
	where := buildPGInsightFilter(f, pb)
	rows, err := s.pg.QueryContext(ctx,
		`SELECT id, type, date_from, date_to,
			project, agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json,
			created_at
		 FROM insights
		 WHERE `+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT `+fmt.Sprintf("%d", maxPGInsights),
		pb.args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying insights: %w", err)
	}
	defer rows.Close()

	insights := make([]db.Insight, 0)
	for rows.Next() {
		row, err := scanPGInsight(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning insight: %w", err)
		}
		insights = append(insights, row)
	}
	return insights, rows.Err()
}

// GetInsight returns a single insight by ID.
func (s *Store) GetInsight(
	ctx context.Context, id int64,
) (*db.Insight, error) {
	row := s.pg.QueryRowContext(ctx,
		`SELECT id, type, date_from, date_to,
			project, agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json,
			created_at
		 FROM insights
		 WHERE id = $1`,
		id,
	)
	insight, err := scanPGInsight(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting insight %d: %w", id, err)
	}
	return &insight, nil
}

// GetCachedInsight returns the newest insight with the cache key.
func (s *Store) GetCachedInsight(
	ctx context.Context, cacheKey string,
) (*db.Insight, error) {
	if strings.TrimSpace(cacheKey) == "" {
		return nil, nil
	}
	row := s.pg.QueryRowContext(ctx,
		`SELECT id, type, date_from, date_to,
			project, agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json,
			created_at
		 FROM insights
		 WHERE cache_key = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
		cacheKey,
	)
	insight, err := scanPGInsight(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting cached insight: %w", err)
	}
	return &insight, nil
}

func (s *Store) ListRecallEntries(
	_ context.Context, _ db.RecallQuery,
) ([]db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) GetRecallEntry(
	_ context.Context, _ string,
) (*db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) QueryRecallEntries(
	_ context.Context, _ db.RecallQuery,
) (db.RecallPage, error) {
	return db.RecallPage{}, db.ErrReadOnly
}

func (s *Store) RecordRecallQueryEvent(
	_ context.Context, _ db.RecallQueryEvent,
) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) InsertRecallEntry(_ db.RecallEntry) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) ImportAcceptedRecallEntriesJSONL(
	_ context.Context, _ io.Reader,
) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (s *Store) ImportAcceptedRecallEntriesJSONLWithOptions(
	_ context.Context, _ io.Reader, _ db.RecallImportOptions,
) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (s *Store) IngestEvalTrajectory(
	_ context.Context, _ db.EvalTrajectoryIngest,
) (db.EvalTrajectoryIngestResult, error) {
	return db.EvalTrajectoryIngestResult{}, db.ErrReadOnly
}

// RenameSession updates the visible session name in PG.
func (s *Store) RenameSession(
	id string, displayName *string,
) error {
	_, err := s.pg.Exec(
		`UPDATE sessions
		 SET display_name = $2,
		     updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id, displayName,
	)
	if err != nil {
		return mapPGWriteError(fmt.Sprintf("renaming session %s", id), err)
	}
	return nil
}

// SoftDeleteSession moves a session to user trash, including conversion from
// a recoverable source-missing tombstone.
func (s *Store) SoftDeleteSession(id string) error {
	_, err := s.pg.Exec(
		`UPDATE sessions
		 SET deleted_at = NOW(),
		     deletion_cause = NULL,
		     updated_at = NOW()
		 WHERE id = $1
		   AND (deleted_at IS NULL OR deletion_cause = '`+deletionCauseSourceMissing+`')`,
		id,
	)
	if err != nil {
		return mapPGWriteError(
			fmt.Sprintf("soft deleting session %s", id), err,
		)
	}
	return nil
}

// SoftDeleteSessions moves multiple sessions to user trash, including
// conversion from recoverable source-missing tombstones.
func (s *Store) SoftDeleteSessions(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	total := 0
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		pb := &paramBuilder{}
		placeholders := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			placeholders = append(placeholders, pb.add(id))
		}
		res, err := s.pg.Exec(
			`UPDATE sessions
			 SET deleted_at = NOW(),
			     deletion_cause = NULL,
			     updated_at = NOW()
			 WHERE id IN (`+strings.Join(placeholders, ",")+
				`)
			   AND (deleted_at IS NULL OR deletion_cause = '`+deletionCauseSourceMissing+`')`,
			pb.args...,
		)
		if err != nil {
			return total, mapPGWriteError("soft deleting sessions", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("counting soft deleted sessions: %w", err)
		}
		total += int(n)
	}
	return total, nil
}

// SoftDeleteSessionsReturningIDs moves multiple sessions to the trash and
// returns the IDs that changed state.
func (s *Store) SoftDeleteSessionsReturningIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	deleted := make([]string, 0, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		pb := &paramBuilder{}
		placeholders := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			placeholders = append(placeholders, pb.add(id))
		}
		rows, err := s.pg.Query(
			`UPDATE sessions
			 SET deleted_at = NOW(),
			     updated_at = NOW()
			 WHERE id IN (`+strings.Join(placeholders, ",")+
				`) AND deleted_at IS NULL
			 RETURNING id`,
			pb.args...,
		)
		if err != nil {
			return deleted, mapPGWriteError("soft deleting sessions", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return deleted, fmt.Errorf("scanning soft deleted session: %w", err)
			}
			deleted = append(deleted, id)
		}
		if err := rows.Close(); err != nil {
			return deleted, fmt.Errorf("closing soft deleted session rows: %w", err)
		}
		if err := rows.Err(); err != nil {
			return deleted, fmt.Errorf("iterating soft deleted sessions: %w", err)
		}
	}
	return deleted, nil
}

// RestoreSession restores a trashed session.
func (s *Store) RestoreSession(id string) (int64, error) {
	res, err := s.pg.Exec(
		`UPDATE sessions
		 SET deleted_at = NULL,
		     deletion_cause = NULL,
		     updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NOT NULL
		   AND deletion_cause IS NULL`,
		id,
	)
	if err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("restoring session %s", id), err,
		)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting restored session %s: %w", id, err)
	}
	return n, nil
}

// DeleteSessionIfTrashed permanently deletes a trashed session.
func (s *Store) DeleteSessionIfTrashed(
	id string,
) (int64, error) {
	ctx := context.Background()
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("begin delete-if-trashed tx for %s", id),
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	sessionIDs, excludedIDs, err := readPGTrashedSessionExclusions(
		ctx, tx,
		"s.id = $1 AND s.deleted_at IS NOT NULL AND s.deletion_cause IS NULL",
		id,
	)
	if err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("locking trashed session %s", id),
			err,
		)
	}
	if len(sessionIDs) == 0 {
		return 0, nil
	}

	if err := insertPGExcludedSessionIDs(ctx, tx, excludedIDs); err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("recording excluded trashed session %s", id),
			err,
		)
	}

	n, err := deletePGTrashedSessionRows(ctx, tx, sessionIDs)
	if err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("deleting trashed session %s", id), err,
		)
	}
	if err := deletePGExcludedSessionRows(ctx, tx, excludedIDs); err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("purging excluded session aliases for %s", id), err,
		)
	}
	if err := tx.Commit(); err != nil {
		return 0, mapPGWriteError(
			fmt.Sprintf("commit delete-if-trashed %s", id),
			err,
		)
	}
	return n, nil
}

// ListTrashedSessions returns trashed sessions in most-recent order.
func (s *Store) ListTrashedSessions(
	ctx context.Context,
) ([]db.Session, error) {
	rows, err := s.pg.QueryContext(ctx,
		"SELECT "+pgSessionCols+
			" FROM sessions WHERE deleted_at IS NOT NULL"+
			" AND deletion_cause IS NULL"+
			" ORDER BY deleted_at DESC LIMIT 500",
	)
	if err != nil {
		return nil, fmt.Errorf("querying trashed sessions: %w", err)
	}
	defer rows.Close()
	return scanPGSessionRows(rows)
}

// EmptyTrash permanently deletes every trashed session.
func (s *Store) EmptyTrash() (int, error) {
	ctx := context.Background()
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapPGWriteError("begin empty-trash tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	sessionIDs, excludedIDs, err := readPGTrashedSessionExclusions(
		ctx, tx, "s.deleted_at IS NOT NULL AND s.deletion_cause IS NULL",
	)
	if err != nil {
		return 0, mapPGWriteError("locking trashed sessions", err)
	}
	if len(sessionIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, mapPGWriteError("commit empty-trash", err)
		}
		return 0, nil
	}

	if err := insertPGExcludedSessionIDs(ctx, tx, excludedIDs); err != nil {
		return 0, mapPGWriteError("recording excluded trashed sessions", err)
	}

	n, err := deletePGTrashedSessionRows(ctx, tx, sessionIDs)
	if err != nil {
		return 0, mapPGWriteError("emptying trash", err)
	}
	if err := deletePGExcludedSessionRows(ctx, tx, excludedIDs); err != nil {
		return 0, mapPGWriteError("purging excluded trashed session aliases", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, mapPGWriteError("commit empty-trash", err)
	}
	return int(n), nil
}

func readPGTrashedSessionExclusions(
	ctx context.Context, tx *sql.Tx, where string, args ...any,
) ([]string, []string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, sa.alias_id, rsa.session_id, rsaa.alias_id
		 FROM sessions s
		 LEFT JOIN session_aliases sa ON sa.session_id = s.id
		 LEFT JOIN session_aliases rsa ON rsa.alias_id = s.id
		 LEFT JOIN session_aliases rsaa ON rsaa.session_id = rsa.session_id
		 WHERE `+where+`
		 FOR UPDATE OF s`,
		args...,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	sessionIDs := []string{}
	excludedIDs := []string{}
	sessionSeen := map[string]struct{}{}
	excludedSeen := map[string]struct{}{}
	addExcludedID := func(id string) {
		if id == "" {
			return
		}
		if _, ok := excludedSeen[id]; ok {
			return
		}
		excludedSeen[id] = struct{}{}
		excludedIDs = append(excludedIDs, id)
	}
	for rows.Next() {
		var id string
		var aliasID sql.NullString
		var reverseSessionID sql.NullString
		var reverseAliasID sql.NullString
		if err := rows.Scan(
			&id, &aliasID, &reverseSessionID, &reverseAliasID,
		); err != nil {
			return nil, nil, err
		}
		if _, ok := sessionSeen[id]; !ok {
			sessionSeen[id] = struct{}{}
			sessionIDs = append(sessionIDs, id)
		}
		addExcludedID(id)
		if aliasID.Valid {
			addExcludedID(aliasID.String)
		}
		if reverseSessionID.Valid {
			addExcludedID(reverseSessionID.String)
		}
		if reverseAliasID.Valid {
			addExcludedID(reverseAliasID.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return sessionIDs, excludedIDs, nil
}

func deletePGTrashedSessionRows(
	ctx context.Context, tx *sql.Tx, ids []string,
) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM sessions
		 WHERE id = ANY($1) AND deleted_at IS NOT NULL
		   AND deletion_cause IS NULL`,
		ids,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UpsertSession is not supported in read-only mode.
func (s *Store) UpsertSession(_ db.Session) error {
	return db.ErrReadOnly
}

// ReplaceSessionMessages is not supported in read-only mode.
func (s *Store) ReplaceSessionMessages(
	_ string, _ []db.Message,
) error {
	return db.ErrReadOnly
}

// WriteSessionBatchAtomic is not supported in read-only mode.
func (s *Store) WriteSessionBatchAtomic(
	_ []db.SessionBatchWrite,
	_ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}
