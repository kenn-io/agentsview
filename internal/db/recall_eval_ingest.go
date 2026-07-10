package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

// defaultEvalChunkChars is the rune-window size for splitting a flattened
// trajectory into raw-chunk entries (~500 tokens). Finer than extraction
// chunk's 12000-char default so keyword recall can pinpoint passages. It is a
// server-side tuning knob, deliberately a constant rather than a request field.
const defaultEvalChunkChars = 2000

// maxEvalFieldRunes caps the length of the required identifier-like eval
// ingest fields (extractor_method, source_version) so a misbehaving caller
// cannot stash arbitrarily large text in columns meant for short labels.
const maxEvalFieldRunes = 200

const (
	evalTrajectoryMachine        = "recall-eval-ingest"
	defaultEvalTrajectoryProject = "eval-harness"
	defaultEvalTrajectoryAgent   = "eval-harness"
)

// EvalTrajectoryIngest is the input to IngestEvalTrajectory: one raw eval
// trajectory plus its run scope, harness identity, and optional provenance
// fields. ExtractorMethod and SourceVersion are caller-supplied so this
// generic raw-chunk ingest path carries no knowledge of any specific
// benchmark or harness.
type EvalTrajectoryIngest struct {
	RunID           string
	TrajectoryID    string
	Trajectory      json.RawMessage
	ExtractorMethod string
	SourceVersion   string
	Project         string
	CWD             string
	GitBranch       string
	Agent           string
}

// EvalTrajectoryIngestResult reports how many chunk entries were newly
// inserted for the trajectory. An idempotent re-ingest reports 0.
type EvalTrajectoryIngestResult struct {
	RunID          string `json:"run_id"`
	TrajectoryID   string `json:"trajectory_id"`
	CorpusID       string `json:"corpus_id,omitempty"`
	EntriesIndexed int    `json:"entries_indexed"`
}

// IngestEvalTrajectory chunks a raw eval trajectory into FTS-indexed recall
// rows scoped by run_id + extractor_method, so the keyword retriever can
// recall them. It is lab-only (the HTTP layer guards the
// production data dir) and idempotent: deterministic ids mean re-ingesting a
// trajectory with the same extractor, source version, and normalized content
// inserts only the chunks it is missing. Changing any of those identity inputs
// creates a distinct eval corpus instead of silently retaining stale rows. It
// mirrors the /import write path — a placeholder session satisfies the
// source_session_id FK, then each chunk is inserted only if absent.
func (db *DB) IngestEvalTrajectory(
	ctx context.Context, in EvalTrajectoryIngest,
) (EvalTrajectoryIngestResult, error) {
	in = normalizeEvalTrajectoryIngest(in)
	result := EvalTrajectoryIngestResult{
		RunID:        in.RunID,
		TrajectoryID: in.TrajectoryID,
	}
	if in.RunID == "" || in.TrajectoryID == "" {
		return result, fmt.Errorf("run_id and trajectory_id are required")
	}
	if err := validateEvalRequiredField("extractor_method", in.ExtractorMethod); err != nil {
		return result, err
	}
	if err := validateEvalRequiredField("source_version", in.SourceVersion); err != nil {
		return result, err
	}
	if len(in.Trajectory) == 0 {
		return result, fmt.Errorf("trajectory is required")
	}
	text, err := flattenTrajectoryText(in.Trajectory)
	if err != nil {
		return result, err
	}
	chunks := chunkText(text, defaultEvalChunkChars)
	if len(chunks) == 0 {
		return result, nil
	}
	contentDigest := evalTrajectoryContentDigest(text)
	sessionID, err := evalTrajectorySessionID(in, contentDigest)
	if err != nil {
		return result, err
	}
	result.CorpusID = sessionID
	entries := make([]RecallEntry, 0, len(chunks))
	for idx, chunk := range chunks {
		id, err := evalTrajectoryChunkID(in, contentDigest, idx)
		if err != nil {
			return result, err
		}
		entries = append(entries, newEvalChunkRecallEntry(
			id, sessionID, in, idx, len(chunks), chunk,
		))
	}
	indexed, err := db.ingestEvalTrajectoryChunks(
		ctx, newEvalTrajectorySession(sessionID, in), entries,
	)
	if err != nil {
		return result, err
	}
	result.EntriesIndexed = indexed
	return result, nil
}

