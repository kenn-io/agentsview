package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	corememory "go.kenn.io/agentsview/internal/memory"
)

const (
	DefaultMemoryLimit               = 50
	MaxMemoryLimit                   = 500
	MaxMemorySearchTerms             = 12
	memoryFTS4PreselectLimit         = 50000
	memoryEvidenceFTS4PreselectLimit = 50000
)

type Memory struct {
	ID                   string           `json:"id"`
	Type                 string           `json:"type"`
	Scope                string           `json:"scope"`
	Status               string           `json:"status"`
	Title                string           `json:"title"`
	Body                 string           `json:"body"`
	Trigger              string           `json:"trigger,omitempty"`
	Confidence           *float64         `json:"confidence,omitempty"`
	Uncertainty          string           `json:"uncertainty,omitempty"`
	Project              string           `json:"project,omitempty"`
	CWD                  string           `json:"cwd,omitempty"`
	GitBranch            string           `json:"git_branch,omitempty"`
	Agent                string           `json:"agent,omitempty"`
	SourceSessionID      string           `json:"source_session_id"`
	SourceEpisodeID      string           `json:"source_episode_id,omitempty"`
	SourceRunID          string           `json:"source_run_id,omitempty"`
	ExtractorMethod      string           `json:"extractor_method,omitempty"`
	Model                string           `json:"model,omitempty"`
	Transferable         bool             `json:"transferable"`
	ProvenanceOK         bool             `json:"provenance_ok"`
	SupersedesMemoryID   string           `json:"supersedes_memory_id,omitempty"`
	SupersededByMemoryID string           `json:"superseded_by_memory_id,omitempty"`
	CreatedAt            string           `json:"created_at"`
	UpdatedAt            string           `json:"updated_at"`
	Evidence             []MemoryEvidence `json:"evidence,omitempty"`
}

// LifecycleBucket classifies a memory's supersession lifecycle as one of
// "active", "replacement", "superseded", or "replacement_superseded". An
// archived memory with no explicit superseded-by link is treated as superseded.
// It is the single classifier shared by `memory stats` and
// `memory query --summary` so both report identical by_lifecycle buckets.
func (m Memory) LifecycleBucket() string {
	switch {
	case m.SupersedesMemoryID != "" && m.SupersededByMemoryID != "":
		return "replacement_superseded"
	case m.SupersedesMemoryID != "":
		return "replacement"
	case m.SupersededByMemoryID != "" || m.Status == corememory.StatusArchived:
		return "superseded"
	default:
		return "active"
	}
}

type MemoryEvidence struct {
	ID                  int64  `json:"id"`
	MemoryID            string `json:"memory_id"`
	SessionID           string `json:"session_id"`
	MessageStartOrdinal int    `json:"message_start_ordinal"`
	MessageEndOrdinal   int    `json:"message_end_ordinal"`
	ToolUseID           string `json:"tool_use_id,omitempty"`
	Snippet             string `json:"snippet,omitempty"`
}

type MemoryQuery struct {
	Text                 string
	Project              string
	CWD                  string
	GitBranch            string
	Agent                string
	Type                 string
	Scope                string
	Status               string
	ExtractorMethod      string
	SourceSessionID      string
	SourceEpisodeID      string
	SourceRunID          string
	SupersedesMemoryID   string
	SupersededByMemoryID string
	TrustedOnly          bool
	Limit                int
}

type MemoryResult struct {
	Memory
	Score          float64                   `json:"score"`
	ScoreBreakdown corememory.ScoreBreakdown `json:"score_breakdown"`
	MatchReasons   []string                  `json:"match_reasons,omitempty"`
	MatchedTerms   []string                  `json:"matched_terms,omitempty"`
}

type MemoryPage struct {
	Memories []MemoryResult `json:"memories"`
}

const memoryBaseCols = `id, type, scope, status, title, body, trigger,
	confidence, uncertainty, project, cwd, git_branch, agent,
	source_session_id, source_episode_id, source_run_id, extractor_method,
	model, transferable, provenance_ok, supersedes_memory_id,
	superseded_by_memory_id, created_at, updated_at`

