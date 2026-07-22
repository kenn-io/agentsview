package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

const (
	DefaultRecallEntryLimit          = 50
	MaxRecallEntryLimit              = 500
	MaxRecallSearchTerms             = corerecall.MaxScoringQueryTerms
	recallFTS4PreselectLimit         = 50000
	recallEvidenceFTS4PreselectLimit = 50000
)

type RecallEntry struct {
	ID                  string           `json:"id"`
	Type                string           `json:"type"`
	Scope               string           `json:"scope"`
	Status              string           `json:"status"`
	ReviewState         string           `json:"review_state"`
	Title               string           `json:"title"`
	Body                string           `json:"body"`
	Trigger             string           `json:"trigger,omitempty"`
	Confidence          *float64         `json:"confidence,omitempty"`
	Uncertainty         string           `json:"uncertainty,omitempty"`
	Project             string           `json:"project,omitempty"`
	CWD                 string           `json:"cwd,omitempty"`
	GitBranch           string           `json:"git_branch,omitempty"`
	Agent               string           `json:"agent,omitempty"`
	SourceSessionID     string           `json:"source_session_id"`
	SourceEpisodeID     string           `json:"source_episode_id,omitempty"`
	SourceRunID         string           `json:"source_run_id,omitempty"`
	ExtractorMethod     string           `json:"extractor_method,omitempty"`
	Model               string           `json:"model,omitempty"`
	Transferable        bool             `json:"transferable"`
	ProvenanceOK        bool             `json:"provenance_ok"`
	SupersedesEntryID   string           `json:"supersedes_entry_id,omitempty"`
	SupersededByEntryID string           `json:"superseded_by_entry_id,omitempty"`
	CreatedAt           string           `json:"created_at"`
	UpdatedAt           string           `json:"updated_at"`
	Evidence            []RecallEvidence `json:"evidence,omitempty"`
}

// LifecycleBucket classifies an entry's supersession lifecycle as one of
// "active", "replacement", "superseded", or "replacement_superseded". An
// archived entry with no explicit superseded-by link is treated as superseded.
// It is the single classifier shared by `recall stats` and
// `recall query --summary` so both report identical by_lifecycle buckets.
func (m RecallEntry) LifecycleBucket() string {
	switch {
	case m.SupersedesEntryID != "" && m.SupersededByEntryID != "":
		return "replacement_superseded"
	case m.SupersedesEntryID != "":
		return "replacement"
	case m.SupersededByEntryID != "" || m.Status == corerecall.StatusArchived:
		return "superseded"
	default:
		return "active"
	}
}

type RecallEvidence struct {
	ID                     int64  `json:"id"`
	EntryID                string `json:"entry_id"`
	SessionID              string `json:"session_id"`
	MessageStartOrdinal    int    `json:"message_start_ordinal"`
	MessageEndOrdinal      int    `json:"message_end_ordinal"`
	MessageStartSourceUUID string `json:"message_start_source_uuid,omitempty"`
	MessageEndSourceUUID   string `json:"message_end_source_uuid,omitempty"`
	ContentDigest          string `json:"content_digest,omitempty"`
	ToolUseID              string `json:"tool_use_id,omitempty"`
	Snippet                string `json:"snippet,omitempty"`
}

type RecallQuery struct {
	Text                string
	Project             string
	CWD                 string
	GitBranch           string
	Agent               string
	Type                string
	Scope               string
	Status              string
	ExtractorMethod     string
	SourceSessionID     string
	SourceEpisodeID     string
	SourceRunID         string
	SupersedesEntryID   string
	SupersededByEntryID string
	TrustedOnly         bool
	Limit               int
}

type RecallResult struct {
	RecallEntry
	Score          float64                   `json:"score"`
	ScoreBreakdown corerecall.ScoreBreakdown `json:"score_breakdown"`
	MatchReasons   []string                  `json:"match_reasons,omitempty"`
	MatchedTerms   []string                  `json:"matched_terms,omitempty"`
}

type RecallPage struct {
	RecallEntries []RecallResult `json:"entries"`
}

const recallBaseCols = `id, type, scope, status, review_state, title, body, trigger,
	confidence, uncertainty, project, cwd, git_branch, agent,
	source_session_id, source_episode_id, source_run_id, extractor_method,
	model, transferable, provenance_ok, supersedes_entry_id,
	superseded_by_entry_id, created_at, updated_at`

const recallBaseColsQualified = `recall_entries.id, recall_entries.type,
	recall_entries.scope, recall_entries.status, recall_entries.review_state,
	recall_entries.title, recall_entries.body,
	recall_entries.trigger, recall_entries.confidence, recall_entries.uncertainty,
	recall_entries.project, recall_entries.cwd, recall_entries.git_branch, recall_entries.agent,
	recall_entries.source_session_id, recall_entries.source_episode_id,
	recall_entries.source_run_id, recall_entries.extractor_method, recall_entries.model,
	recall_entries.transferable, recall_entries.provenance_ok,
	recall_entries.supersedes_entry_id, recall_entries.superseded_by_entry_id,
	recall_entries.created_at, recall_entries.updated_at`

// ErrInvalidRecallQuery identifies contradictory or unsupported recall filters.
var ErrInvalidRecallQuery = errors.New("invalid recall query")

var errRecallFTSCandidateQueryUnavailable = errors.New("recall fts candidate query unavailable")

