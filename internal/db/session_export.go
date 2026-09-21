package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const sessionExportOrder = "last_activity_at DESC, id ASC"

const (
	sessionExportActivityTable = "_export_activity_sort"
	sessionExportActivityIndex = "_export_activity_sort_order"
)

type sessionExportActivitySource struct {
	materialized bool
	where        string
	args         []any
	observe      func(string, []any)
}

type sessionExportQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var (
	ErrSessionExportCursorReset    = errors.New("session export cursor reset required")
	ErrSessionExportCursorConflict = errors.New("session export cursor conflict")
)

type SessionExportOptions struct {
	Filter SessionFilter
	Cursor string
	// UseCursorFilter resumes with the cursor's embedded filter instead of
	// requiring Filter to repeat it. CLI callers set this after flag validation.
	UseCursorFilter bool
	Limit           int
	Format          string
}

type SessionExportResult struct {
	SchemaVersion int                               `json:"schema_version"`
	ArchiveID     string                            `json:"archive_id"`
	DatabaseID    string                            `json:"database_id"`
	Rows          []SessionSummaryRow               `json:"rows"`
	NextCursor    string                            `json:"next_cursor,omitempty"`
	Pricing       *export.PricingBlock              `json:"pricing,omitempty"`
	Projects      map[string]export.ProjectMapEntry `json:"projects"`
}

type SessionSummaryRow struct {
	ID                    string                       `json:"id"`
	TranscriptRevision    string                       `json:"transcript_revision"`
	LocalModifiedAt       *string                      `json:"local_modified_at"`
	Project               string                       `json:"-"`
	ProjectReference      export.ProjectReference      `json:"project"`
	Machine               string                       `json:"-"`
	Agent                 string                       `json:"agent"`
	Cwd                   string                       `json:"-"`
	GitBranch             string                       `json:"-"`
	StartedAt             *string                      `json:"started_at"`
	EndedAt               *string                      `json:"ended_at"`
	LastActivityAt        string                       `json:"last_activity_at"`
	DurationSeconds       *int64                       `json:"duration_seconds"`
	MessageCount          int                          `json:"message_count"`
	UserMessageCount      int                          `json:"user_message_count"`
	AssistantMessageCount int                          `json:"assistant_message_count"`
	TurnCount             int                          `json:"turn_count"`
	Classification        export.SessionClassification `json:"classification"`
	IsAutomated           bool                         `json:"is_automated"`
	ModelUsage            *SessionModelUsage           `json:"model_usage"`
	ParentSessionID       *string                      `json:"parent_session_id"`
	RelationshipType      *string                      `json:"relationship_type"`
	Worktree              *SessionExportWorktree       `json:"-"`
	TotalOutputTokens     int                          `json:"total_output_tokens"`
	PeakContextTokens     int                          `json:"peak_context_tokens"`
	HasTotalOutputTokens  bool                         `json:"has_total_output_tokens"`
	HasPeakContextTokens  bool                         `json:"has_peak_context_tokens"`
	lastActivitySort      float64
}

type SessionModelUsage struct {
	Models                   []string                              `json:"models"`
	InputTokens              int                                   `json:"input_tokens"`
	OutputTokens             int                                   `json:"output_tokens"`
	CacheCreationInputTokens int                                   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                                   `json:"cache_read_input_tokens"`
	ReasoningTokens          int                                   `json:"reasoning_tokens"`
	Cost                     money.Money                           `json:"cost"`
	HasCost                  bool                                  `json:"has_cost"`
	ByModel                  map[string]SessionModelUsageBreakdown `json:"by_model"`
}