func normalizeEvalTrajectoryIngest(
	in EvalTrajectoryIngest,
) EvalTrajectoryIngest {
	in.RunID = strings.TrimSpace(in.RunID)
	in.TrajectoryID = strings.TrimSpace(in.TrajectoryID)
	in.ExtractorMethod = strings.TrimSpace(in.ExtractorMethod)
	in.SourceVersion = strings.TrimSpace(in.SourceVersion)
	in.Project = strings.TrimSpace(in.Project)
	in.CWD = strings.TrimSpace(in.CWD)
	in.GitBranch = strings.TrimSpace(in.GitBranch)
	in.Agent = strings.TrimSpace(in.Agent)
	if in.Project == "" {
		in.Project = defaultEvalTrajectoryProject
	}
	if in.Agent == "" {
		in.Agent = defaultEvalTrajectoryAgent
	}
	return in
}

// validateEvalRequiredField reports an error if value is empty or exceeds
// maxEvalFieldRunes once trimmed. value must already be trimmed (see
// normalizeEvalTrajectoryIngest, which runs before this check).
func validateEvalRequiredField(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len([]rune(value)) > maxEvalFieldRunes {
		return fmt.Errorf("%s exceeds maximum length of %d characters", field, maxEvalFieldRunes)
	}
	return nil
}

// flattenTrajectoryText walks an arbitrary trajectory JSON value and returns
// all of its string leaves joined by newlines. Object keys are visited in
// sorted order and arrays in their natural order, so the same trajectory always
// flattens to byte-identical text — which keeps chunk ids, and thus
// idempotency, stable. Keys are structural and skipped; non-string scalars
// (numbers, booleans, null) carry no searchable text and are skipped too.
func flattenTrajectoryText(raw json.RawMessage) (string, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", fmt.Errorf("parsing trajectory: %w", err)
	}
	var b strings.Builder
	appendTrajectoryStrings(&b, root)
	return b.String(), nil
}

func appendTrajectoryStrings(b *strings.Builder, node any) {
	switch n := node.(type) {
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	case []any:
		for _, item := range n {
			appendTrajectoryStrings(b, item)
		}
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			appendTrajectoryStrings(b, n[k])
		}
	}
}