func scanRecallEntryRow(rs rowScanner) (RecallEntry, error) {
	var m RecallEntry
	var confidence sql.NullFloat64
	err := rs.Scan(
		&m.ID, &m.Type, &m.Scope, &m.Status, &m.ReviewState, &m.Title, &m.Body,
		&m.Trigger, &confidence, &m.Uncertainty, &m.Project,
		&m.CWD, &m.GitBranch, &m.Agent, &m.SourceSessionID,
		&m.SourceEpisodeID, &m.SourceRunID, &m.ExtractorMethod,
		&m.Model, &m.Transferable, &m.ProvenanceOK,
		&m.SupersedesEntryID, &m.SupersededByEntryID,
		&m.CreatedAt, &m.UpdatedAt,
	)
	if confidence.Valid {
		m.Confidence = &confidence.Float64
	}
	return m, err
}

func scanRecallEvidenceRow(rs rowScanner) (RecallEvidence, error) {
	var e RecallEvidence
	err := rs.Scan(
		&e.ID, &e.EntryID, &e.SessionID, &e.MessageStartOrdinal,
		&e.MessageEndOrdinal, &e.MessageStartSourceUUID,
		&e.MessageEndSourceUUID, &e.ContentDigest,
		&e.ToolUseID, &e.Snippet,
	)
	return e, err
}

func (db *DB) InsertRecallEntry(m RecallEntry) (string, error) {
	if err := normalizeRecallEntryReviewState(&m); err != nil {
		return "", err
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	if m.ID == "" {
		return "", fmt.Errorf("recall entry id is required")
	}
	if m.Status == "" {
		m.Status = corerecall.StatusAccepted
	}

	tx, err := db.getWriter().Begin()
	if err != nil {
		return "", fmt.Errorf("begin recall insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertRecallEntryTx(tx, m); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit recall insert: %w", err)
	}
	return m.ID, nil
}

// CopyRecallEntriesFrom copies entries and their evidence from a source database
// into this database. A full resync rebuilds the DB from source files, which
// never contain entries, so without this copy every accepted entry is
// destroyed on resync (the DB is meant to be a persistent archive). Original
// timestamps are preserved so recency ranking survives.
//
// Only entries whose source session already exists in this DB are copied,
// keeping the source_session_id / session_id foreign keys valid. Sessions are
// copied earlier in the resync (re-synced or orphan-copied), so this normally
// covers every entry; parser-excluded sessions are the exception. Any skipped
// entries are logged.
func (db *DB) CopyRecallEntriesFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Pin one connection for the ATTACH/INSERT/DETACH sequence; ATTACH is
	// connection-scoped and the pool may otherwise switch connections.
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db")
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recall copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var pendingRecallRevocations recallEvidenceRevocationEvents
	if err := copyRecallQueryEventsFromAttachedTx(ctx, tx); err != nil {
		return err
	}
	if err := copyRecallExtractStateFromAttachedTx(ctx, tx); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_entries (
			id, type, scope, status, review_state, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_entry_id, superseded_by_entry_id,
			created_at, updated_at
		)
		SELECT
			id, type, scope, status, review_state, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_entry_id, superseded_by_entry_id,
			created_at, updated_at
		FROM old_db.recall_entries
		WHERE source_session_id IN (SELECT id FROM main.sessions)`)
	if err != nil {
		return fmt.Errorf("copying entries: %w", err)
	}
	copied, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting copied entries: %w", err)
	}

	var total int64
	if err := tx.QueryRowContext(
		ctx, "SELECT count(*) FROM old_db.recall_entries",
	).Scan(&total); err != nil {
		return fmt.Errorf("counting source entries: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO recall_evidence (
			entry_id, session_id, message_start_ordinal,
			message_end_ordinal, message_start_source_uuid,
			message_end_source_uuid, content_digest, tool_use_id, snippet
		)
		SELECT
			entry_id, session_id, message_start_ordinal,
			message_end_ordinal, message_start_source_uuid,
			message_end_source_uuid, content_digest, tool_use_id, snippet
		FROM old_db.recall_evidence
		WHERE entry_id IN (SELECT id FROM main.recall_entries)
		  AND session_id IN (SELECT id FROM main.sessions)`); err != nil {
		return fmt.Errorf("copying recall evidence: %w", err)
	}
	if err := revokeRecallEntriesWithDroppedEvidenceTx(
		ctx,
		tx,
		&pendingRecallRevocations,
	); err != nil {
		return err
	}
	if err := reconcileAllRecallEvidenceTx(
		ctx,
		tx,
		&pendingRecallRevocations,
	); err != nil {
		return fmt.Errorf("reconciling copied recall evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recall copy: %w", err)
	}
	pendingRecallRevocations.flush()

	if total > copied {
		log.Printf(
			"resync: copied %d/%d entries (%d skipped: "+
				"source session not preserved)",
			copied, total, total-copied,
		)
	}
	return nil
}

func revokeRecallEntriesWithDroppedEvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	pending *recallEvidenceRevocationEvents,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT entry.id, entry.source_session_id
		FROM recall_entries entry
		WHERE entry.provenance_ok = 1
		  AND entry.id IN (SELECT id FROM old_db.recall_entries)
		  AND (
			SELECT count(*)
			FROM old_db.recall_evidence old_e
			WHERE old_e.entry_id = entry.id
		  ) != (
			SELECT count(*)
			FROM main.recall_evidence new_e
			WHERE new_e.entry_id = entry.id
		  )
		ORDER BY entry.id ASC`)
	if err != nil {
		return fmt.Errorf("querying recall with dropped evidence: %w", err)
	}
	type droppedEvidenceEntry struct {
		id        string
		sessionID string
	}
	entries := make([]droppedEvidenceEntry, 0)
	for rows.Next() {
		var entry droppedEvidenceEntry
		if err := rows.Scan(&entry.id, &entry.sessionID); err != nil {
			rows.Close()
			return fmt.Errorf("scanning recall with dropped evidence: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing recall with dropped evidence: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading recall with dropped evidence: %w", err)
	}
	for _, entry := range entries {
		if err := revokeRecallEvidenceEntryTx(
			ctx,
			tx,
			entry.id,
			entry.sessionID,
			recallEvidenceRevocationEvidenceDroppedDuringResync,
			pending,
		); err != nil {
			return fmt.Errorf("revoking recall with dropped evidence: %w", err)
		}
	}
	return nil
}

func (db *DB) SupersedeRecallEntry(
	ctx context.Context,
	oldID string,
	replacement RecallEntry,
) (string, error) {
	oldID = strings.TrimSpace(oldID)
	if oldID == "" {
		return "", fmt.Errorf("superseded entry id is required")
	}
	if replacement.ID == "" {
		return "", fmt.Errorf("replacement entry id is required")
	}
	if replacement.ID == oldID {
		return "", fmt.Errorf("replacement entry id must differ from superseded entry id")
	}
	if err := normalizeRecallEntryReviewState(&replacement); err != nil {
		return "", err
	}
	if replacement.Status == "" {
		replacement.Status = corerecall.StatusAccepted
	}
	if replacement.Status != corerecall.StatusAccepted {
		return "", fmt.Errorf(
			"replacement entry status must be %q",
			corerecall.StatusAccepted,
		)
	}
	replacement.SupersedesEntryID = oldID
	replacement.SupersededByEntryID = ""

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin recall supersede: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := supersedeRecallEntryTx(ctx, tx, oldID, replacement); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit recall supersede: %w", err)
	}
	return replacement.ID, nil
}

func supersedeRecallEntryTx(
	ctx context.Context,
	tx *sql.Tx,
	oldID string,
	replacement RecallEntry,
) error {
	if err := requireActiveRecallSupersessionTarget(ctx, tx, oldID); err != nil {
		return err
	}

	if err := insertRecallEntryTx(tx, replacement); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = ?,
		    superseded_by_entry_id = ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		corerecall.StatusArchived, replacement.ID, oldID,
	)
	if err != nil {
		return fmt.Errorf("archiving superseded entry %s: %w", oldID, err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("checking superseded entry update: %w", err)
	} else if rows != 1 {
		return fmt.Errorf("superseded entry %s not found", oldID)
	}
	return nil
}

type recallQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requireActiveRecallSupersessionTarget(
	ctx context.Context,
	queryer recallQueryRower,
	id string,
) error {
	var status, supersededByEntryID string
	if err := queryer.QueryRowContext(ctx, `
		SELECT status, superseded_by_entry_id
		FROM recall_entries
		WHERE id = ?
	`, id).Scan(&status, &supersededByEntryID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("superseded entry %s not found", id)
		}
		return fmt.Errorf("checking superseded entry %s: %w", id, err)
	}
	if status != corerecall.StatusAccepted || supersededByEntryID != "" {
		return fmt.Errorf("superseded entry %s is not active", id)
	}
	return nil
}

func insertRecallEntryTx(tx *sql.Tx, m RecallEntry) error {
	if err := normalizeRecallEntryReviewState(&m); err != nil {
		return err
	}
	if err := validateRecallEvidenceOwnership(m); err != nil {
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO recall_entries (
			id, type, scope, status, review_state, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_entry_id, superseded_by_entry_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Type, m.Scope, m.Status, m.ReviewState, m.Title, m.Body, m.Trigger,
		sqlFloat(m.Confidence), m.Uncertainty, m.Project, m.CWD, m.GitBranch,
		m.Agent, m.SourceSessionID, m.SourceEpisodeID, m.SourceRunID,
		m.ExtractorMethod, m.Model, m.Transferable, m.ProvenanceOK,
		m.SupersedesEntryID, m.SupersededByEntryID,
	)
	if err != nil {
		return fmt.Errorf("inserting recall entry: %w", err)
	}

	for _, e := range m.Evidence {
		_, err = tx.Exec(`
			INSERT INTO recall_evidence (
				entry_id, session_id, message_start_ordinal,
				message_end_ordinal, message_start_source_uuid,
				message_end_source_uuid, content_digest, tool_use_id, snippet
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, e.SessionID, e.MessageStartOrdinal,
			e.MessageEndOrdinal, e.MessageStartSourceUUID,
			e.MessageEndSourceUUID, e.ContentDigest, e.ToolUseID, e.Snippet,
		)
		if err != nil {
			return fmt.Errorf("inserting recall evidence: %w", err)
		}
	}
	return nil
}

func validateRecallEvidenceOwnership(m RecallEntry) error {
	for _, evidence := range m.Evidence {
		if evidence.EntryID != "" && evidence.EntryID != m.ID {
			return fmt.Errorf(
				"recall evidence entry %q does not match inserted entry %q",
				evidence.EntryID,
				m.ID,
			)
		}
		if evidence.SessionID != m.SourceSessionID {
			return fmt.Errorf(
				"recall evidence session %q does not match source session %q",
				evidence.SessionID,
				m.SourceSessionID,
			)
		}
	}
	return nil
}