type SessionModelUsageBreakdown struct {
	Model                    string            `json:"model"`
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	CacheCreationInputTokens int               `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int               `json:"cache_read_input_tokens"`
	ReasoningTokens          int               `json:"reasoning_tokens"`
	Cost                     money.Money       `json:"cost"`
	HasCost                  bool              `json:"has_cost"`
	CostSource               export.CostSource `json:"cost_source"`
}

type SessionExportWorktree struct {
	Name     *string `json:"name"`
	RootPath *string `json:"root_path"`
}

type sessionExportCursorPayload struct {
	DatabaseID       string                     `json:"database_id"`
	Filters          sessionExportCursorFilters `json:"filters"`
	Order            string                     `json:"order"`
	Watermark        string                     `json:"watermark"`
	WatermarkSort    float64                    `json:"watermark_sort,omitempty"`
	LastActivityAt   string                     `json:"last_activity_at"`
	LastActivitySort float64                    `json:"last_activity_sort,omitempty"`
	LastID           string                     `json:"last_id"`
	Limit            int                        `json:"limit"`
	SnapshotCount    int                        `json:"snapshot_count,omitempty"`
	SnapshotDigest   string                     `json:"snapshot_digest,omitempty"`
	PrefixCount      int                        `json:"prefix_count,omitempty"`
	PrefixDigest     string                     `json:"prefix_digest,omitempty"`
}

type sessionExportCursorFilters struct {
	Project              string   `json:"project,omitempty"`
	ExcludeProject       string   `json:"exclude_project,omitempty"`
	Machine              string   `json:"machine,omitempty"`
	GitBranch            string   `json:"git_branch,omitempty"`
	Agent                string   `json:"agent,omitempty"`
	Date                 string   `json:"date,omitempty"`
	DateFrom             string   `json:"date_from,omitempty"`
	DateTo               string   `json:"date_to,omitempty"`
	Timezone             string   `json:"timezone,omitempty"`
	ActiveSince          string   `json:"active_since,omitempty"`
	MinMessages          int      `json:"min_messages,omitempty"`
	MaxMessages          int      `json:"max_messages,omitempty"`
	MinUserMessages      int      `json:"min_user_messages,omitempty"`
	ExcludeOneShot       bool     `json:"exclude_one_shot,omitempty"`
	ExcludeAutomated     bool     `json:"exclude_automated,omitempty"`
	AutomatedScope       string   `json:"automated_scope,omitempty"`
	IncludeChildren      bool     `json:"include_children,omitempty"`
	IncludeOrphans       bool     `json:"include_orphans,omitempty"`
	Outcome              []string `json:"outcome,omitempty"`
	HealthGrade          []string `json:"health_grade,omitempty"`
	MinToolFailures      *int     `json:"min_tool_failures,omitempty"`
	HasSecret            bool     `json:"has_secret,omitempty"`
	Starred              bool     `json:"starred,omitempty"`
	SecretsRulesVersions []string `json:"secrets_rules_versions,omitempty"`
	Termination          string   `json:"termination,omitempty"`
}

type sessionExportUsageAccum struct {
	models                   map[string]struct{}
	byModel                  map[string]*sessionExportModelUsageAccum
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	reasoningTokens          int
	cost                     money.Money
	authoritativeCost        *money.Money
	contributing             bool
	allPriced                bool
	seen                     map[usageDedupToken]struct{}
}

type sessionExportModelUsageAccum struct {
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	reasoningTokens          int
	cost                     money.Money
	contributing             bool
	allPriced                bool
	computed                 bool
	reported                 bool
}

// ExportSessionSummaries returns content-free session summary rows from the
// local SQLite archive. It intentionally does not live on Store because export
// cursors are archive-bound and include the SQLite database identity.
func (db *DB) ExportSessionSummaries(
	ctx context.Context, opts SessionExportOptions,
) (SessionExportResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SessionExportResult{}, fmt.Errorf(
			"starting session export snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := db.exportSessionSummariesTx(
		ctx, tx, opts, true, sessionExportActivitySource{},
	)
	if err != nil {
		return SessionExportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionExportResult{}, fmt.Errorf(
			"committing session export snapshot: %w", err)
	}
	return result, nil
}

// ExportAllSessionSummaries follows every page inside one read transaction so
// a combined JSON or NDJSON artifact has one pricing and identity snapshot.
func (db *DB) ExportAllSessionSummaries(
	ctx context.Context, opts SessionExportOptions,
) ([]SessionExportResult, error) {
	return db.exportAllSessionSummaries(ctx, opts, nil, nil)
}

func (db *DB) exportAllSessionSummaries(
	ctx context.Context,
	opts SessionExportOptions,
	afterPage func(int, *sql.Tx) error,
	observe func(string, []any),
) ([]SessionExportResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting complete session export snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	effectiveFilter, err := db.resolveSessionExportFilter(opts)
	if err != nil {
		return nil, err
	}
	where, args := buildSessionExportFilterForAlias(effectiveFilter, "sessions")
	if err := db.materializeSessionExportActivitySort(
		ctx, tx, where, args,
	); err != nil {
		return nil, err
	}
	pages := []SessionExportResult{}
	for {
		result, err := db.exportSessionSummariesTx(
			ctx, tx, opts, false,
			sessionExportActivitySource{
				materialized: true, where: where, args: args, observe: observe,
			},
		)
		if err != nil {
			return nil, err
		}
		pages = append(pages, result)
		if afterPage != nil {
			if err := afterPage(len(pages), tx); err != nil {
				return nil, err
			}
		}
		if result.NextCursor == "" {
			break
		}
		opts.Cursor = result.NextCursor
	}
	if err := db.dropSessionExportActivitySort(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing complete session export snapshot: %w", err)
	}
	return pages, nil
}

func (db *DB) exportSessionSummariesTx(
	ctx context.Context, tx *sql.Tx, opts SessionExportOptions,
	cursorIntegrity bool, activitySource sessionExportActivitySource,
) (SessionExportResult, error) {
	if opts.Limit <= 0 || opts.Limit > MaxSessionLimit {
		opts.Limit = MaxSessionLimit
	}
	if opts.Format == "" {
		opts.Format = "json"
	}
	opts.Filter = canonicalSessionExportFilter(opts.Filter)

	filters := sessionExportFilters(opts.Filter)

	var cursor sessionExportCursorPayload
	var err error
	if opts.Cursor != "" {
		cursor, err = db.decodeSessionExportCursor(opts.Cursor)
		if err != nil {
			return SessionExportResult{}, err
		}
		if cursor.Order != sessionExportOrder {
			return SessionExportResult{}, fmt.Errorf(
				"%w: order changed", ErrSessionExportCursorConflict)
		}
		if opts.UseCursorFilter {
			opts.Filter = sessionExportFilterFromCursor(cursor.Filters)
			filters = cursor.Filters
		} else if !sessionExportFiltersEqual(cursor.Filters, filters) {
			return SessionExportResult{}, fmt.Errorf(
				"%w: filters changed", ErrSessionExportCursorConflict)
		}
	}

	// Reuse the materialization filter so time-based termination cutoffs
	// cannot change between pages of the same export snapshot.
	where, args := activitySource.where, activitySource.args
	if !activitySource.materialized {
		where, args = buildSessionExportFilterForAlias(
			opts.Filter, activitySource.sessionAlias(),
		)
	}
	databaseID, err := sessionExportMetadataValue(
		ctx, tx, archiveMetadataDatabaseIDKey, ErrDatabaseIDMissing,
		"database id",
	)
	if err != nil {
		return SessionExportResult{}, err
	}
	if opts.Cursor != "" && cursor.DatabaseID != databaseID {
		return SessionExportResult{}, fmt.Errorf(
			"%w: cursor database %q does not match %q",
			ErrSessionExportCursorReset, cursor.DatabaseID, databaseID,
		)
	}
	archiveID, err := sessionExportMetadataValue(
		ctx, tx, archiveMetadataArchiveIDKey, ErrArchiveIDMissing,
		"archive id",
	)
	if err != nil {
		return SessionExportResult{}, err
	}

	watermark := cursor.Watermark
	watermarkSort := cursor.WatermarkSort
	if watermark == "" {
		watermark, watermarkSort, err = db.sessionExportWatermarkFrom(
			ctx, tx, where, args, activitySource,
		)
		if err != nil {
			return SessionExportResult{}, err
		}
	}
	if watermark == "" {
		return SessionExportResult{
			SchemaVersion: export.SessionSummarySchemaVersion,
			ArchiveID:     archiveID,
			DatabaseID:    databaseID,
			Rows:          []SessionSummaryRow{},
			Projects:      map[string]export.ProjectMapEntry{},
		}, nil
	}
	if cursorIntegrity && (cursor.SnapshotDigest != "" || cursor.SnapshotCount != 0) {
		count, digest, err := db.sessionExportSnapshotFingerprint(
			ctx, tx, where, args, watermarkSort)
		if err != nil {
			return SessionExportResult{}, err
		}
		if count != cursor.SnapshotCount || digest != cursor.SnapshotDigest {
			return SessionExportResult{}, fmt.Errorf(
				"%w: session export snapshot changed",
				ErrSessionExportCursorReset,
			)
		}
	}
	if cursorIntegrity && (cursor.PrefixDigest != "" || cursor.PrefixCount != 0) {
		count, digest, err := db.sessionExportPrefixFingerprint(
			ctx, tx, where, args, watermarkSort,
			cursor.LastActivitySort, cursor.LastID)
		if err != nil {
			return SessionExportResult{}, err
		}
		if count != cursor.PrefixCount || digest != cursor.PrefixDigest {
			return SessionExportResult{}, fmt.Errorf(
				"%w: session export snapshot changed",
				ErrSessionExportCursorReset,
			)
		}
	}

	rows, err := db.querySessionExportRowsFrom(
		ctx, tx, where, args, watermarkSort, cursor, opts.Limit,
		activitySource,
	)
	if err != nil {
		return SessionExportResult{}, err
	}

	resultRows := rows
	var next string
	if len(resultRows) > opts.Limit {
		resultRows = resultRows[:opts.Limit]
		last := resultRows[len(resultRows)-1]
		var snapshotCount, prefixCount int
		var snapshotDigest, prefixDigest string
		if cursorIntegrity {
			snapshotCount, snapshotDigest, err = db.sessionExportSnapshotFingerprint(
				ctx, tx, where, args, watermarkSort)
			if err != nil {
				return SessionExportResult{}, err
			}
			prefixCount, prefixDigest, err = db.sessionExportPrefixFingerprint(
				ctx, tx, where, args, watermarkSort, last.lastActivitySort, last.ID)
			if err != nil {
				return SessionExportResult{}, err
			}
		}
		next = db.encodeSessionExportCursor(sessionExportCursorPayload{
			DatabaseID:       databaseID,
			Filters:          filters,
			Order:            sessionExportOrder,
			Watermark:        watermark,
			WatermarkSort:    watermarkSort,
			LastActivityAt:   last.LastActivityAt,
			LastActivitySort: last.lastActivitySort,
			LastID:           last.ID,
			Limit:            opts.Limit,
			SnapshotCount:    snapshotCount,
			SnapshotDigest:   snapshotDigest,
			PrefixCount:      prefixCount,
			PrefixDigest:     prefixDigest,
		})
	}
	pricing, err := db.attachSessionExportUsage(ctx, tx, resultRows)
	if err != nil {
		return SessionExportResult{}, err
	}
	if err := db.attachSessionExportWorktrees(ctx, tx, resultRows); err != nil {
		return SessionExportResult{}, err
	}
	sessionIDs := make([]string, len(resultRows))
	for i := range resultRows {
		sessionIDs[i] = resultRows[i].ID
	}
	snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(
		ctx, tx, sessionIDs)
	if err != nil {
		return SessionExportResult{}, err
	}
	archiveSalt, err := sessionExportMetadataValue(
		ctx, tx, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing,
		"archive salt",
	)
	if err != nil {
		return SessionExportResult{}, err
	}
	archiveSalt, err = validateArchiveSalt(archiveSalt)
	if err != nil {
		return SessionExportResult{}, err
	}
	archiveScope := export.IdentityScope{
		ArchiveID: archiveID, ArchiveSalt: archiveSalt,
	}
	projects := make(map[string]export.ProjectMapEntry, len(resultRows))
	for i := range resultRows {
		obs, ok := snapshots[resultRows[i].ID]
		if !ok {
			obs = export.ProjectIdentityObservation{
				Project: resultRows[i].Project,
				Machine: resultRows[i].Machine,
			}
		}
		resultRows[i].ProjectReference = export.ResolveProjectReferenceFromObservation(obs, archiveScope)
		reference := resultRows[i].ProjectReference
		next := export.ProjectMapEntry{
			DisplayLabel: reference.DisplayLabel,
			ProjectKey:   reference.ProjectKey,
			Resolution:   reference.Resolution,
			Identity:     export.ProjectCatalogIdentity(reference.Identity),
		}
		if existing, ok := projects[reference.ProjectKey]; ok {
			projects[reference.ProjectKey] = MergeSessionProjectCatalogEntry(
				existing, next,
			)
		} else {
			projects[reference.ProjectKey] = next
		}
	}
	return SessionExportResult{
		SchemaVersion: export.SessionSummarySchemaVersion,
		ArchiveID:     archiveID,
		DatabaseID:    databaseID,
		Rows:          resultRows,
		NextCursor:    next,
		Pricing:       pricing,
		Projects:      projects,
	}, nil
}

func MergeSessionProjectCatalogEntry(
	existing, next export.ProjectMapEntry,
) export.ProjectMapEntry {
	if existing.Resolution == export.ProjectResolutionAmbiguous ||
		next.Resolution == export.ProjectResolutionAmbiguous ||
		(existing.Identity != nil && next.Identity != nil &&
			existing.Identity.Key != next.Identity.Key) {
		return export.ProjectMapEntry{
			DisplayLabel: existing.DisplayLabel,
			ProjectKey:   existing.ProjectKey,
			Resolution:   export.ProjectResolutionAmbiguous,
		}
	}
	if existing.Identity == nil && next.Identity != nil {
		return next
	}
	return existing
}

func sessionExportMetadataValue(
	ctx context.Context,
	q sessionExportQuerier,
	key string,
	missing error,
	label string,
) (string, error) {
	var value string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, key,
	).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", missing
		}
		return "", fmt.Errorf("reading %s: %w", label, err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", missing
	}
	return value, nil
}

func (s sessionExportActivitySource) sessionAlias() string {
	if s.materialized {
		return "sessions"
	}
	return ""
}

func (db *DB) resolveSessionExportFilter(
	opts SessionExportOptions,
) (SessionFilter, error) {
	opts.Filter = canonicalSessionExportFilter(opts.Filter)
	filters := sessionExportFilters(opts.Filter)
	if opts.Cursor == "" {
		return opts.Filter, nil
	}
	cursor, err := db.decodeSessionExportCursor(opts.Cursor)
	if err != nil {
		return SessionFilter{}, err
	}
	if cursor.Order != sessionExportOrder {
		return SessionFilter{}, fmt.Errorf(
			"%w: order changed", ErrSessionExportCursorConflict)
	}
	if opts.UseCursorFilter {
		return sessionExportFilterFromCursor(cursor.Filters), nil
	}
	if !sessionExportFiltersEqual(cursor.Filters, filters) {
		return SessionFilter{}, fmt.Errorf(
			"%w: filters changed", ErrSessionExportCursorConflict)
	}
	return opts.Filter, nil
}

func buildSessionExportFilterForAlias(
	f SessionFilter, qualifier string,
) (string, []any) {
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	where := buildSessionFilterWithBuilder(f, b, qualifier)
	return where, b.Args()
}

func (db *DB) materializeSessionExportActivitySort(
	ctx context.Context, tx *sql.Tx, where string, args []any,
) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE `+sessionExportActivityTable+` (
			id TEXT PRIMARY KEY,
			last_activity_at TEXT,
			last_activity_sort REAL
		)`); err != nil {
		return fmt.Errorf("creating session export activity table: %w", err)
	}
	activityExpr := sessionExportLastActivityExprFor("sessions")
	activitySortExpr := sessionExportLastActivitySortExprFor("sessions")
	_, err := tx.ExecContext(ctx, `
		INSERT INTO `+sessionExportActivityTable+` (
			id, last_activity_at, last_activity_sort
		)
		SELECT sessions.id, `+activityExpr+`, `+activitySortExpr+`
		FROM sessions
		WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("populating session export activity table: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX `+sessionExportActivityIndex+` ON `+
			sessionExportActivityTable+` (last_activity_sort DESC, id ASC)`,
	); err != nil {
		return fmt.Errorf("indexing session export activity table: %w", err)
	}
	return nil
}