const memoryBaseColsQualified = `memories.id, memories.type,
	memories.scope, memories.status, memories.title, memories.body,
	memories.trigger, memories.confidence, memories.uncertainty,
	memories.project, memories.cwd, memories.git_branch, memories.agent,
	memories.source_session_id, memories.source_episode_id,
	memories.source_run_id, memories.extractor_method, memories.model,
	memories.transferable, memories.provenance_ok,
	memories.supersedes_memory_id, memories.superseded_by_memory_id,
	memories.created_at, memories.updated_at`

var memorySearchTokenPattern = regexp.MustCompile(`[A-Za-z0-9_]+`)
var memoryQuotedTextPattern = regexp.MustCompile("`([^`]+)`|'([^']+)'|\"([^\"]+)\"")

var errMemoryFTSCandidateQueryUnavailable = errors.New("memory fts candidate query unavailable")

func scanMemoryRow(rs rowScanner) (Memory, error) {
	var m Memory
	var confidence sql.NullFloat64
	err := rs.Scan(
		&m.ID, &m.Type, &m.Scope, &m.Status, &m.Title, &m.Body,
		&m.Trigger, &confidence, &m.Uncertainty, &m.Project,
		&m.CWD, &m.GitBranch, &m.Agent, &m.SourceSessionID,
		&m.SourceEpisodeID, &m.SourceRunID, &m.ExtractorMethod,
		&m.Model, &m.Transferable, &m.ProvenanceOK,
		&m.SupersedesMemoryID, &m.SupersededByMemoryID,
		&m.CreatedAt, &m.UpdatedAt,
	)
	if confidence.Valid {
		m.Confidence = &confidence.Float64
	}
	return m, err
}

func scanMemoryEvidenceRow(rs rowScanner) (MemoryEvidence, error) {
	var e MemoryEvidence
	err := rs.Scan(
		&e.ID, &e.MemoryID, &e.SessionID, &e.MessageStartOrdinal,
		&e.MessageEndOrdinal, &e.ToolUseID, &e.Snippet,
	)
	return e, err
}

func (db *DB) InsertMemory(m Memory) (string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if m.ID == "" {
		return "", fmt.Errorf("memory id is required")
	}
	if m.Status == "" {
		m.Status = corememory.StatusAccepted
	}

	tx, err := db.getWriter().Begin()
	if err != nil {
		return "", fmt.Errorf("begin memory insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertMemoryTx(tx, m); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit memory insert: %w", err)
	}
	return m.ID, nil
}

// CopyMemoriesFrom copies memories and their evidence from a source database
// into this database. A full resync rebuilds the DB from source files, which
// never contain memories, so without this copy every accepted memory is
// destroyed on resync (the DB is meant to be a persistent archive). Original
// timestamps are preserved so recency ranking survives.
//
// Only memories whose source session already exists in this DB are copied,
// keeping the source_session_id / session_id foreign keys valid. Sessions are
// copied earlier in the resync (re-synced or orphan-copied), so this normally
// covers every memory; parser-excluded sessions are the exception. Any skipped
// memories are logged.
func (db *DB) CopyMemoriesFrom(sourcePath string) error {
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
		return fmt.Errorf("begin memory copy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO memories (
			id, type, scope, status, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_memory_id, superseded_by_memory_id,
			created_at, updated_at
		)
		SELECT
			id, type, scope, status, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_memory_id, superseded_by_memory_id,
			created_at, updated_at
		FROM old_db.memories
		WHERE source_session_id IN (SELECT id FROM main.sessions)`)
	if err != nil {
		return fmt.Errorf("copying memories: %w", err)
	}
	copied, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting copied memories: %w", err)
	}

	var total int64
	if err := tx.QueryRowContext(
		ctx, "SELECT count(*) FROM old_db.memories",
	).Scan(&total); err != nil {
		return fmt.Errorf("counting source memories: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_evidence (
			memory_id, session_id, message_start_ordinal,
			message_end_ordinal, tool_use_id, snippet
		)
		SELECT
			memory_id, session_id, message_start_ordinal,
			message_end_ordinal, tool_use_id, snippet
		FROM old_db.memory_evidence
		WHERE memory_id IN (SELECT id FROM main.memories)
		  AND session_id IN (SELECT id FROM main.sessions)`); err != nil {
		return fmt.Errorf("copying memory evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory copy: %w", err)
	}

	if total > copied {
		log.Printf(
			"resync: copied %d/%d memories (%d skipped: "+
				"source session not preserved)",
			copied, total, total-copied,
		)
	}
	return nil
}