func normalizeRecallEntryReviewState(m *RecallEntry) error {
	state, ok := corerecall.NormalizeReviewState(m.ReviewState)
	if !ok {
		return fmt.Errorf("invalid recall review state %q", m.ReviewState)
	}
	m.ReviewState = state
	return nil
}

func (db *DB) GetRecallEntry(ctx context.Context, id string) (*RecallEntry, error) {
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+recallBaseCols+" FROM recall_entries WHERE id = ?",
		id,
	)
	m, err := scanRecallEntryRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting recall entry %s: %w", id, err)
	}
	evidence, err := db.listRecallEvidence(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	m.Evidence = evidence[id]
	return &m, nil
}

func (db *DB) ListRecallEntries(
	ctx context.Context, q RecallQuery,
) ([]RecallEntry, error) {
	if err := ValidateRecallQuery(q); err != nil {
		return nil, err
	}
	q = NormalizeRecallQuery(q)
	where, args := buildRecallEntryWhere(q, false)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseCols +
		" FROM recall_entries WHERE " + where +
		" ORDER BY updated_at DESC, id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying entries: %w", err)
	}
	defer rows.Close()

	entries, err := scanRecallEntryRows(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	evidence, err := db.listRecallEvidence(ctx, recallIDs(entries))
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Evidence = evidence[entries[i].ID]
	}
	return entries, nil
}

func (db *DB) ListRecallEntryTextCandidates(
	ctx context.Context, q RecallQuery,
) ([]RecallEntry, error) {
	if err := ValidateRecallQuery(q); err != nil {
		return nil, err
	}
	q = NormalizeRecallQuery(q)
	terms := recallQueryTerms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	direct, err := db.listRecallFTSCandidates(ctx, q, terms)
	if err == nil {
		if len(direct) > 0 {
			return db.mergeRecallEntryCandidatesWithEvidence(ctx, q, terms, direct)
		}
	} else if !recallFTSUnavailable(err) {
		return nil, err
	}
	direct, err = db.listRecallEntryLikeCandidates(ctx, q, terms)
	if err != nil {
		return nil, err
	}
	return db.mergeRecallEntryCandidatesWithEvidence(ctx, q, terms, direct)
}

func (db *DB) listRecallMetadataCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	where, args := buildRecallEntryWhere(q, false)
	metadataWhere, metadataArgs := buildRecallMetadataWhere(terms)
	query := "SELECT " + recallBaseCols +
		" FROM recall_entries WHERE " + where +
		" AND (" + metadataWhere + ")" +
		" ORDER BY " + recallStableSQLTieOrder("") +
		", updated_at DESC, id ASC"
	args = append(args, metadataArgs...)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall metadata candidates: %w", err)
	}
	defer rows.Close()
	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listRecallEntriesForTemporalRanking(
	ctx context.Context, q RecallQuery,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	query := "SELECT " + recallBaseCols +
		" FROM recall_entries WHERE " + where +
		" ORDER BY updated_at DESC, id ASC"
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying temporal recall candidates: %w", err)
	}
	defer rows.Close()
	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listRecallFTSCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	kind := db.recallFTSKind(ctx)
	switch kind {
	case "fts5":
		return db.listRecallFTS5Candidates(ctx, q, terms)
	case "fts4":
		return db.listRecallFTS4RowIDCandidates(ctx, q, terms)
	default:
		return nil, errRecallFTSCandidateQueryUnavailable
	}
}

func (db *DB) listRecallFTS5Candidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseColsQualified +
		" FROM recall_entries_fts" +
		" JOIN recall_entries ON recall_entries.rowid = recall_entries_fts.rowid" +
		" WHERE recall_entries_fts MATCH ? AND " + where +
		" ORDER BY bm25(recall_entries_fts), " +
		recallStableSQLTieOrder("recall_entries") +
		", recall_entries.updated_at DESC, recall_entries.id ASC LIMIT ?"
	args = append([]any{recallFTSQuery(terms)}, args...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall fts candidates: %w", err)
	}
	defer rows.Close()

	candidates, err := scanRecallEntryRowsWithEvidence(ctx, db, rows)
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func (db *DB) listRecallFTS4RowIDCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseCols +
		" FROM recall_entries" +
		" WHERE rowid IN (" +
		"SELECT rowid FROM recall_entries_fts" +
		" WHERE recall_entries_fts MATCH ? LIMIT ?" +
		") AND " + where +
		" ORDER BY " + recallStableSQLTieOrder("") +
		", updated_at DESC, id ASC LIMIT ?"
	args = append(
		[]any{recallFTSQuery(terms), recallFTS4PreselectLimit},
		args...,
	)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall fts4 candidates: %w", err)
	}
	defer rows.Close()

	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listRecallEntryLikeCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	textWhere, textArgs := buildRecallEntryTextWhere(terms)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseCols +
		" FROM recall_entries WHERE " + where +
		" AND (" + textWhere + ")" +
		" ORDER BY " + recallStableSQLTieOrder("") +
		", updated_at DESC, id ASC LIMIT ?"
	args = append(args, textArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall like candidates: %w", err)
	}
	defer rows.Close()

	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) recallFTSKind(ctx context.Context) string {
	var ddl string
	err := db.getReader().QueryRowContext(
		ctx,
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'recall_entries_fts'`,
	).Scan(&ddl)
	if err != nil {
		return ""
	}
	if strings.Contains(ddl, "using fts5") {
		return "fts5"
	}
	if strings.Contains(ddl, "using fts4") {
		return "fts4"
	}
	return ""
}

func (db *DB) recallEvidenceFTSKind(ctx context.Context) string {
	var ddl string
	err := db.getReader().QueryRowContext(
		ctx,
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'recall_evidence_fts'`,
	).Scan(&ddl)
	if err != nil {
		return ""
	}
	if strings.Contains(ddl, "using fts5") {
		return "fts5"
	}
	if strings.Contains(ddl, "using fts4") {
		return "fts4"
	}
	return ""
}

