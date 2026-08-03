package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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

	pricingMu     sync.Mutex
	pricingLoadMu sync.Mutex
	pricingLoad   *pricingLoad
	customPricing map[string]config.CustomModelRate

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
	return db.BackendCapabilities{Writes: writes}
}

func (*postgresBunBackend) SessionQueryDialect() db.QueryDialect {
	return db.PortableBunSessionQueryDialect()
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

func normalizePGAutomatedScope(
	scope string,
	excludeAutomated bool,
) string {
	switch strings.TrimSpace(scope) {
	case "human", "all", "automated":
		return strings.TrimSpace(scope)
	}
	if excludeAutomated {
		return "human"
	}
	return "all"
}

func pgAutomatedScopePredicate(scope, col string) string {
	switch scope {
	case "human":
		return col + " = FALSE"
	case "automated":
		return col + " = TRUE"
	default:
		return ""
	}
}

// pgActivityWindows holds the cutoff durations used by
// pgTerminationPred. Kept in sync with the SQLite-side constants
// in internal/db/sessions.go so both stores classify a session
// the same way at the same wall-clock time.
const (
	pgActiveWindow = 10 * time.Minute
	pgStaleWindow  = 60 * time.Minute
)

// pgActivityExpr returns the COALESCEd activity timestamp
// expression used to compute a session's effective recency.
const pgActivityExpr = "COALESCE(ended_at, started_at, created_at)"

// pgTerminationPred returns a WHERE fragment for the multi-state
// termination filter (active / stale / unclean). The status value
// may be comma-separated to OR multiple states. Returns "" when
// status is empty or "all".
//
// Stale and unclean both require a parser red flag — sessions with
// termination_status NULL or 'clean' never appear under those
// filters, so a short-lived agent that completes normally never
// generates a yellow false-positive once it ages past 10 minutes.
func pgTerminationPred(status string, pb *paramBuilder) string {
	if status == "" || status == "all" {
		return ""
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-pgActiveWindow)
	staleCutoff := now.Add(-pgStaleWindow)
	const flagged = "termination_status IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds,
				pgActivityExpr+" > "+pb.add(activeCutoff))
		case "stale":
			preds = append(preds, "("+
				pgActivityExpr+" > "+pb.add(staleCutoff)+
				" AND "+pgActivityExpr+" <= "+pb.add(activeCutoff)+
				" AND "+flagged+")")
		case "unclean":
			preds = append(preds, "("+
				pgActivityExpr+" <= "+pb.add(staleCutoff)+
				" AND "+flagged+")")
		case "clean":
			preds = append(preds, "termination_status = 'clean'")
		case "awaiting_user":
			preds = append(preds,
				"termination_status = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return ""
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return "(" + strings.Join(preds, " OR ") + ")"
}

// scanPGSession scans a row with pgSessionCols into a
// db.Session, converting TIMESTAMPTZ columns to string.
func scanPGSession(
	rs interface{ Scan(...any) error },
) (db.Session, error) {
	var s db.Session
	var createdAt *time.Time
	var startedAt, endedAt, deletedAt *time.Time
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
		&s.FirstMessage, &s.DisplayName,
		&createdAt, &startedAt, &endedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.ParserParentSessionID, &s.RelationshipType,
		&s.TotalOutputTokens, &s.PeakContextTokens,
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
		&s.IsAutomated,
		&s.ToolFailureSignalCount, &s.ToolRetryCount,
		&s.EditChurnCount, &s.ConsecutiveFailureMax,
		&s.Outcome, &s.OutcomeConfidence,
		&s.EndedWithRole, &s.FinalFailureStreak,
		&s.SignalsPendingSince,
		&s.CompactionCount, &s.MidTaskCompactionCount,
		&s.ContextPressureMax,
		&s.HealthScore, &s.HealthGrade,
		&s.HasToolCalls, &s.HasContextData,
		&s.QualitySignalVersion,
		&s.ShortPromptCount, &s.UnstructuredStart,
		&s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.TranscriptFidelity, &s.ParserMalformedLines, &s.IsTruncated,
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&deletedAt, &s.DeletionCause, &s.TerminationStatus, &s.TranscriptRevision,
		&s.FilePath,
	)
	if err != nil {
		return s, err
	}
	if createdAt != nil {
		s.CreatedAt = FormatISO8601(*createdAt)
	}
	if startedAt != nil {
		str := FormatISO8601(*startedAt)
		s.StartedAt = &str
	}
	if endedAt != nil {
		str := FormatISO8601(*endedAt)
		s.EndedAt = &str
	}
	if deletedAt != nil {
		str := FormatISO8601(*deletedAt)
		s.DeletedAt = &str
	}
	return s, nil
}

// scanPGSessionRows iterates rows and scans each.
func scanPGSessionRows(
	rows *sql.Rows,
) ([]db.Session, error) {
	sessions := []db.Session{}
	for rows.Next() {
		s, err := scanPGSession(rows)
		if err != nil {
			return nil, fmt.Errorf(
				"scanning session: %w", err,
			)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// buildPGSessionFilter returns a WHERE clause with $N
// placeholders and the corresponding args.
func buildPGSessionFilter(
	f db.SessionFilter,
) (string, []any) {
	return db.BuildSessionFilterSQL(f, db.PostgresQueryDialect())
}

func buildPGSessionBaseFilter(
	f db.SessionFilter,
) (string, []any) {
	return db.BuildSessionBaseFilterSQL(f, db.PostgresQueryDialect())
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