func (db *DB) SupersedeMemory(
	ctx context.Context,
	oldID string,
	replacement Memory,
) (string, error) {
	oldID = strings.TrimSpace(oldID)
	if oldID == "" {
		return "", fmt.Errorf("superseded memory id is required")
	}
	if replacement.ID == "" {
		return "", fmt.Errorf("replacement memory id is required")
	}
	if replacement.ID == oldID {
		return "", fmt.Errorf("replacement memory id must differ from superseded memory id")
	}
	if replacement.Status == "" {
		replacement.Status = corememory.StatusAccepted
	}
	if replacement.Status != corememory.StatusAccepted {
		return "", fmt.Errorf(
			"replacement memory status must be %q",
			corememory.StatusAccepted,
		)
	}
	replacement.SupersedesMemoryID = oldID
	replacement.SupersededByMemoryID = ""

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin memory supersede: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingOldID string
	if err := tx.QueryRowContext(
		ctx, "SELECT id FROM memories WHERE id = ?", oldID,
	).Scan(&existingOldID); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("superseded memory %s not found", oldID)
		}
		return "", fmt.Errorf("checking superseded memory %s: %w", oldID, err)
	}

	if err := insertMemoryTx(tx, replacement); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET status = ?,
		    superseded_by_memory_id = ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		corememory.StatusArchived, replacement.ID, oldID,
	)
	if err != nil {
		return "", fmt.Errorf("archiving superseded memory %s: %w", oldID, err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return "", fmt.Errorf("checking superseded memory update: %w", err)
	} else if rows != 1 {
		return "", fmt.Errorf("superseded memory %s not found", oldID)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit memory supersede: %w", err)
	}
	return replacement.ID, nil
}