func scanRecallEntryRowsWithEvidence(
	ctx context.Context, db *DB, rows *sql.Rows,
) ([]RecallEntry, error) {
	entries, err := scanRecallEntryRows(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	evidence, err := db.listRecallEvidence(ctx, recallIDs(entries))
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Evidence = evidence[entries[i].ID]
	}
	return entries, nil
}

func (db *DB) mergeRecallEntryCandidatesWithEvidence(
	ctx context.Context,
	q RecallQuery,
	terms []string,
	direct []RecallEntry,
) ([]RecallEntry, error) {
	evidence, err := db.listRecallEvidenceTextCandidates(ctx, q, terms)
	if err != nil {
		return nil, err
	}
	if len(direct) == 0 {
		return evidence, nil
	}
	seen := make(map[string]struct{}, len(direct)+len(evidence))
	out := make([]RecallEntry, 0, len(direct)+len(evidence))
	for _, recall := range direct {
		seen[recall.ID] = struct{}{}
		out = append(out, recall)
	}
	for _, recall := range evidence {
		if _, ok := seen[recall.ID]; ok {
			continue
		}
		seen[recall.ID] = struct{}{}
		out = append(out, recall)
	}
	return out, nil
}

func (db *DB) listRecallEvidenceTextCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	fts, err := db.listRecallEvidenceFTSCandidates(ctx, q, terms)
	if err == nil {
		if len(fts) > 0 {
			return fts, nil
		}
	} else if !recallFTSUnavailable(err) {
		return nil, err
	}
	return db.listRecallEvidenceLikeCandidates(ctx, q, terms)
}

func (db *DB) listRecallEvidenceFTSCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	kind := db.recallEvidenceFTSKind(ctx)
	switch kind {
	case "fts4":
		return db.listRecallEvidenceFTS4PreselectedCandidates(ctx, q, terms)
	case "fts5":
		return db.listRecallEvidenceFTSScoredCandidates(ctx, q, terms)
	default:
		return nil, errRecallFTSCandidateQueryUnavailable
	}
}

