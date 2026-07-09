package db

import (
	"context"
	"crypto/sha256"
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
	EntriesIndexed int    `json:"entries_indexed"`
}

// IngestEvalTrajectory chunks a raw eval trajectory into FTS-indexed recall
// rows scoped by run_id + extractor_method, so the keyword retriever can
// recall them. It is lab-only (the HTTP layer guards the
// production data dir) and idempotent: deterministic ids mean re-ingesting a
// trajectory inserts only the chunks it is missing. It mirrors the /import
// write path — a placeholder session satisfies the source_session_id FK, then
// each chunk is inserted only if absent.
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
	sessionID, err := evalIngestID(
		"eval-trajectory-session", in.RunID, in.TrajectoryID,
	)
	if err != nil {
		return result, err
	}
	if err := db.ensureEvalTrajectorySession(ctx, sessionID, in); err != nil {
		return result, fmt.Errorf("preparing eval session: %w", err)
	}
	for idx, chunk := range chunks {
		id, err := evalIngestID(
			"eval-trajectory", in.RunID, in.TrajectoryID, idx,
		)
		if err != nil {
			return result, err
		}
		existing, err := db.GetRecallEntry(ctx, id)
		if err != nil {
			return result, fmt.Errorf("checking duplicate chunk: %w", err)
		}
		if existing != nil {
			continue
		}
		if _, err := db.InsertRecallEntry(
			newEvalChunkRecallEntry(id, sessionID, in, idx, len(chunks), chunk),
		); err != nil {
			return result, fmt.Errorf("inserting chunk %d: %w", idx, err)
		}
		result.EntriesIndexed++
	}
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

// ensureEvalTrajectorySession inserts a placeholder session (idempotently) so
// the chunk entries' source_session_id FK is satisfied, mirroring
// ensureRecallImportSession. The source_version marker distinguishes eval
// sessions from real synced ones.
func (db *DB) ensureEvalTrajectorySession(
	ctx context.Context, sessionID string, in EvalTrajectoryIngest,
) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	firstMessage := "Eval trajectory " + in.TrajectoryID
	displayName := firstMessage
	return db.insertSessionIfAbsent(ctx, Session{
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
	})
}

// newEvalChunkRecallEntry builds the raw-chunk recall row for one chunk. Confidence
// is left nil (raw chunks earn no confidence ranking bonus); transferable +
// provenance_ok are true so trusted_only queries retrieve them.
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