func insertMemoryTx(tx *sql.Tx, m Memory) error {
	_, err := tx.Exec(`
		INSERT INTO memories (
			id, type, scope, status, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
				extractor_method, model, transferable, provenance_ok,
				supersedes_memory_id, superseded_by_memory_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Type, m.Scope, m.Status, m.Title, m.Body, m.Trigger,
		sqlFloat(m.Confidence), m.Uncertainty, m.Project, m.CWD, m.GitBranch,
		m.Agent, m.SourceSessionID, m.SourceEpisodeID, m.SourceRunID,
		m.ExtractorMethod, m.Model, m.Transferable, m.ProvenanceOK,
		m.SupersedesMemoryID, m.SupersededByMemoryID,
	)
	if err != nil {
		return fmt.Errorf("inserting memory: %w", err)
	}

	for _, e := range m.Evidence {
		if e.MemoryID == "" {
			e.MemoryID = m.ID
		}
		_, err = tx.Exec(`
			INSERT INTO memory_evidence (
				memory_id, session_id, message_start_ordinal,
				message_end_ordinal, tool_use_id, snippet
			) VALUES (?, ?, ?, ?, ?, ?)`,
			e.MemoryID, e.SessionID, e.MessageStartOrdinal,
			e.MessageEndOrdinal, e.ToolUseID, e.Snippet,
		)
		if err != nil {
			return fmt.Errorf("inserting memory evidence: %w", err)
		}
	}
	return nil
}

func (db *DB) GetMemory(ctx context.Context, id string) (*Memory, error) {
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+memoryBaseCols+" FROM memories WHERE id = ?",
		id,
	)
	m, err := scanMemoryRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting memory %s: %w", id, err)
	}
	evidence, err := db.listMemoryEvidence(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	m.Evidence = evidence[id]
	return &m, nil
}

func (db *DB) ListMemories(
	ctx context.Context, q MemoryQuery,
) ([]Memory, error) {
	q = NormalizeMemoryQuery(q)
	where, args := buildMemoryWhere(q, false)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseCols +
		" FROM memories WHERE " + where +
		" ORDER BY updated_at DESC, id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memories: %w", err)
	}
	defer rows.Close()

	memories, err := scanMemoryRows(rows)
	if err != nil {
		return nil, err
	}
	if len(memories) == 0 {
		return nil, nil
	}
	evidence, err := db.listMemoryEvidence(ctx, memoryIDs(memories))
	if err != nil {
		return nil, err
	}
	for i := range memories {
		memories[i].Evidence = evidence[memories[i].ID]
	}
	return memories, nil
}

func (db *DB) ListMemoryTextCandidates(
	ctx context.Context, q MemoryQuery,
) ([]Memory, error) {
	q = NormalizeMemoryQuery(q)
	terms := memoryQueryTerms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	direct, err := db.listMemoryFTSCandidates(ctx, q, terms)
	if err == nil {
		if len(direct) > 0 {
			return db.mergeMemoryCandidatesWithEvidence(ctx, q, terms, direct)
		}
	} else if !memoryFTSUnavailable(err) {
		return nil, err
	}
	direct, err = db.listMemoryLikeCandidates(ctx, q, terms)
	if err != nil {
		return nil, err
	}
	return db.mergeMemoryCandidatesWithEvidence(ctx, q, terms, direct)
}

func (db *DB) listMemoryFTSCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	kind := db.memoryFTSKind(ctx)
	switch kind {
	case "fts5":
		return db.listMemoryFTS5Candidates(ctx, q, terms)
	case "fts4":
		return db.listMemoryFTS4RowIDCandidates(ctx, q, terms)
	default:
		return nil, errMemoryFTSCandidateQueryUnavailable
	}
}

func (db *DB) listMemoryFTS5Candidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseColsQualified +
		" FROM memories_fts" +
		" JOIN memories ON memories.rowid = memories_fts.rowid" +
		" WHERE memories_fts MATCH ? AND " + where +
		" ORDER BY bm25(memories_fts), " +
		memoryStableSQLTieOrder("memories") +
		", memories.updated_at DESC, memories.id ASC LIMIT ?"
	args = append([]any{memoryFTSQuery(terms)}, args...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory fts candidates: %w", err)
	}
	defer rows.Close()

	candidates, err := scanMemoryRowsWithEvidence(ctx, db, rows)
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func (db *DB) listMemoryFTS4RowIDCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseCols +
		" FROM memories" +
		" WHERE rowid IN (" +
		"SELECT rowid FROM memories_fts" +
		" WHERE memories_fts MATCH ? LIMIT ?" +
		") AND " + where +
		" ORDER BY " + memoryStableSQLTieOrder("") +
		", updated_at DESC, id ASC LIMIT ?"
	args = append(
		[]any{memoryFTSQuery(terms), memoryFTS4PreselectLimit},
		args...,
	)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory fts4 candidates: %w", err)
	}
	defer rows.Close()

	return scanMemoryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listMemoryLikeCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	textWhere, textArgs := buildMemoryTextWhere(terms)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseCols +
		" FROM memories WHERE " + where +
		" AND (" + textWhere + ")" +
		" ORDER BY " + memoryStableSQLTieOrder("") +
		", updated_at DESC, id ASC LIMIT ?"
	args = append(args, textArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory like candidates: %w", err)
	}
	defer rows.Close()

	return scanMemoryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) memoryFTSKind(ctx context.Context) string {
	var ddl string
	err := db.getReader().QueryRowContext(
		ctx,
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'memories_fts'`,
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

func (db *DB) memoryEvidenceFTSKind(ctx context.Context) string {
	var ddl string
	err := db.getReader().QueryRowContext(
		ctx,
		`SELECT lower(sql) FROM sqlite_master
		 WHERE type = 'table' AND name = 'memory_evidence_fts'`,
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

func scanMemoryRowsWithEvidence(
	ctx context.Context, db *DB, rows *sql.Rows,
) ([]Memory, error) {
	memories, err := scanMemoryRows(rows)
	if err != nil {
		return nil, err
	}
	if len(memories) == 0 {
		return nil, nil
	}
	evidence, err := db.listMemoryEvidence(ctx, memoryIDs(memories))
	if err != nil {
		return nil, err
	}
	for i := range memories {
		memories[i].Evidence = evidence[memories[i].ID]
	}
	return memories, nil
}

func (db *DB) mergeMemoryCandidatesWithEvidence(
	ctx context.Context,
	q MemoryQuery,
	terms []string,
	direct []Memory,
) ([]Memory, error) {
	evidence, err := db.listMemoryEvidenceTextCandidates(ctx, q, terms)
	if err != nil {
		return nil, err
	}
	if len(direct) == 0 {
		return evidence, nil
	}
	seen := make(map[string]struct{}, len(direct)+len(evidence))
	out := make([]Memory, 0, len(direct)+len(evidence))
	for _, memory := range direct {
		seen[memory.ID] = struct{}{}
		out = append(out, memory)
	}
	for _, memory := range evidence {
		if _, ok := seen[memory.ID]; ok {
			continue
		}
		seen[memory.ID] = struct{}{}
		out = append(out, memory)
	}
	return out, nil
}

func (db *DB) listMemoryEvidenceTextCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	fts, err := db.listMemoryEvidenceFTSCandidates(ctx, q, terms)
	if err == nil {
		if len(fts) > 0 {
			return fts, nil
		}
	} else if !memoryFTSUnavailable(err) {
		return nil, err
	}
	return db.listMemoryEvidenceLikeCandidates(ctx, q, terms)
}

func (db *DB) listMemoryEvidenceFTSCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	kind := db.memoryEvidenceFTSKind(ctx)
	switch kind {
	case "fts4":
		return db.listMemoryEvidenceFTS4PreselectedCandidates(ctx, q, terms)
	case "fts5":
		return db.listMemoryEvidenceFTSScoredCandidates(ctx, q, terms)
	default:
		return nil, errMemoryFTSCandidateQueryUnavailable
	}
}

func (db *DB) listMemoryEvidenceFTSScoredCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	scoreExpr, scoreArgs := buildMemoryEvidenceMatchScoreExpr(terms)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseColsQualified +
		" FROM memory_evidence_fts" +
		" JOIN memory_evidence" +
		" ON memory_evidence.id = memory_evidence_fts.rowid" +
		" JOIN memories ON memories.id = memory_evidence.memory_id" +
		" WHERE memory_evidence_fts MATCH ? AND " + where +
		" GROUP BY memories.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		memoryStableSQLTieOrder("memories") +
		", memories.updated_at DESC, memories.id ASC LIMIT ?"
	args = append([]any{memoryFTSQuery(terms)}, args...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory evidence fts candidates: %w", err)
	}
	defer rows.Close()

	return scanMemoryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listMemoryEvidenceFTS4PreselectedCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	scoreExpr, scoreArgs := buildMemoryEvidenceMatchScoreExpr(terms)
	limit := memoryLimit(q.Limit)
	query := "WITH matched_evidence(rowid) AS (" +
		"SELECT rowid FROM memory_evidence_fts" +
		" WHERE memory_evidence_fts MATCH ? LIMIT ?" +
		") SELECT " + memoryBaseColsQualified +
		" FROM matched_evidence" +
		" JOIN memory_evidence ON memory_evidence.id = matched_evidence.rowid" +
		" JOIN memories ON memories.id = memory_evidence.memory_id" +
		" WHERE " + where +
		" GROUP BY memories.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		memoryStableSQLTieOrder("memories") +
		", memories.updated_at DESC, memories.id ASC LIMIT ?"
	args = append(
		[]any{memoryFTSQuery(terms), memoryEvidenceFTS4PreselectLimit},
		args...,
	)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying memory evidence fts4 candidates: %w", err,
		)
	}
	defer rows.Close()

	return scanMemoryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) listMemoryEvidenceLikeCandidates(
	ctx context.Context, q MemoryQuery, terms []string,
) ([]Memory, error) {
	where, args := buildMemoryWhere(q, false)
	textWhere, textArgs := buildMemoryEvidenceTextWhere(terms)
	scoreExpr, scoreArgs := buildMemoryEvidenceMatchScoreExpr(terms)
	limit := memoryLimit(q.Limit)
	query := "SELECT " + memoryBaseColsQualified +
		" FROM memories" +
		" JOIN memory_evidence ON memory_evidence.memory_id = memories.id" +
		" WHERE " + where +
		" AND (" + textWhere + ")" +
		" GROUP BY memories.id" +
		" ORDER BY " + scoreExpr + " DESC, " +
		memoryStableSQLTieOrder("memories") +
		", memories.updated_at DESC, memories.id ASC LIMIT ?"
	args = append(args, textArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory evidence candidates: %w", err)
	}
	defer rows.Close()

	return scanMemoryRowsWithEvidence(ctx, db, rows)
}