func (db *DB) listRecallEvidenceFTSScoredCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	scoreExpr, scoreArgs := buildRecallEvidenceMatchScoreExpr(terms)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseColsQualified +
		" FROM recall_evidence_fts" +
		" JOIN recall_evidence" +
		" ON recall_evidence.id = recall_evidence_fts.rowid" +
		" JOIN recall_entries ON recall_entries.id = recall_evidence.entry_id" +
		" WHERE recall_evidence_fts MATCH ? AND " + where +
		" GROUP BY recall_entries.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		recallStableSQLTieOrder("recall_entries") +
		", recall_entries.updated_at DESC, recall_entries.id ASC LIMIT ?"
	args = append([]any{recallFTSQuery(terms)}, args...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall evidence fts candidates: %w", err)
	}
	defer rows.Close()

	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listRecallEvidenceFTS4PreselectedCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	scoreExpr, scoreArgs := buildRecallEvidenceMatchScoreExpr(terms)
	limit := recallLimit(q.Limit)
	query := "WITH matched_evidence(rowid) AS (" +
		"SELECT rowid FROM recall_evidence_fts" +
		" WHERE recall_evidence_fts MATCH ? LIMIT ?" +
		") SELECT " + recallBaseColsQualified +
		" FROM matched_evidence" +
		" JOIN recall_evidence ON recall_evidence.id = matched_evidence.rowid" +
		" JOIN recall_entries ON recall_entries.id = recall_evidence.entry_id" +
		" WHERE " + where +
		" GROUP BY recall_entries.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		recallStableSQLTieOrder("recall_entries") +
		", recall_entries.updated_at DESC, recall_entries.id ASC LIMIT ?"
	args = append(
		[]any{recallFTSQuery(terms), recallEvidenceFTS4PreselectLimit},
		args...,
	)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying recall evidence fts4 candidates: %w", err,
		)
	}
	defer rows.Close()

	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listRecallEvidenceLikeCandidates(
	ctx context.Context, q RecallQuery, terms []string,
) ([]RecallEntry, error) {
	where, args := buildRecallEntryWhere(q, false)
	textWhere, textArgs := buildRecallEvidenceTextWhere(terms)
	scoreExpr, scoreArgs := buildRecallEvidenceMatchScoreExpr(terms)
	limit := recallLimit(q.Limit)
	query := "SELECT " + recallBaseColsQualified +
		" FROM recall_entries" +
		" JOIN recall_evidence ON recall_evidence.entry_id = recall_entries.id" +
		" WHERE " + where +
		" AND (" + textWhere + ")" +
		" GROUP BY recall_entries.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		recallStableSQLTieOrder("recall_entries") +
		", recall_entries.updated_at DESC, recall_entries.id ASC LIMIT ?"
	args = append(args, textArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recall evidence candidates: %w", err)
	}
	defer rows.Close()

	return scanRecallEntryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) QueryRecallEntries(
	ctx context.Context, q RecallQuery,
) (RecallPage, error) {
	if err := ValidateRecallQuery(q); err != nil {
		return RecallPage{}, err
	}
	q = NormalizeRecallQuery(q)
	if strings.TrimSpace(q.Text) == "" {
		entries, err := db.ListRecallEntries(ctx, q)
		if err != nil {
			return RecallPage{}, err
		}
		return recallPageFromList(entries), nil
	}
	if strings.TrimSpace(corerecall.LexicalQueryText(q.Text)) == "" {
		return RecallPage{RecallEntries: []RecallResult{}}, nil
	}

	candidateQuery := q
	candidateQuery.Limit = MaxRecallEntryLimit
	candidates, err := db.ListRecallEntryTextCandidates(ctx, candidateQuery)
	if err != nil {
		return RecallPage{}, err
	}
	var supplemental []RecallEntry
	if corerecall.QueryUsesTemporalSignals(q.Text) {
		supplemental, err = db.listRecallEntriesForTemporalRanking(ctx, candidateQuery)
	} else {
		supplemental, err = db.listRecallMetadataCandidates(
			ctx, candidateQuery, recallQueryTerms(q.Text),
		)
	}
	if err != nil {
		return RecallPage{}, err
	}
	candidates = mergeRecallEntryCandidateSets(candidates, supplemental)
	if len(candidates) == 0 {
		candidates, err = db.ListRecallEntries(ctx, candidateQuery)
		if err != nil {
			return RecallPage{}, err
		}
	}
	results := corerecall.Rank(toCoreRecallEntries(candidates), corerecall.Query{
		Text:      q.Text,
		Project:   q.Project,
		CWD:       q.CWD,
		GitBranch: q.GitBranch,
		Agent:     q.Agent,
		Status:    q.Status,
		Limit:     len(candidates),
	})
	byID := make(map[string]RecallEntry, len(candidates))
	for _, m := range candidates {
		byID[m.ID] = m
	}
	sortRecallResultsByStableSource(results, byID)
	results = diversifyRecallResults(results, byID, recallLimit(q.Limit))
	page := RecallPage{RecallEntries: make([]RecallResult, 0, len(results))}
	for _, result := range results {
		m := byID[result.Entry.ID]
		page.RecallEntries = append(page.RecallEntries, RecallResult{
			RecallEntry:    m,
			Score:          result.Score,
			ScoreBreakdown: result.Breakdown,
			MatchReasons:   recallMatchReasons(result.Breakdown),
			MatchedTerms:   result.MatchedTerms,
		})
	}
	return page, nil
}

// NormalizeRecallQuery trims whitespace from a query's exact-match filters so
// padded values match consistently. It is exported so the Postgres store can
// apply the same normalization as the SQLite store.
func NormalizeRecallQuery(q RecallQuery) RecallQuery {
	q.Project = strings.TrimSpace(q.Project)
	q.CWD = strings.TrimSpace(q.CWD)
	q.GitBranch = strings.TrimSpace(q.GitBranch)
	q.Agent = strings.TrimSpace(q.Agent)
	q.Type = strings.TrimSpace(q.Type)
	q.Scope = strings.TrimSpace(q.Scope)
	q.Status = strings.TrimSpace(q.Status)
	q.ExtractorMethod = strings.TrimSpace(q.ExtractorMethod)
	q.SourceSessionID = strings.TrimSpace(q.SourceSessionID)
	q.SourceEpisodeID = strings.TrimSpace(q.SourceEpisodeID)
	q.SourceRunID = strings.TrimSpace(q.SourceRunID)
	q.SupersedesEntryID = strings.TrimSpace(q.SupersedesEntryID)
	q.SupersededByEntryID = strings.TrimSpace(q.SupersededByEntryID)
	return q
}

// ValidateRecallQuery rejects filter combinations that cannot produce a
// meaningful result. Trusted queries always target accepted entries; asking
// for another explicit status is therefore a caller error, not an empty page.
func ValidateRecallQuery(q RecallQuery) error {
	q = NormalizeRecallQuery(q)
	if q.TrustedOnly && q.Status != "" && q.Status != corerecall.StatusAccepted {
		return fmt.Errorf(
			"%w: trusted_only requires status %q",
			ErrInvalidRecallQuery,
			corerecall.StatusAccepted,
		)
	}
	return nil
}

func recallPageFromList(entries []RecallEntry) RecallPage {
	page := RecallPage{RecallEntries: make([]RecallResult, 0, len(entries))}
	for _, recall := range entries {
		page.RecallEntries = append(page.RecallEntries, RecallResult{RecallEntry: recall})
	}
	return page
}

func sortRecallResultsByStableSource(
	results []corerecall.Result,
	byID map[string]RecallEntry,
) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		left := byID[results[i].Entry.ID]
		right := byID[results[j].Entry.ID]
		return recallStableSortKey(left, results[i].Entry.ID) <
			recallStableSortKey(right, results[j].Entry.ID)
	})
}