func (db *DB) dropSessionExportActivitySort(
	ctx context.Context, tx *sql.Tx,
) error {
	if _, err := tx.ExecContext(ctx,
		`DROP TABLE IF EXISTS `+sessionExportActivityTable,
	); err != nil {
		return fmt.Errorf("dropping session export activity table: %w", err)
	}
	return nil
}

func sessionExportWatermarkQuery(
	source sessionExportActivitySource,
	where string,
	args []any,
) (string, []any) {
	if source.materialized {
		return `SELECT last_activity_at, last_activity_sort
		FROM ` + sessionExportActivityTable + ` INDEXED BY ` +
			sessionExportActivityIndex + `
		ORDER BY last_activity_sort DESC, id ASC
		LIMIT 1`, nil
	}
	activityExpr := sessionExportLastActivityExpr()
	activitySortExpr := sessionExportLastActivitySortExpr()
	return `SELECT ` + activityExpr + `, ` + activitySortExpr + `
		FROM sessions WHERE ` + where + `
		ORDER BY ` + activitySortExpr + ` DESC, id ASC
		LIMIT 1`, args
}

func (db *DB) sessionExportWatermarkFrom(
	ctx context.Context,
	q sessionExportQuerier,
	where string,
	args []any,
	source sessionExportActivitySource,
) (string, float64, error) {
	query, queryArgs := sessionExportWatermarkQuery(source, where, args)
	if source.observe != nil {
		source.observe(query, append([]any(nil), queryArgs...))
	}
	var watermark string
	var watermarkSort sql.NullFloat64
	if err := q.QueryRowContext(
		ctx, query, queryArgs...,
	).Scan(&watermark, &watermarkSort); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("querying session export watermark: %w", err)
	}
	if !watermarkSort.Valid {
		return "", 0, nil
	}
	return watermark, watermarkSort.Float64, nil
}