func (db *DB) QueryMemories(
	ctx context.Context, q MemoryQuery,
) (MemoryPage, error) {
	q = NormalizeMemoryQuery(q)
	if strings.TrimSpace(q.Text) == "" {
		memories, err := db.ListMemories(ctx, q)
		if err != nil {
			return MemoryPage{}, err
		}
		return memoryPageFromList(memories), nil
	}

	candidateQuery := q
	candidateQuery.Limit = MaxMemoryLimit
	candidates, err := db.ListMemoryTextCandidates(ctx, candidateQuery)
	if err != nil {
		return MemoryPage{}, err
	}
	if len(candidates) == 0 {
		candidates, err = db.ListMemories(ctx, candidateQuery)
		if err != nil {
			return MemoryPage{}, err
		}
	}
	results := corememory.Rank(toCoreMemories(candidates), corememory.Query{
		Text:      q.Text,
		Project:   q.Project,
		CWD:       q.CWD,
		GitBranch: q.GitBranch,
		Agent:     q.Agent,
		Status:    q.Status,
		Limit:     len(candidates),
	})
	byID := make(map[string]Memory, len(candidates))
	for _, m := range candidates {
		byID[m.ID] = m
	}
	sortMemoryResultsByStableSource(results, byID)
	results = diversifyMemoryResults(results, byID, memoryLimit(q.Limit))
	page := MemoryPage{Memories: make([]MemoryResult, 0, len(results))}
	for _, result := range results {
		m := byID[result.Memory.ID]
		page.Memories = append(page.Memories, MemoryResult{
			Memory:         m,
			Score:          result.Score,
			ScoreBreakdown: result.Breakdown,
			MatchReasons:   memoryMatchReasons(result.Breakdown),
			MatchedTerms:   result.MatchedTerms,
		})
	}
	return page, nil
}