func recallStableSortKey(m RecallEntry, fallbackID string) string {
	id := m.ID
	if id == "" {
		id = fallbackID
	}
	primary := m.SourceEpisodeID
	if primary == "" {
		primary = m.SourceSessionID
	}
	if primary == "" {
		primary = id
	}
	return strings.Join([]string{
		primary,
		m.SourceSessionID,
		m.SourceRunID,
		id,
	}, "\x00")
}

func recallMatchReasons(b corerecall.ScoreBreakdown) []string {
	reasons := []string{}
	if b.KeywordIDFScore > 0 || b.KeywordOverlap > 0 {
		reasons = append(reasons, "keyword")
	}
	if b.EvidenceIDFScore > 0 || b.EvidenceKeywordOverlap > 0 {
		reasons = append(reasons, "evidence")
	}
	if b.IdentifierBoost > 0 {
		reasons = append(reasons, "identifier")
	}
	if b.PhraseBoost > 0 {
		reasons = append(reasons, "phrase")
	}
	if b.EntityBoost > 0 {
		reasons = append(reasons, "entity")
	}
	if b.TemporalBoost > 0 {
		reasons = append(reasons, "temporal")
	}
	if b.ConfidenceBonus > 0 {
		reasons = append(reasons, "confidence")
	}
	return reasons
}

func diversifyRecallResults(
	results []corerecall.Result,
	byID map[string]RecallEntry,
	limit int,
) []corerecall.Result {
	if limit <= 0 || len(results) <= limit {
		return results
	}
	usedIDs := make(map[string]bool, limit)
	usedSources := make(map[string]bool, limit)
	out := make([]corerecall.Result, 0, limit)
	for _, result := range results {
		source := recallSourceDiversityKey(byID[result.Entry.ID])
		if source == "" || usedSources[source] {
			continue
		}
		out = append(out, result)
		usedIDs[result.Entry.ID] = true
		usedSources[source] = true
		if len(out) >= limit {
			return out
		}
	}
	for _, result := range results {
		if usedIDs[result.Entry.ID] {
			continue
		}
		out = append(out, result)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func recallSourceDiversityKey(m RecallEntry) string {
	if base, _, ok := strings.Cut(m.SourceEpisodeID, ":chunk:"); ok && base != "" {
		return base
	}
	if m.SourceEpisodeID != "" {
		return m.SourceEpisodeID
	}
	if m.SourceSessionID != "" {
		return m.SourceSessionID
	}
	return m.ID
}

func scanRecallEntryRows(rows *sql.Rows) ([]RecallEntry, error) {
	var entries []RecallEntry
	for rows.Next() {
		m, err := scanRecallEntryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning recall: %w", err)
		}
		entries = append(entries, m)
	}
	return entries, rows.Err()
}

func buildRecallEntryWhere(q RecallQuery, includeText bool) (string, []any) {
	preds := []string{"1=1"}
	var args []any
	status := q.Status
	if status == "" {
		status = corerecall.StatusAccepted
	}
	preds = append(preds, "status = ?")
	args = append(args, status)
	if q.Project != "" {
		preds = append(preds, "project = ?")
		args = append(args, q.Project)
	}
	if q.CWD != "" {
		preds = append(preds, "cwd = ?")
		args = append(args, q.CWD)
	}
	if q.GitBranch != "" {
		preds = append(preds, "git_branch = ?")
		args = append(args, q.GitBranch)
	}
	if q.Agent != "" {
		preds = append(preds, "agent = ?")
		args = append(args, q.Agent)
	}
	if q.Type != "" {
		preds = append(preds, "type = ?")
		args = append(args, q.Type)
	}
	if q.Scope != "" {
		preds = append(preds, "scope = ?")
		args = append(args, q.Scope)
	}
	if q.ExtractorMethod != "" {
		preds = append(preds, "extractor_method = ?")
		args = append(args, q.ExtractorMethod)
	}
	if q.SourceSessionID != "" {
		preds = append(preds, "source_session_id = ?")
		args = append(args, q.SourceSessionID)
	}
	if q.SourceEpisodeID != "" {
		preds = append(preds, "source_episode_id = ?")
		args = append(args, q.SourceEpisodeID)
	}
	if q.SourceRunID != "" {
		preds = append(preds, "source_run_id = ?")
		args = append(args, q.SourceRunID)
	}
	if q.SupersedesEntryID != "" {
		preds = append(preds, "supersedes_entry_id = ?")
		args = append(args, q.SupersedesEntryID)
	}
	if q.SupersededByEntryID != "" {
		preds = append(preds, "superseded_by_entry_id = ?")
		args = append(args, q.SupersededByEntryID)
	}
	if q.TrustedOnly {
		preds = append(preds, "review_state = ?")
		args = append(args, corerecall.ReviewStateHumanReviewed)
		preds = append(preds, "transferable = 1")
		preds = append(preds, "provenance_ok = 1")
	}
	if includeText && q.Text != "" {
		like := "%" + escapeLike(q.Text) + "%"
		preds = append(preds,
			"(title LIKE ? ESCAPE '\\' OR body LIKE ? ESCAPE '\\' OR trigger LIKE ? ESCAPE '\\')")
		args = append(args, like, like, like)
	}
	return strings.Join(preds, " AND "), args
}

func buildRecallEntryTextWhere(terms []string) (string, []any) {
	preds := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		preds = append(preds, `(
			title LIKE ? ESCAPE '\'
			OR body LIKE ? ESCAPE '\'
			OR trigger LIKE ? ESCAPE '\'
		)`)
		args = append(args, like, like, like)
	}
	return strings.Join(preds, " OR "), args
}