func (db *DB) sessionExportPrefixFingerprint(
	ctx context.Context,
	q sessionExportQuerier,
	where string,
	args []any,
	watermarkSort float64,
	lastActivitySort float64,
	lastID string,
) (int, string, error) {
	if lastActivitySort == 0 || lastID == "" {
		return 0, "", nil
	}
	activityExpr := sessionExportLastActivityExpr()
	activitySortExpr := sessionExportLastActivitySortExpr()
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs,
		watermarkSort, lastActivitySort, lastActivitySort, lastID)
	query := `
SELECT id,
       ` + activityExpr + ` AS last_activity_at,
       ` + activitySortExpr + ` AS last_activity_sort
FROM sessions
WHERE ` + where + `
  AND ` + activitySortExpr + ` <= ?
  AND (` + activitySortExpr + ` > ?
       OR (` + activitySortExpr + ` = ? AND id <= ?))
ORDER BY last_activity_sort DESC, id ASC`

	return sessionExportFingerprintRows(
		ctx, q, "session export cursor prefix", query, queryArgs)
}

func (db *DB) sessionExportSnapshotFingerprint(
	ctx context.Context,
	q sessionExportQuerier,
	where string,
	args []any,
	watermarkSort float64,
) (int, string, error) {
	if watermarkSort == 0 {
		return 0, "", nil
	}
	activityExpr := sessionExportLastActivityExpr()
	activitySortExpr := sessionExportLastActivitySortExpr()
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, watermarkSort)
	query := `
SELECT id,
       ` + activityExpr + ` AS last_activity_at,
       ` + activitySortExpr + ` AS last_activity_sort
FROM sessions
WHERE ` + where + `
  AND ` + activitySortExpr + ` <= ?
ORDER BY last_activity_sort DESC, id ASC`
	count, digest, err := sessionExportFingerprintRows(
		ctx, q, "session export cursor snapshot", query, queryArgs)
	if err != nil {
		return 0, "", err
	}
	return count, digest, nil
}

func sessionExportFingerprintRows(
	ctx context.Context,
	q sessionExportQuerier,
	label string,
	query string,
	args []any,
) (int, string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, "", fmt.Errorf("querying %s: %w", label, err)
	}
	defer rows.Close()

	hash := sha256.New()
	var count int
	for rows.Next() {
		var id, activity string
		var activitySort float64
		if err := rows.Scan(&id, &activity, &activitySort); err != nil {
			return 0, "", fmt.Errorf("scanning %s: %w", label, err)
		}
		count++
		hash.Write(fmt.Appendf(nil, "%.17f", activitySort))
		hash.Write([]byte{0})
		hash.Write([]byte(activity))
		hash.Write([]byte{0})
		hash.Write([]byte(id))
		hash.Write([]byte{0})
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("iterating %s: %w", label, err)
	}
	return count, hex.EncodeToString(hash.Sum(nil)), nil
}