// NormalizeMemoryQuery trims whitespace from a query's exact-match filters so
// padded values match consistently. It is exported so the Postgres store can
// apply the same normalization as the SQLite store.
func NormalizeMemoryQuery(q MemoryQuery) MemoryQuery {
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
	q.SupersedesMemoryID = strings.TrimSpace(q.SupersedesMemoryID)
	q.SupersededByMemoryID = strings.TrimSpace(q.SupersededByMemoryID)
	return q
}

func memoryPageFromList(memories []Memory) MemoryPage {
	page := MemoryPage{Memories: make([]MemoryResult, 0, len(memories))}
	for _, memory := range memories {
		page.Memories = append(page.Memories, MemoryResult{Memory: memory})
	}
	return page
}

func sortMemoryResultsByStableSource(
	results []corememory.Result,
	byID map[string]Memory,
) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		left := byID[results[i].Memory.ID]
		right := byID[results[j].Memory.ID]
		return memoryStableSortKey(left, results[i].Memory.ID) <
			memoryStableSortKey(right, results[j].Memory.ID)
	})
}

func memoryStableSortKey(m Memory, fallbackID string) string {
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

func memoryMatchReasons(b corememory.ScoreBreakdown) []string {
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

func diversifyMemoryResults(
	results []corememory.Result,
	byID map[string]Memory,
	limit int,
) []corememory.Result {
	if limit <= 0 || len(results) <= limit {
		return results
	}
	usedIDs := make(map[string]bool, limit)
	usedSources := make(map[string]bool, limit)
	out := make([]corememory.Result, 0, limit)
	for _, result := range results {
		source := memorySourceDiversityKey(byID[result.Memory.ID])
		if source == "" || usedSources[source] {
			continue
		}
		out = append(out, result)
		usedIDs[result.Memory.ID] = true
		usedSources[source] = true
		if len(out) >= limit {
			return out
		}
	}
	for _, result := range results {
		if usedIDs[result.Memory.ID] {
			continue
		}
		out = append(out, result)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func memorySourceDiversityKey(m Memory) string {
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

func scanMemoryRows(rows *sql.Rows) ([]Memory, error) {
	var memories []Memory
	for rows.Next() {
		m, err := scanMemoryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning memory: %w", err)
		}
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

func buildMemoryWhere(q MemoryQuery, includeText bool) (string, []any) {
	preds := []string{"1=1"}
	var args []any
	status := q.Status
	if status == "" {
		status = corememory.StatusAccepted
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
	if q.SupersedesMemoryID != "" {
		preds = append(preds, "supersedes_memory_id = ?")
		args = append(args, q.SupersedesMemoryID)
	}
	if q.SupersededByMemoryID != "" {
		preds = append(preds, "superseded_by_memory_id = ?")
		args = append(args, q.SupersededByMemoryID)
	}
	if q.TrustedOnly {
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

func buildMemoryTextWhere(terms []string) (string, []any) {
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

func buildMemoryEvidenceTextWhere(terms []string) (string, []any) {
	preds := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		preds = append(preds, "memory_evidence.snippet LIKE ? ESCAPE '\\'")
		args = append(args, like)
	}
	return strings.Join(preds, " OR "), args
}

func buildMemoryEvidenceMatchScoreExpr(terms []string) (string, []any) {
	parts := make([]string, 0, len(terms))
	var args []any
	for _, term := range terms {
		like := "%" + escapeLike(term) + "%"
		parts = append(parts, "MAX(CASE WHEN memory_evidence.snippet LIKE ? ESCAPE '\\' THEN 1 ELSE 0 END)")
		args = append(args, like)
	}
	return strings.Join(parts, " + "), args
}

func memoryStableSQLTieOrder(table string) string {
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

func memoryFTSQuery(terms []string) string {
	return strings.Join(terms, " OR ")
}

func memoryFTSUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errMemoryFTSCandidateQueryUnavailable) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table: memories_fts") ||
		strings.Contains(msg, "no such table: memory_evidence_fts") ||
		strings.Contains(msg, "no such module") ||
		strings.Contains(msg, "unable to use function MATCH")
}

func memoryQueryTerms(text string) []string {
	queryText := corememory.LexicalQueryText(text)
	priority := memoryQuotedQueryTerms(queryText)
	raw := memorySearchTokenPattern.FindAllString(strings.ToLower(queryText), -1)
	seen := make(map[string]struct{}, len(raw))
	terms := make([]string, 0, len(raw))
	for _, token := range priority {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
	}
	var rest []string
	for _, token := range raw {
		if !validMemoryQueryTerm(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		rest = append(rest, token)
	}
	sortMemoryQueryTerms(rest)
	terms = append(terms, rest...)
	if len(terms) > MaxMemorySearchTerms {
		terms = terms[:MaxMemorySearchTerms]
	}
	return terms
}

func memoryQuotedQueryTerms(text string) []string {
	var terms []string
	for _, match := range memoryQuotedTextPattern.FindAllStringSubmatch(text, -1) {
		quoted := firstNonEmptyString(match[1:])
		for _, token := range memorySearchTokenPattern.FindAllString(
			strings.ToLower(quoted),
			-1,
		) {
			if validMemoryQueryTerm(token) {
				terms = append(terms, token)
			}
		}
	}
	sortMemoryQueryTerms(terms)
	return terms
}

func validMemoryQueryTerm(token string) bool {
	if memorySearchStopwords[token] {
		return false
	}
	return len(token) >= 3 || memorySearchShortToken(token)
}

func sortMemoryQueryTerms(terms []string) {
	sort.SliceStable(terms, func(i, j int) bool {
		if len(terms[i]) != len(terms[j]) {
			return len(terms[i]) > len(terms[j])
		}
		return terms[i] < terms[j]
	})
}

func firstNonEmptyString(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func memorySearchShortToken(token string) bool {
	switch token {
	case "go", "js", "ts", "py", "rs", "id":
		return true
	default:
		return false
	}
}

var memorySearchStopwords = map[string]bool{
	"about":      true,
	"accomplish": true,
	"agent":      true,
	"and":        true,
	"answer":     true,
	"are":        true,
	"asked":      true,
	"between":    true,
	"contain":    true,
	"contains":   true,
	"first":      true,
	"for":        true,
	"final":      true,
	"from":       true,
	"given":      true,
	"have":       true,
	"help":       true,
	"label":      true,
	"labels":     true,
	"mentions":   true,
	"more":       true,
	"name":       true,
	"names":      true,
	"phrases":    true,
	"our":        true,
	"past":       true,
	"question":   true,
	"retrieve":   true,
	"second":     true,
	"several":    true,
	"short":      true,
	"should":     true,
	"specific":   true,
	"task":       true,
	"tell":       true,
	"that":       true,
	"the":        true,
	"this":       true,
	"trajectory": true,
	"typically":  true,
	"what":       true,
	"when":       true,
	"where":      true,
	"which":      true,
	"with":       true,
	"working":    true,
}

func memoryLimit(limit int) int {
	if limit <= 0 || limit > MaxMemoryLimit {
		return DefaultMemoryLimit
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

func memoryIDs(memories []Memory) []string {
	ids := make([]string, 0, len(memories))
	for _, m := range memories {
		ids = append(ids, m.ID)
	}
	return ids
}

func (db *DB) listMemoryEvidence(
	ctx context.Context, ids []string,
) (map[string][]MemoryEvidence, error) {
	result := make(map[string][]MemoryEvidence, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, memory_id, session_id, message_start_ordinal,
			message_end_ordinal, tool_use_id, snippet
		FROM memory_evidence
		WHERE memory_id IN (`+listPlaceholders(len(ids))+`)
		ORDER BY memory_id ASC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying memory evidence: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e, err := scanMemoryEvidenceRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning memory evidence: %w", err)
		}
		result[e.MemoryID] = append(result[e.MemoryID], e)
	}
	return result, rows.Err()
}

func toCoreMemories(memories []Memory) []corememory.Memory {
	result := make([]corememory.Memory, 0, len(memories))
	for _, m := range memories {
		result = append(result, corememory.Memory{
			ID:          m.ID,
			Type:        m.Type,
			Scope:       m.Scope,
			Status:      m.Status,
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

func toCoreEvidence(evidence []MemoryEvidence) []corememory.Evidence {
	result := make([]corememory.Evidence, 0, len(evidence))
	for _, e := range evidence {
		result = append(result, corememory.Evidence{
			SessionID:           e.SessionID,
			MessageStartOrdinal: e.MessageStartOrdinal,
			MessageEndOrdinal:   e.MessageEndOrdinal,
			ToolUseID:           e.ToolUseID,
			Snippet:             e.Snippet,
		})
	}
	return result
}