// chunkText splits s into windows of at most size runes (not bytes, so
// multi-byte characters are never split mid-rune). An empty string yields no
// chunks. A non-positive size falls back to defaultEvalChunkChars.
func chunkText(s string, size int) []string {
	if size <= 0 {
		size = defaultEvalChunkChars
	}
	if s == "" {
		return nil
	}
	runes := []rune(s)
	chunks := make([]string, 0, (len(runes)+size-1)/size)
	for start := 0; start < len(runes); start += size {
		end := min(start+size, len(runes))
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

// evalIngestID derives a deterministic, collision-safe id as the SHA-256 hex of
// the JSON-encoded tuple of parts (e.g. ["eval-trajectory", runID, trajID,
// idx]). JSON length-delimits the parts, so ids cannot collide through
// delimiter confusion the way raw string concatenation could.
func evalIngestID(parts ...any) (string, error) {
	encoded, err := json.Marshal(parts)
	if err != nil {
		return "", fmt.Errorf("encoding eval id parts: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func evalTrajectoryContentDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func evalTrajectorySessionID(
	in EvalTrajectoryIngest, contentDigest string,
) (string, error) {
	return evalIngestID(
		"eval-trajectory-session",
		in.RunID,
		in.TrajectoryID,
		in.ExtractorMethod,
		in.SourceVersion,
		contentDigest,
	)
}

func evalTrajectoryChunkID(
	in EvalTrajectoryIngest, contentDigest string, idx int,
) (string, error) {
	return evalIngestID(
		"eval-trajectory",
		in.RunID,
		in.TrajectoryID,
		in.ExtractorMethod,
		in.SourceVersion,
		contentDigest,
		idx,
	)
}

// newEvalTrajectorySession builds the placeholder session required by the
// chunk entries' source_session_id foreign key.
func newEvalTrajectorySession(
	sessionID string, in EvalTrajectoryIngest,
) Session {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	firstMessage := "Eval trajectory " + in.TrajectoryID
	displayName := firstMessage
	return Session{
		ID:                sessionID,
		Project:           in.Project,
		Machine:           evalTrajectoryMachine,
		Agent:             in.Agent,
		FirstMessage:      &firstMessage,
		DisplayName:       &displayName,
		StartedAt:         &now,
		EndedAt:           &now,
		MessageCount:      0,
		UserMessageCount:  0,
		Outcome:           "unknown",
		OutcomeConfidence: "low",
		Cwd:               in.CWD,
		GitBranch:         in.GitBranch,
		SourceVersion:     in.SourceVersion,
	}
}

func (db *DB) ingestEvalTrajectoryChunks(
	ctx context.Context, session Session, entries []RecallEntry,
) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin eval trajectory ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateRecallImportPlaceholderSessionStateWithQueryer(
		ctx, tx, session.ID,
	); err != nil {
		return 0, fmt.Errorf("preparing eval session: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx, insertSessionIfAbsentSQL, upsertSessionArgs(session)...,
	); err != nil {
		return 0, fmt.Errorf("preparing eval session: %w", err)
	}
	indexed := 0
	for idx, entry := range entries {
		inserted, err := insertEvalRecallEntryIfAbsentTx(ctx, tx, entry)
		if err != nil {
			return 0, fmt.Errorf("inserting chunk %d: %w", idx, err)
		}
		if inserted {
			indexed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit eval trajectory ingest: %w", err)
	}
	return indexed, nil
}

func insertEvalRecallEntryIfAbsentTx(
	ctx context.Context, tx *sql.Tx, entry RecallEntry,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_entries (
			id, type, scope, status, review_state, title, body, trigger,
			confidence, uncertainty, project, cwd, git_branch, agent,
			source_session_id, source_episode_id, source_run_id,
			extractor_method, model, transferable, provenance_ok,
			supersedes_entry_id, superseded_by_entry_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.Type, entry.Scope, entry.Status, entry.ReviewState,
		entry.Title, entry.Body, entry.Trigger, sqlFloat(entry.Confidence),
		entry.Uncertainty, entry.Project, entry.CWD, entry.GitBranch, entry.Agent,
		entry.SourceSessionID, entry.SourceEpisodeID, entry.SourceRunID,
		entry.ExtractorMethod, entry.Model, entry.Transferable, entry.ProvenanceOK,
		entry.SupersedesEntryID, entry.SupersededByEntryID,
	)
	if err != nil {
		return false, fmt.Errorf("inserting recall entry: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting inserted recall entry: %w", err)
	}
	return rows == 1, nil
}

// newEvalChunkRecallEntry builds the raw-chunk recall row for one chunk.
// Confidence is left nil (raw chunks earn no confidence ranking bonus).
// Transferability and provenance remain measurable, while eval_raw keeps these
// rows quarantined from trusted-only recall.
func newEvalChunkRecallEntry(
	id, sessionID string, in EvalTrajectoryIngest, idx, total int, body string,
) RecallEntry {
	return RecallEntry{
		ID:              id,
		Type:            corerecall.TypeFact,
		Scope:           corerecall.ScopeRepository,
		Status:          corerecall.StatusAccepted,
		ReviewState:     corerecall.ReviewStateEvalRaw,
		Title:           fmt.Sprintf("%s [chunk %d/%d]", in.TrajectoryID, idx+1, total),
		Body:            body,
		Project:         in.Project,
		CWD:             in.CWD,
		GitBranch:       in.GitBranch,
		Agent:           in.Agent,
		SourceSessionID: sessionID,
		SourceEpisodeID: fmt.Sprintf("%s:chunk:%d", in.TrajectoryID, idx),
		SourceRunID:     in.RunID,
		ExtractorMethod: in.ExtractorMethod,
		Transferable:    true,
		ProvenanceOK:    true,
	}
}