func sessionExportRowsQuery(
	source sessionExportActivitySource,
	where string,
	args []any,
	watermarkSort float64,
	cursor sessionExportCursorPayload,
	limit int,
) (string, []any) {
	queryArgs := append([]any{}, args...)
	activityAtColumn := sessionExportLastActivityExpr()
	activitySortColumn := sessionExportLastActivitySortExpr()
	idColumn := "sessions.id"
	from := "sessions"
	orderBy := "last_activity_sort DESC, sessions.id ASC"
	if source.materialized {
		activityAtColumn = "activity.last_activity_at"
		activitySortColumn = "activity.last_activity_sort"
		from = "sessions\nJOIN " + sessionExportActivityTable +
			" AS activity INDEXED BY " + sessionExportActivityIndex +
			" ON activity.id = sessions.id"
		orderBy = "activity.last_activity_sort DESC, activity.id ASC"
	}
	cursorWhere := where + " AND " + activitySortColumn + " <= ?"
	queryArgs = append(queryArgs, watermarkSort)
	if cursor.LastActivityAt != "" || cursor.LastID != "" {
		cursorWhere += " AND ((" + activitySortColumn + " < ?) OR (" +
			activitySortColumn + " = ? AND " + idColumn + " > ?))"
		queryArgs = append(queryArgs,
			cursor.LastActivitySort, cursor.LastActivitySort, cursor.LastID)
	}
	queryArgs = append(queryArgs, limit+1)
	query := `
SELECT
	` + idColumn + `,
	sessions.transcript_revision,
	sessions.local_modified_at,
	sessions.project,
	sessions.machine,
	sessions.agent,
	sessions.cwd,
	sessions.git_branch,
	sessions.started_at,
	sessions.ended_at,
	` + activityAtColumn + ` AS last_activity_at,
	` + activitySortColumn + ` AS last_activity_sort,
	sessions.message_count,
	sessions.user_message_count,
	(SELECT COUNT(*)
	 FROM messages m
	 WHERE m.session_id = sessions.id
	   AND m.role = 'assistant'
	   AND COALESCE(m.is_system, 0) = 0) AS assistant_message_count,
	COALESCE(sessions.is_automated, 0) AS is_automated,
	sessions.parent_session_id,
	sessions.relationship_type,
	sessions.total_output_tokens,
	sessions.peak_context_tokens,
	sessions.has_total_output_tokens,
	sessions.has_peak_context_tokens
FROM ` + from + `
WHERE ` + cursorWhere + `
ORDER BY ` + orderBy + `
LIMIT ?`
	return query, queryArgs
}

func (db *DB) querySessionExportRowsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	where string,
	args []any,
	watermarkSort float64,
	cursor sessionExportCursorPayload,
	limit int,
	source sessionExportActivitySource,
) ([]SessionSummaryRow, error) {
	query, queryArgs := sessionExportRowsQuery(
		source, where, args, watermarkSort, cursor, limit,
	)
	if source.observe != nil {
		source.observe(query, append([]any(nil), queryArgs...))
	}

	sqlRows, err := q.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying session summary export: %w", err)
	}
	defer sqlRows.Close()

	out := []SessionSummaryRow{}
	for sqlRows.Next() {
		var row SessionSummaryRow
		var startedAt, endedAt sql.NullString
		var localModifiedAt sql.NullString
		var parentID, relationship sql.NullString
		var automated bool
		if err := sqlRows.Scan(
			&row.ID,
			&row.TranscriptRevision,
			&localModifiedAt,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.Cwd,
			&row.GitBranch,
			&startedAt,
			&endedAt,
			&row.LastActivityAt,
			&row.lastActivitySort,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.AssistantMessageCount,
			&automated,
			&parentID,
			&relationship,
			&row.TotalOutputTokens,
			&row.PeakContextTokens,
			&row.HasTotalOutputTokens,
			&row.HasPeakContextTokens,
		); err != nil {
			return nil, fmt.Errorf("scanning session summary export: %w", err)
		}
		row.StartedAt = nullStringPtr(startedAt)
		row.LocalModifiedAt = nullStringPtr(localModifiedAt)
		row.EndedAt = nullStringPtr(endedAt)
		row.DurationSeconds = sessionExportDurationSeconds(
			row.StartedAt, row.EndedAt, row.LastActivityAt)
		row.TurnCount = row.UserMessageCount
		row.ParentSessionID = nullStringPtr(parentID)
		row.RelationshipType = nonEmptyNullStringPtr(relationship)
		row.Classification = export.SessionClassificationInteractive
		if automated {
			row.Classification = export.SessionClassificationAutomated
		}
		row.IsAutomated = row.Classification ==
			export.SessionClassificationAutomated
		row.ModelUsage = emptySessionModelUsage()
		out = append(out, row)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session summary export: %w", err)
	}
	return out, nil
}

