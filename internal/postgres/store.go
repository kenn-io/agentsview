package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

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
		return 0, fmt.Errorf("inserting insight: %w", err)
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
		return fmt.Errorf("deleting insight %d: %w", id, err)
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
		return fmt.Errorf("renaming session %s: %w", id, err)
	}
	return nil
}

// SoftDeleteSession moves a session to the trash.
func (s *Store) SoftDeleteSession(id string) error {
	_, err := s.pg.Exec(
		`UPDATE sessions
		 SET deleted_at = NOW(),
		     updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("soft deleting session %s: %w", id, err)
	}
	return nil
}

// SoftDeleteSessions moves multiple sessions to the trash.
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
			     updated_at = NOW()
			 WHERE id IN (`+strings.Join(placeholders, ",")+
				`) AND deleted_at IS NULL`,
			pb.args...,
		)
		if err != nil {
			return total, fmt.Errorf("soft deleting sessions: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("counting soft deleted sessions: %w", err)
		}
		total += int(n)
	}
	return total, nil
}

// RestoreSession restores a trashed session.
func (s *Store) RestoreSession(id string) (int64, error) {
	res, err := s.pg.Exec(
		`UPDATE sessions
		 SET deleted_at = NULL,
		     updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return 0, fmt.Errorf("restoring session %s: %w", id, err)
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
		return 0, fmt.Errorf(
			"begin delete-if-trashed tx for %s: %w", id, err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET deleted_at = deleted_at
		 WHERE id = $1 AND deleted_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"locking trashed session %s: %w", id, err,
		)
	}
	locked, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf(
			"counting locked trashed session %s: %w", id, err,
		)
	}
	if locked == 0 {
		return 0, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO excluded_sessions (id)
		 SELECT id FROM sessions
		 WHERE id = $1 AND deleted_at IS NOT NULL
		 ON CONFLICT (id) DO NOTHING`,
		id,
	); err != nil {
		return 0, fmt.Errorf(
			"recording excluded trashed session %s: %w", id, err,
		)
	}

	res, err = tx.ExecContext(ctx,
		`DELETE FROM sessions
		 WHERE id = $1 AND deleted_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return 0, fmt.Errorf("deleting trashed session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted trashed session %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"commit delete-if-trashed %s: %w", id, err,
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
		return 0, fmt.Errorf("begin empty-trash tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET deleted_at = deleted_at
		 WHERE deleted_at IS NOT NULL`,
	); err != nil {
		return 0, fmt.Errorf("locking trashed sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO excluded_sessions (id)
		 SELECT id FROM sessions WHERE deleted_at IS NOT NULL
		 ON CONFLICT (id) DO NOTHING`,
	); err != nil {
		return 0, fmt.Errorf("recording excluded trashed sessions: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		"DELETE FROM sessions WHERE deleted_at IS NOT NULL",
	)
	if err != nil {
		return 0, fmt.Errorf("emptying trash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting emptied trash rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit empty-trash: %w", err)
	}
	return int(n), nil
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
