package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// Store wraps a PostgreSQL connection for read-only session
// queries.
type Store struct {
	*db.BunStore

	pg  *sql.DB
	bun *bun.DB

	insightCapabilityMu        sync.RWMutex
	insightGenerationAvailable bool
	insightDeletionAvailable   bool

	// vectorMu guards the semantic-search seam. vectorSearcher is the PG
	// vector searcher wired at pg serve startup when a generation matches
	// the configured embeddings fingerprint; semanticUnavailableReason is a
	// human explanation surfaced (wrapped in db.ErrSemanticUnavailable) when
	// no searcher could be wired (extension missing, no matching generation,
	// stale build).
	vectorMu                  sync.RWMutex
	vectorSearcher            db.VectorSearcher
	semanticUnavailableReason string
}

type postgresBunBackend struct {
	store *Store
}

var _ db.BunBackend = (*postgresBunBackend)(nil)

func (*postgresBunBackend) Name() string { return "postgres" }

func (*postgresBunBackend) ReadOnly() bool { return true }

func (b *postgresBunBackend) Capabilities() db.BackendCapabilities {
	writes := map[db.WriteOperation]bool{
		db.WriteCuration:          true,
		db.WriteSessionManagement: true,
	}
	if b.store.InsightGenerationAvailable() {
		writes[db.WriteInsight] = true
	}
	if b.store.InsightDeletionAvailable() {
		writes[db.WriteInsightDelete] = true
	}
	return db.BackendCapabilities{
		FullText:      postgresFullTextCapability{},
		SessionSearch: postgresFullTextCapability{},
		HybridLexical: postgresFullTextCapability{},
		SearchDialect: db.PostgresBunSearchDialect(),
		Semantic: db.NewVectorSemanticCapability(
			b.store.getVectorSearcher,
			b.store.semanticUnavailableError,
		),
		Writes:           writes,
		SessionMutations: postgresSessionMutationAdapter{},
	}
}

type postgresSessionMutationAdapter struct{}

func (postgresSessionMutationAdapter) ApplyTouch(
	query *bun.UpdateQuery, _ bunmodel.Timestamp,
) {
	query.Set("local_modified_at = CURRENT_TIMESTAMP")
	query.Set(`updated_at = GREATEST(
		CURRENT_TIMESTAMP,
		updated_at + INTERVAL '1 microsecond'
	)`)
}

func (adapter postgresSessionMutationAdapter) ApplySoftDelete(
	query *bun.UpdateQuery, now bunmodel.Timestamp,
) {
	query.Set("deleted_at = CURRENT_TIMESTAMP")
	adapter.ApplyTouch(query, now)
}

func (postgresSessionMutationAdapter) AfterRestore(
	context.Context, bun.Tx, string,
) error {
	return nil
}

func (postgresSessionMutationAdapter) BeforeDelete(
	context.Context, bun.Tx, []string,
) error {
	return nil
}

func (*postgresBunBackend) TimestampOrderExpr(column string) string { return column }

func (*postgresBunBackend) BunTableExists(
	ctx context.Context, store bun.IDB, table string,
) (bool, error) {
	var exists bool
	err := store.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?
	)`, table).Scan(ctx, &exists)
	return exists, err
}

func (*postgresBunBackend) SessionVersion(
	ctx context.Context, store bun.IDB, id string,
) (int, int64, error) {
	var row struct {
		MessageCount int       `bun:"message_count"`
		UpdatedAt    time.Time `bun:"updated_at"`
	}
	err := store.NewSelect().Table("sessions").
		Column("message_count").
		ColumnExpr("COALESCE(updated_at, created_at) AS updated_at").
		Where("id = ?", id).Scan(ctx, &row)
	if err != nil {
		return 0, 0, err
	}
	return row.MessageCount,
		db.SessionVersionMarker(FormatISO8601(row.UpdatedAt)), nil
}

func (b *postgresBunBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store.bun)
}

func (b *postgresBunBackend) ConsistentView(
	ctx context.Context, fn func(bun.IDB) error,
) error {
	return b.store.bun.RunInTx(
		ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead},
		func(_ context.Context, tx bun.Tx) error { return fn(tx) },
	)
}

func (b *postgresBunBackend) Update(
	_ context.Context, fn func(bun.IDB) error,
) error {
	err := fn(b.store.bun)
	if IsReadOnlyError(err) {
		return fmt.Errorf("postgres write: %w: %w", db.ErrReadOnly, err)
	}
	return err
}

func newStore(pg *sql.DB) *Store {
	store := &Store{pg: pg}
	store.bun = bun.NewDB(pg, pgdialect.New())
	store.BunStore = db.NewBunStore(&postgresBunBackend{store: store})
	return store
}

// pgSessionCols is the column list for standard PG session queries.
// PostgreSQL retains the source file path used by read-side session
// functionality while omitting volatile local fingerprint metadata.
const pgSessionCols = `id, project, machine, agent,
	agent_label, entrypoint, session_kind,
	first_message, COALESCE(display_name, session_name) AS display_name, created_at, started_at,
	ended_at, message_count, user_message_count,
	parent_session_id, parser_parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	tool_failure_signal_count, tool_retry_count,
	edit_churn_count, consecutive_failure_max,
	outcome, outcome_confidence,
	ended_with_role, final_failure_streak,
	signals_pending_since,
	compaction_count, mid_task_compaction_count,
	context_pressure_max,
	health_score, health_grade,
	has_tool_calls, has_context_data,
	quality_signal_version,
	short_prompt_count, unstructured_start,
	missing_success_criteria_count,
	missing_verification_count, duplicate_prompt_count,
	no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	transcript_fidelity, parser_malformed_lines, is_truncated,
	secret_leak_count, secrets_rules_version,
	deleted_at, deletion_cause, termination_status, transcript_revision,
	file_path`

// paramBuilder generates numbered PostgreSQL placeholders.
type paramBuilder struct {
	n    int
	args []any
}

func (pb *paramBuilder) add(v any) string {
	pb.n++
	pb.args = append(pb.args, v)
	return fmt.Sprintf("$%d", pb.n)
}

// FindSessionIDsByRawSuffix returns up to limit session IDs whose
// stored id is either the exact raw input or the raw input preceded
// by an agent prefix. The suffix comparison is literal and results
// match SQLite ordering: exact match first, then most recent session.
func (s *Store) FindSessionIDsByRawSuffix(
	ctx context.Context, raw string, limit int,
) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.pg.QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (id = $1
		        OR RIGHT(id, LENGTH($1) + 1) = ':' || $1)
		   AND deleted_at IS NULL
		 ORDER BY (id = $1) DESC,
		          COALESCE(ended_at, started_at, created_at) DESC
		 LIMIT $2`,
		raw, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"finding pg sessions by raw suffix %q: %w",
			raw, err,
		)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning pg session id: %w", err,
			)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating pg raw suffix session ids: %w", err,
		)
	}
	return ids, nil
}