func (db *DB) attachSessionExportUsage(
	ctx context.Context, q sessionExportQuerier, rows []SessionSummaryRow,
) (*export.PricingBlock, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	pricingRows, err := db.loadPricingMapFrom(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("loading pricing: %w", err)
	}
	resolver := export.NewPricingResolver(pricingRows)
	accum := make(map[string]*sessionExportUsageAccum, len(rows))
	for i := range rows {
		accum[rows[i].ID] = newSessionExportUsageAccum()
	}

	query, args := sessionExportUsageQuery(sessionExportIDs(rows))
	usageRows, err := querySessionExportUsageRows(ctx, q, query, args)
	if err != nil {
		return nil, err
	}
	peers, err := sessionExportClaudeSnapshotPeers(
		ctx, q, usageRows, sessionExportIDs(rows),
	)
	if err != nil {
		return nil, err
	}
	usageRows = append(usageRows, peers...)

	snapshotRows := make([]activity.UsageRow, len(usageRows))
	for i, r := range usageRows {
		_, outputTok, _, _, _ := sessionExportUsageTokens(r)
		ordinal := int64(-1)
		if r.messageOrdinal.Valid {
			ordinal = r.messageOrdinal.Int64
		}
		snapshotRows[i] = activity.UsageRow{
			SessionID:         r.sessionID,
			Timestamp:         r.ts,
			MessageOrdinal:    ordinal,
			OutputTokens:      outputTok,
			WebSearchRequests: usageRowWebSearchRequests(r.usageSource, r.tokenJSON),
			ClaudeMessageID:   r.claudeMessageID,
			ClaudeRequestID:   r.claudeRequestID,
		}
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests := activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	for i, r := range usageRows {
		if !snapshotMask[i] {
			continue
		}
		a := accum[snapshotAttribution[i]]
		if a == nil {
			continue
		}
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := a.seen[key]; dup {
				continue
			}
			a.seen[key] = struct{}{}
		}
		inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok := sessionExportUsageTokens(r)
		costRow := r
		authoritative := r.costSource == CopilotReportedCostSource &&
			r.cost.Valid
		if authoritative {
			v := money.Money{Microdollars: r.cost.Int64}
			a.authoritativeCost = &v
			costRow.cost = sql.NullInt64{}
			resolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr := sessionRowCostWithWebSearchRequests(
			costRow, snapshotWebSearchRequests[i], resolver)
		if priceErr != nil {
			return nil, priceErr
		}
		if !contributes {
			continue
		}
		a.contributing = true
		a.models[r.model] = struct{}{}
		a.inputTokens += inputTok
		a.outputTokens += outputTok
		a.cacheCreationInputTokens += cacheCrTok
		a.cacheReadInputTokens += cacheRdTok
		a.reasoningTokens += reasoningTok
		if priced {
			a.cost, priceErr = money.Add(a.cost, cost)
			if priceErr != nil {
				return nil, fmt.Errorf(
					"summing session export cost: %w", priceErr)
			}
		} else {
			a.allPriced = false
		}
		ma := a.byModel[r.model]
		if ma == nil {
			ma = &sessionExportModelUsageAccum{allPriced: true}
			a.byModel[r.model] = ma
		}
		ma.contributing = true
		ma.inputTokens += inputTok
		ma.outputTokens += outputTok
		ma.cacheCreationInputTokens += cacheCrTok
		ma.cacheReadInputTokens += cacheRdTok
		ma.reasoningTokens += reasoningTok
		if costRow.cost.Valid {
			ma.reported = true
		} else {
			ma.computed = true
		}
		if priced {
			ma.cost, priceErr = money.Add(ma.cost, cost)
			if priceErr != nil {
				return nil, fmt.Errorf(
					"summing session export model cost: %w", priceErr)
			}
		} else {
			ma.allPriced = false
		}
	}
	for _, a := range accum {
		if a == nil || a.authoritativeCost == nil {
			continue
		}
		type modelAllocation struct {
			model string
			usage *sessionExportModelUsageAccum
		}
		models := make([]modelAllocation, 0, len(a.byModel))
		for model, usage := range a.byModel {
			if usage != nil {
				models = append(models, modelAllocation{model: model, usage: usage})
			}
		}
		sort.Slice(models, func(i, j int) bool {
			return models[i].model < models[j].model
		})
		weights := make([]money.Money, len(models))
		for i, model := range models {
			weights[i] = model.usage.cost
		}
		costs := export.AllocateCostByWeight(*a.authoritativeCost, weights)
		for i, model := range models {
			model.usage.cost = costs[i]
			model.usage.allPriced = true
		}
		a.cost = *a.authoritativeCost
		a.allPriced = true
	}

	for i := range rows {
		a := accum[rows[i].ID]
		if a == nil {
			continue
		}
		rows[i].ModelUsage = &SessionModelUsage{
			Models:                   sortedSetKeys(a.models),
			InputTokens:              a.inputTokens,
			OutputTokens:             a.outputTokens,
			CacheCreationInputTokens: a.cacheCreationInputTokens,
			CacheReadInputTokens:     a.cacheReadInputTokens,
			ReasoningTokens:          a.reasoningTokens,
			Cost:                     a.cost,
			HasCost:                  a.authoritativeCost != nil || (a.contributing && a.allPriced),
			ByModel:                  sessionExportModelUsageBreakdowns(a.byModel),
		}
	}
	block, err := resolver.BuildBlock()
	if err != nil {
		return nil, fmt.Errorf("building pricing block: %w", err)
	}
	return &block, nil
}

func sessionExportUsageQuery(sessionIDs []string) (string, []any) {
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := usageRowSelect() + `
	AND u.session_id IN (` + strings.Join(placeholders, ",") + `)
	ORDER BY u.session_id ASC, u.ts ASC, COALESCE(u.message_ordinal, -1) ASC`
	return query, args
}

func querySessionExportUsageRows(
	ctx context.Context, q sessionExportQuerier, query string, args []any,
) ([]usageScanRow, error) {
	sqlRows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying session export usage: %w", err)
	}
	defer sqlRows.Close()

	usageRows := []usageScanRow{}
	for sqlRows.Next() {
		r, scanErr := scanUsageRow(sqlRows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning session export usage: %w", scanErr)
		}
		usageRows = append(usageRows, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session export usage: %w", err)
	}
	return usageRows, nil
}

func sessionExportClaudeSnapshotPeers(
	ctx context.Context,
	q sessionExportQuerier,
	pageRows []usageScanRow,
	pageSessionIDs []string,
) ([]usageScanRow, error) {
	type snapshotKey struct {
		messageID string
		requestID string
	}
	keySet := make(map[snapshotKey]struct{})
	for _, row := range pageRows {
		if row.claudeMessageID == "" || row.claudeRequestID == "" {
			continue
		}
		keySet[snapshotKey{
			messageID: row.claudeMessageID,
			requestID: row.claudeRequestID,
		}] = struct{}{}
	}
	keys := make([]snapshotKey, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].messageID != keys[j].messageID {
			return keys[i].messageID < keys[j].messageID
		}
		return keys[i].requestID < keys[j].requestID
	})
	pageIDs := make(map[string]struct{}, len(pageSessionIDs))
	for _, id := range pageSessionIDs {
		pageIDs[id] = struct{}{}
	}

	peers := []usageScanRow{}
	const snapshotKeyChunk = maxSQLVars / 2
	for i := 0; i < len(keys); i += snapshotKeyChunk {
		end := min(i+snapshotKeyChunk, len(keys))
		tuples := make([]string, 0, end-i)
		args := make([]any, 0, (end-i)*2)
		for _, key := range keys[i:end] {
			tuples = append(tuples, "(?, ?)")
			args = append(args, key.messageID, key.requestID)
		}
		rowsSQL := usageRowsSQLWithWhere(
			usageMessageEligibility+
				" AND m.claude_message_id != '' AND m.claude_request_id != ''"+
				" AND (m.claude_message_id, m.claude_request_id) IN (VALUES "+
				strings.Join(tuples, ", ")+")",
			usageEventEligibility+" AND 1 = 0")
		query := usageRowSelectFromRows(rowsSQL) + `
			ORDER BY u.session_id ASC, u.ts ASC,
				COALESCE(u.message_ordinal, -1) ASC`
		matching, err := querySessionExportUsageRows(ctx, q, query, args)
		if err != nil {
			return nil, fmt.Errorf("loading Claude snapshot peers: %w", err)
		}
		for _, row := range matching {
			if _, pageOwned := pageIDs[row.sessionID]; pageOwned {
				continue
			}
			peers = append(peers, row)
		}
	}
	return peers, nil
}