func buildRecallMetadataWhere(terms []string) (string, []any) {
	preds := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		preds = append(preds, `(
			project LIKE ? ESCAPE '\'
			OR cwd LIKE ? ESCAPE '\'
			OR git_branch LIKE ? ESCAPE '\'
			OR agent LIKE ? ESCAPE '\'
		)`)
		args = append(args, like, like, like, like)
	}
	return strings.Join(preds, " OR "), args
}

func mergeRecallEntryCandidateSets(sets ...[]RecallEntry) []RecallEntry {
	seen := make(map[string]struct{})
	var merged []RecallEntry
	for _, entries := range sets {
		for _, entry := range entries {
			if _, ok := seen[entry.ID]; ok {
				continue
			}
			seen[entry.ID] = struct{}{}
			merged = append(merged, entry)
		}
	}
	return merged
}

func buildRecallEvidenceTextWhere(terms []string) (string, []any) {
	preds := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		preds = append(preds, "recall_evidence.snippet LIKE ? ESCAPE '\\'")
		args = append(args, like)
	}
	return strings.Join(preds, " OR "), args
}

func buildRecallEvidenceMatchScoreExpr(terms []string) (string, []any) {
	parts := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		parts = append(parts, "MAX(CASE WHEN recall_evidence.snippet LIKE ? ESCAPE '\\' THEN 1 ELSE 0 END)")
		args = append(args, like)
	}
	return strings.Join(parts, " + "), args
}

func recallStableSQLTieOrder(table string) string {
	col := func(name string) string {
		if table == "" {
			return name
		}
		return table + "." + name
	}
	sourceEpisodeID := col("source_episode_id")
	sourceSessionID := col("source_session_id")
	sourceRunID := col("source_run_id")
	id := col("id")
	return "CASE WHEN " + sourceEpisodeID + " != '' THEN " + sourceEpisodeID +
		" WHEN " + sourceSessionID + " != '' THEN " + sourceSessionID +
		" ELSE " + id + " END ASC, " + sourceSessionID + " ASC, " +
		sourceRunID + " ASC"
}

func recallFTSQuery(terms []string) string {
	return strings.Join(terms, " OR ")
}

func recallFTSUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRecallFTSCandidateQueryUnavailable) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table: recall_entries_fts") ||
		strings.Contains(msg, "no such table: recall_evidence_fts") ||
		strings.Contains(msg, "no such module") ||
		strings.Contains(msg, "unable to use function MATCH")
}

func recallQueryTerms(text string) []string {
	return corerecall.ScoringQueryTerms(text)
}

func recallLimit(limit int) int {
	if limit <= 0 {
		return DefaultRecallEntryLimit
	}
	if limit > MaxRecallEntryLimit {
		return MaxRecallEntryLimit
	}
	return limit
}

func sqlFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func listPlaceholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func recallIDs(entries []RecallEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, m := range entries {
		ids = append(ids, m.ID)
	}
	return ids
}

func (db *DB) listRecallEvidence(
	ctx context.Context, ids []string,
) (map[string][]RecallEvidence, error) {
	result := make(map[string][]RecallEvidence, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	err := queryChunked(ids, func(chunk []string) error {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT id, entry_id, session_id, message_start_ordinal,
				message_end_ordinal, message_start_source_uuid,
				message_end_source_uuid, content_digest, tool_use_id, snippet
			FROM recall_evidence
			WHERE entry_id IN (`+listPlaceholders(len(chunk))+`)
			ORDER BY entry_id ASC, id ASC`, args...)
		if err != nil {
			return fmt.Errorf("querying recall evidence: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			e, err := scanRecallEvidenceRow(rows)
			if err != nil {
				return fmt.Errorf("scanning recall evidence: %w", err)
			}
			result[e.EntryID] = append(result[e.EntryID], e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func toCoreRecallEntries(entries []RecallEntry) []corerecall.Entry {
	result := make([]corerecall.Entry, 0, len(entries))
	for _, m := range entries {
		result = append(result, corerecall.Entry{
			ID:          m.ID,
			Type:        m.Type,
			Scope:       m.Scope,
			Status:      m.Status,
			ReviewState: m.ReviewState,
			Title:       m.Title,
			Body:        m.Body,
			Trigger:     m.Trigger,
			Confidence:  m.Confidence,
			Uncertainty: m.Uncertainty,
			Project:     m.Project,
			CWD:         m.CWD,
			GitBranch:   m.GitBranch,
			Agent:       m.Agent,
			CreatedAt:   m.CreatedAt,
			UpdatedAt:   m.UpdatedAt,
			Evidence:    toCoreEvidence(m.Evidence),
		})
	}
	return result
}

func toCoreEvidence(evidence []RecallEvidence) []corerecall.Evidence {
	result := make([]corerecall.Evidence, 0, len(evidence))
	for _, e := range evidence {
		result = append(result, corerecall.Evidence{
			SessionID:           e.SessionID,
			MessageStartOrdinal: e.MessageStartOrdinal,
			MessageEndOrdinal:   e.MessageEndOrdinal,
			ToolUseID:           e.ToolUseID,
			Snippet:             e.Snippet,
		})
	}
	return result
}