func sessionExportUsageTokens(
	r usageScanRow,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	if r.usageSource == "message" {
		return clampedUsageTokenCountersWithReasoning(r.tokenJSON)
	}
	inputTok, outputTok, cacheCrTok, cacheRdTok = usageEventRowTokens(
		r.usageSource,
		r.inputTokens,
		r.outputTokens,
		r.cacheCreationInputTokens,
		r.cacheReadInputTokens,
	)
	return inputTok, outputTok, cacheCrTok, cacheRdTok, r.reasoningTokens
}

func (db *DB) attachSessionExportWorktrees(
	ctx context.Context, q sessionExportQuerier, rows []SessionSummaryRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	labels := sortedSetKeys(sessionExportProjectLabels(rows))
	observations, err := db.listProjectIdentityObservationsFrom(ctx, q, labels)
	if err != nil {
		return err
	}
	byProjectMachine := make(map[string][]export.ProjectIdentityObservation)
	for _, obs := range observations {
		if obs.WorktreeName == "" && obs.WorktreeRootPath == "" {
			continue
		}
		key := obs.Project + "\x00" + obs.Machine
		byProjectMachine[key] = append(byProjectMachine[key], obs)
	}
	for i := range rows {
		if wt, ok := sessionExportWorktreeForRow(
			rows[i], byProjectMachine[rows[i].Project+"\x00"+rows[i].Machine],
		); ok {
			w := wt
			rows[i].Worktree = &w
		}
	}
	return nil
}

func (db *DB) encodeSessionExportCursor(c sessionExportCursorPayload) string {
	data, _ := json.Marshal(c)

	db.cursorMu.RLock()
	mac := hmac.New(sha256.New, db.cursorSecret)
	db.cursorMu.RUnlock()

	mac.Write(data)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func (db *DB) decodeSessionExportCursor(
	s string,
) (sessionExportCursorPayload, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return sessionExportCursorPayload{},
			fmt.Errorf("%w: invalid format", ErrInvalidCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionExportCursorPayload{},
			fmt.Errorf("%w: invalid payload: %w", ErrInvalidCursor, err)
	}
	var payload sessionExportCursorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return sessionExportCursorPayload{},
			fmt.Errorf("%w: invalid json: %w", ErrInvalidCursor, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sessionExportCursorPayload{},
			fmt.Errorf("%w: invalid signature encoding: %w", ErrInvalidCursor, err)
	}
	db.cursorMu.RLock()
	mac := hmac.New(sha256.New, db.cursorSecret)
	db.cursorMu.RUnlock()

	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return sessionExportCursorPayload{},
			fmt.Errorf("%w: signature mismatch", ErrInvalidCursor)
	}
	return payload, nil
}

func sessionExportFilters(f SessionFilter) sessionExportCursorFilters {
	f = canonicalSessionExportFilter(f)
	return sessionExportCursorFilters{
		Project:              f.Project,
		ExcludeProject:       f.ExcludeProject,
		Machine:              f.Machine,
		GitBranch:            f.GitBranch,
		Agent:                f.Agent,
		Date:                 f.Date,
		DateFrom:             f.DateFrom,
		DateTo:               f.DateTo,
		Timezone:             f.Timezone,
		ActiveSince:          f.ActiveSince,
		MinMessages:          f.MinMessages,
		MaxMessages:          f.MaxMessages,
		MinUserMessages:      f.MinUserMessages,
		ExcludeOneShot:       f.ExcludeOneShot,
		ExcludeAutomated:     f.ExcludeAutomated,
		AutomatedScope:       f.AutomatedScope,
		IncludeChildren:      f.IncludeChildren,
		IncludeOrphans:       f.IncludeOrphans,
		Outcome:              append([]string(nil), f.Outcome...),
		HealthGrade:          append([]string(nil), f.HealthGrade...),
		MinToolFailures:      cloneIntPtr(f.MinToolFailures),
		HasSecret:            f.HasSecret,
		Starred:              f.Starred,
		SecretsRulesVersions: append([]string(nil), f.SecretsRulesVersions...),
		Termination:          f.Termination,
	}
}

func sessionExportFilterFromCursor(f sessionExportCursorFilters) SessionFilter {
	return canonicalSessionExportFilter(SessionFilter{
		Project:              f.Project,
		ExcludeProject:       f.ExcludeProject,
		Machine:              f.Machine,
		GitBranch:            f.GitBranch,
		Agent:                f.Agent,
		Date:                 f.Date,
		DateFrom:             f.DateFrom,
		DateTo:               f.DateTo,
		Timezone:             f.Timezone,
		ActiveSince:          f.ActiveSince,
		MinMessages:          f.MinMessages,
		MaxMessages:          f.MaxMessages,
		MinUserMessages:      f.MinUserMessages,
		ExcludeOneShot:       f.ExcludeOneShot,
		AutomatedScope:       f.AutomatedScope,
		IncludeChildren:      f.IncludeChildren,
		IncludeOrphans:       f.IncludeOrphans,
		Outcome:              append([]string(nil), f.Outcome...),
		HealthGrade:          append([]string(nil), f.HealthGrade...),
		MinToolFailures:      cloneIntPtr(f.MinToolFailures),
		HasSecret:            f.HasSecret,
		Starred:              f.Starred,
		SecretsRulesVersions: append([]string(nil), f.SecretsRulesVersions...),
		Termination:          f.Termination,
	})
}

func canonicalSessionExportFilter(f SessionFilter) SessionFilter {
	f.Project = strings.TrimSpace(f.Project)
	f.ExcludeProject = strings.TrimSpace(f.ExcludeProject)
	f.Machine = canonicalCSVFilter(f.Machine)
	f.GitBranch = canonicalBranchListFilter(f.GitBranch)
	f.Agent = canonicalCSVFilter(f.Agent)
	f.Date = strings.TrimSpace(f.Date)
	f.DateFrom = strings.TrimSpace(f.DateFrom)
	f.DateTo = strings.TrimSpace(f.DateTo)
	if timezone, err := NormalizeSessionTimezone(f.Timezone); err == nil {
		if timezone == "UTC" {
			// Preserve the existing cursor wire form while treating an
			// explicit UTC filter as equivalent to the omitted UTC default.
			f.Timezone = ""
		} else {
			f.Timezone = timezone
		}
	} else {
		f.Timezone = strings.TrimSpace(f.Timezone)
	}
	f.ActiveSince = strings.TrimSpace(f.ActiveSince)
	f.AutomatedScope = normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	f.ExcludeAutomated = false
	f.Outcome = canonicalStringSliceFilter(f.Outcome)
	f.HealthGrade = canonicalStringSliceFilter(f.HealthGrade)
	f.SecretsRulesVersions = canonicalStringSliceFilter(f.SecretsRulesVersions)
	f.Termination = strings.TrimSpace(f.Termination)
	if f.Termination == "all" {
		f.Termination = ""
	}
	f.OrderBy = ""
	return f
}

func canonicalCSVFilter(s string) string {
	parts := splitCSV(s)
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func canonicalBranchListFilter(s string) string {
	parts := strings.Split(s, branchListSep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, branchListSep)
}

func canonicalStringSliceFilter(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func sessionExportFiltersEqual(
	a, b sessionExportCursorFilters,
) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func sessionExportLastActivityExpr() string {
	return sessionExportLastActivityExprFor("sessions")
}

func sessionExportLastActivityExprFor(sessionAlias string) string {
	messageActivity := "(SELECT NULLIF(m.timestamp, '') " +
		"FROM messages m WHERE m.session_id = " + sessionAlias + ".id " +
		"AND m.timestamp != '' " +
		"ORDER BY julianday(m.timestamp) DESC, m.timestamp DESC LIMIT 1)"
	return "COALESCE(NULLIF(" + sessionAlias + ".ended_at, ''), " +
		messageActivity + ", NULLIF(" + sessionAlias + ".started_at, ''), " +
		sessionAlias + ".created_at)"
}

func sessionExportLastActivitySortExpr() string {
	return sessionExportLastActivitySortExprFor("sessions")
}

func sessionExportLastActivitySortExprFor(sessionAlias string) string {
	messageActivity := "(SELECT MAX(julianday(NULLIF(m.timestamp, ''))) " +
		"FROM messages m WHERE m.session_id = " + sessionAlias + ".id)"
	return "COALESCE(julianday(NULLIF(" + sessionAlias + ".ended_at, '')), " +
		messageActivity + ", julianday(NULLIF(" + sessionAlias + ".started_at, '')), " +
		"julianday(" + sessionAlias + ".created_at))"
}

func sessionExportProjectLabels(rows []SessionSummaryRow) map[string]struct{} {
	labels := make(map[string]struct{})
	for _, row := range rows {
		labels[row.Project] = struct{}{}
	}
	return labels
}

func sessionExportIDs(rows []SessionSummaryRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func newSessionExportUsageAccum() *sessionExportUsageAccum {
	return &sessionExportUsageAccum{
		models:    make(map[string]struct{}),
		byModel:   make(map[string]*sessionExportModelUsageAccum),
		allPriced: true,
		seen:      make(map[usageDedupToken]struct{}),
	}
}

func emptySessionModelUsage() *SessionModelUsage {
	return &SessionModelUsage{
		Models:  []string{},
		HasCost: false,
		ByModel: map[string]SessionModelUsageBreakdown{},
	}
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func nonEmptyNullStringPtr(v sql.NullString) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	return &v.String
}

func optionalStringPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func sessionExportModelUsageBreakdowns(
	byModel map[string]*sessionExportModelUsageAccum,
) map[string]SessionModelUsageBreakdown {
	out := make(map[string]SessionModelUsageBreakdown, len(byModel))
	for model, a := range byModel {
		if a == nil {
			continue
		}
		out[model] = SessionModelUsageBreakdown{
			Model:                    model,
			InputTokens:              a.inputTokens,
			OutputTokens:             a.outputTokens,
			CacheCreationInputTokens: a.cacheCreationInputTokens,
			CacheReadInputTokens:     a.cacheReadInputTokens,
			ReasoningTokens:          a.reasoningTokens,
			Cost:                     a.cost,
			HasCost:                  a.contributing && a.allPriced,
			CostSource:               sessionExportCostSource(a.computed, a.reported),
		}
	}
	return out
}

func sessionExportCostSource(computed, reported bool) export.CostSource {
	switch {
	case computed && reported:
		return export.CostSourceMixed
	case reported:
		return export.CostSourceReported
	default:
		return export.CostSourceComputed
	}
}

func sessionExportWorktreeForRow(
	row SessionSummaryRow,
	observations []export.ProjectIdentityObservation,
) (SessionExportWorktree, bool) {
	if len(observations) == 0 {
		return SessionExportWorktree{}, false
	}
	cwd := filepath.Clean(row.Cwd)
	var best *export.ProjectIdentityObservation
	bestLen := -1
	for i := range observations {
		obs := &observations[i]
		for _, root := range []string{obs.WorktreeRootPath, obs.RootPath} {
			if root == "" || !pathHasPrefix(cwd, root) {
				continue
			}
			cleanRoot := filepath.Clean(root)
			if len(cleanRoot) > bestLen {
				best = obs
				bestLen = len(cleanRoot)
			}
		}
	}
	if best == nil {
		return SessionExportWorktree{}, false
	}
	return SessionExportWorktree{
		Name:     optionalStringPtr(best.WorktreeName),
		RootPath: optionalStringPtr(best.WorktreeRootPath),
	}, true
}

func pathHasPrefix(path, prefix string) bool {
	if path == "." || prefix == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanPrefix := filepath.Clean(prefix)
	if cleanPath == cleanPrefix {
		return true
	}
	rel, err := filepath.Rel(cleanPrefix, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sessionExportDurationSeconds(
	startedAt, endedAt *string, lastActivityAt string,
) *int64 {
	if startedAt == nil || *startedAt == "" {
		return nil
	}
	endAt := lastActivityAt
	if endedAt != nil && *endedAt != "" {
		endAt = *endedAt
	}
	if endAt == "" {
		return nil
	}
	start, err := time.Parse(time.RFC3339Nano, *startedAt)
	if err != nil {
		return nil
	}
	end, err := time.Parse(time.RFC3339Nano, endAt)
	if err != nil || end.Before(start) {
		return nil
	}
	seconds := int64(end.Sub(start).Seconds())
	return &seconds
}
