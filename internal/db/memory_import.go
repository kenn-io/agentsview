package db

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	corememory "go.kenn.io/agentsview/internal/memory"
)

type MemoryImportResult struct {
	Imported            int                `json:"imported"`
	Skipped             int                `json:"skipped"`
	WouldImport         int                `json:"would_import,omitempty"`
	ImportedMemories    []MemoryImportItem `json:"imported_memories,omitempty"`
	WouldImportMemories []MemoryImportItem `json:"would_import_memories,omitempty"`
	SkippedMemories     []MemoryImportItem `json:"skipped_memories,omitempty"`
}

type MemoryImportItem struct {
	CandidateID        string `json:"candidate_id"`
	Title              string `json:"title,omitempty"`
	SourceSessionID    string `json:"source_session_id,omitempty"`
	SupersedesMemoryID string `json:"supersedes_memory_id,omitempty"`
	Label              string `json:"label,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

type MemoryImportOptions struct {
	DryRun                  bool
	RequireExistingSessions bool
	AllowProductionImport   bool
}

type probeAcceptedMemory struct {
	CandidateID        string              `json:"candidate_id"`
	RunID              string              `json:"run_id"`
	SessionID          string              `json:"session_id"`
	EpisodeID          string              `json:"episode_id"`
	Type               string              `json:"type"`
	Scope              string              `json:"scope"`
	Title              string              `json:"title"`
	Body               string              `json:"body"`
	Trigger            string              `json:"trigger"`
	Confidence         *float64            `json:"confidence"`
	Uncertainty        string              `json:"uncertainty"`
	Project            string              `json:"project"`
	CWD                string              `json:"cwd"`
	GitBranch          string              `json:"git_branch"`
	Agent              string              `json:"agent"`
	ExtractorMethod    string              `json:"extractor_method"`
	Model              string              `json:"model"`
	Label              string              `json:"label"`
	SupersedesMemoryID string              `json:"supersedes_memory_id"`
	Transferable       bool                `json:"transferable"`
	ProvenanceOK       bool                `json:"provenance_ok"`
	Evidence           probeMemoryEvidence `json:"evidence"`
}

type probeMemoryEvidence struct {
	OrdinalStart *int     `json:"ordinal_start"`
	OrdinalEnd   *int     `json:"ordinal_end"`
	ToolUseIDs   []string `json:"tool_use_ids"`
	Snippets     []string `json:"snippets"`
}

func (db *DB) ImportAcceptedMemoriesJSONL(
	ctx context.Context, r io.Reader,
) (MemoryImportResult, error) {
	return db.ImportAcceptedMemoriesJSONLWithOptions(
		ctx, r, MemoryImportOptions{},
	)
}

func (db *DB) ImportAcceptedMemoriesJSONLWithOptions(
	ctx context.Context, r io.Reader, opts MemoryImportOptions,
) (MemoryImportResult, error) {
	var result MemoryImportResult
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	seen := make(map[string]struct{})
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item probeAcceptedMemory
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return result, fmt.Errorf(
				"importing memory line %d: invalid JSON: %w",
				lineNo, err,
			)
		}
		item = normalizeProbeAcceptedMemory(item)
		if reason := probeMemorySkipReason(item); reason != "" {
			result.Skipped++
			result.SkippedMemories = append(
				result.SkippedMemories,
				probeMemoryImportItem(item, reason),
			)
			continue
		}
		memory, err := probeMemoryToDB(item)
		if err != nil {
			return result, fmt.Errorf(
				"importing memory line %d: %w", lineNo, err,
			)
		}
		existing, err := db.GetMemory(ctx, memory.ID)
		if err != nil {
			return result, fmt.Errorf(
				"importing memory line %d: checking duplicate: %w",
				lineNo, err,
			)
		}
		if existing != nil {
			result.Skipped++
			result.SkippedMemories = append(
				result.SkippedMemories,
				probeMemoryImportItem(item, "duplicate"),
			)
			continue
		}
		if _, dup := seen[memory.ID]; dup {
			// A duplicate candidate_id earlier in this stream: a real
			// import inserts the first and skips the rest, so the dry-run
			// must skip it too instead of double-counting WouldImport.
			result.Skipped++
			result.SkippedMemories = append(
				result.SkippedMemories,
				probeMemoryImportItem(item, "duplicate"),
			)
			continue
		}
		if strings.TrimSpace(memory.SupersedesMemoryID) != "" {
			superseded, err := db.GetMemory(ctx, memory.SupersedesMemoryID)
			if err != nil {
				return result, fmt.Errorf(
					"importing memory line %d: checking superseded memory: %w",
					lineNo, err,
				)
			}
			if superseded == nil {
				return result, fmt.Errorf(
					"importing memory line %d: superseded memory %s not found",
					lineNo, memory.SupersedesMemoryID,
				)
			}
		}
		if opts.RequireExistingSessions {
			if err := db.requireMemoryImportSession(ctx, item); err != nil {
				return result, fmt.Errorf(
					"importing memory line %d: %w", lineNo, err,
				)
			}
			if err := db.requireMemoryImportEvidence(ctx, memory); err != nil {
				return result, fmt.Errorf(
					"importing memory line %d: %w", lineNo, err,
				)
			}
		}
		seen[memory.ID] = struct{}{}
		if opts.DryRun {
			result.WouldImport++
			result.WouldImportMemories = append(
				result.WouldImportMemories,
				probeMemoryImportItem(item, ""),
			)
			continue
		}
		if err := db.ensureMemoryImportSession(ctx, item); err != nil {
			return result, fmt.Errorf(
				"importing memory line %d: preparing source session: %w",
				lineNo, err,
			)
		}
		if memory.SupersedesMemoryID != "" {
			_, err = db.SupersedeMemory(ctx, memory.SupersedesMemoryID, memory)
		} else {
			_, err = db.InsertMemory(memory)
		}
		if err != nil {
			return result, fmt.Errorf(
				"importing memory line %d: %w", lineNo, err,
			)
		}
		result.Imported++
		result.ImportedMemories = append(
			result.ImportedMemories,
			probeMemoryImportItem(item, ""),
		)
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("reading memories JSONL: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func normalizeProbeAcceptedMemory(m probeAcceptedMemory) probeAcceptedMemory {
	m.CandidateID = strings.TrimSpace(m.CandidateID)
	m.RunID = strings.TrimSpace(m.RunID)
	m.SessionID = strings.TrimSpace(m.SessionID)
	m.EpisodeID = strings.TrimSpace(m.EpisodeID)
	m.Type = strings.TrimSpace(m.Type)
	m.Scope = strings.TrimSpace(m.Scope)
	m.Title = strings.TrimSpace(m.Title)
	m.Body = strings.TrimSpace(m.Body)
	m.Trigger = strings.TrimSpace(m.Trigger)
	m.Uncertainty = strings.TrimSpace(m.Uncertainty)
	m.Project = strings.TrimSpace(m.Project)
	m.CWD = strings.TrimSpace(m.CWD)
	m.GitBranch = strings.TrimSpace(m.GitBranch)
	m.Agent = strings.TrimSpace(m.Agent)
	m.ExtractorMethod = strings.TrimSpace(m.ExtractorMethod)
	m.Model = strings.TrimSpace(m.Model)
	m.Label = strings.TrimSpace(m.Label)
	m.SupersedesMemoryID = strings.TrimSpace(m.SupersedesMemoryID)
	m.Evidence.ToolUseIDs = normalizeProbeToolUseIDs(m.Evidence.ToolUseIDs)
	return m
}

func normalizeProbeToolUseIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (db *DB) requireMemoryImportSession(
	ctx context.Context, m probeAcceptedMemory,
) error {
	existing, err := db.GetSession(ctx, m.SessionID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("source session %s not found", m.SessionID)
	}
	return nil
}

func (db *DB) requireMemoryImportEvidence(
	ctx context.Context, memory Memory,
) error {
	if len(memory.Evidence) == 0 {
		return fmt.Errorf("missing memory evidence")
	}
	rangesChecked := map[string]struct{}{}
	toolUsesChecked := map[string]struct{}{}
	for _, evidence := range memory.Evidence {
		rangeKey := fmt.Sprintf(
			"%s:%d-%d",
			evidence.SessionID,
			evidence.MessageStartOrdinal,
			evidence.MessageEndOrdinal,
		)
		if _, ok := rangesChecked[rangeKey]; !ok {
			if err := db.requireMemoryEvidenceRange(ctx, evidence); err != nil {
				return err
			}
			rangesChecked[rangeKey] = struct{}{}
		}
		toolUseID := strings.TrimSpace(evidence.ToolUseID)
		if toolUseID == "" {
			continue
		}
		toolKey := rangeKey + ":" + toolUseID
		if _, ok := toolUsesChecked[toolKey]; ok {
			continue
		}
		if err := db.requireMemoryEvidenceToolUse(ctx, evidence); err != nil {
			return err
		}
		toolUsesChecked[toolKey] = struct{}{}
	}
	return nil
}

func (db *DB) requireMemoryEvidenceRange(
	ctx context.Context, evidence MemoryEvidence,
) error {
	want := evidence.MessageEndOrdinal - evidence.MessageStartOrdinal + 1
	var got int
	err := db.getReader().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM messages
		WHERE session_id = ?
		  AND ordinal BETWEEN ? AND ?`,
		evidence.SessionID,
		evidence.MessageStartOrdinal,
		evidence.MessageEndOrdinal,
	).Scan(&got)
	if err != nil {
		return fmt.Errorf("checking source evidence: %w", err)
	}
	if got != want {
		return fmt.Errorf(
			"source evidence %s:%d-%d not found",
			evidence.SessionID,
			evidence.MessageStartOrdinal,
			evidence.MessageEndOrdinal,
		)
	}
	return nil
}

func (db *DB) requireMemoryEvidenceToolUse(
	ctx context.Context, evidence MemoryEvidence,
) error {
	var got int
	err := db.getReader().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ?
		  AND tc.tool_use_id = ?
		  AND m.ordinal BETWEEN ? AND ?`,
		evidence.SessionID,
		evidence.ToolUseID,
		evidence.MessageStartOrdinal,
		evidence.MessageEndOrdinal,
	).Scan(&got)
	if err != nil {
		return fmt.Errorf("checking source tool use: %w", err)
	}
	if got == 0 {
		return fmt.Errorf(
			"source tool use %s not found in %s:%d-%d",
			evidence.ToolUseID,
			evidence.SessionID,
			evidence.MessageStartOrdinal,
			evidence.MessageEndOrdinal,
		)
	}
	return nil
}

func (db *DB) ensureMemoryImportSession(
	ctx context.Context, m probeAcceptedMemory,
) error {
	// Insert the placeholder only when the session is absent. A plain upsert
	// would clobber a real session synced in the window between a presence
	// check and the write with placeholder metadata (message_count=0, etc.).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	firstMessage := "Memory import placeholder for " + m.SessionID
	displayName := firstMessage
	return db.insertSessionIfAbsent(ctx, Session{
		ID:                m.SessionID,
		Project:           m.Project,
		Machine:           "memory-import",
		Agent:             m.Agent,
		FirstMessage:      &firstMessage,
		DisplayName:       &displayName,
		StartedAt:         &now,
		EndedAt:           &now,
		MessageCount:      0,
		UserMessageCount:  0,
		Outcome:           "unknown",
		OutcomeConfidence: "low",
		Cwd:               m.CWD,
		GitBranch:         m.GitBranch,
		SourceVersion:     "memory-import-placeholder",
	})
}

func probeMemorySkipReason(m probeAcceptedMemory) string {
	if !m.Transferable {
		return "not_transferable"
	}
	if !m.ProvenanceOK {
		return "provenance_not_ok"
	}
	switch m.Label {
	case "correct", "useful-but-uncertain", "genuine-tradeoff":
		return ""
	default:
		return "label_not_keeper"
	}
}

func probeMemoryImportItem(m probeAcceptedMemory, reason string) MemoryImportItem {
	return MemoryImportItem{
		CandidateID:        m.CandidateID,
		Title:              m.Title,
		SourceSessionID:    m.SessionID,
		SupersedesMemoryID: m.SupersedesMemoryID,
		Label:              m.Label,
		Reason:             reason,
	}
}

func probeMemoryToDB(m probeAcceptedMemory) (Memory, error) {
	if m.CandidateID == "" {
		return Memory{}, fmt.Errorf("missing candidate_id")
	}
	if m.SessionID == "" {
		return Memory{}, fmt.Errorf("missing session_id")
	}
	if err := validateImportedMemoryIdentities(m); err != nil {
		return Memory{}, err
	}
	if m.Title == "" || m.Body == "" {
		return Memory{}, fmt.Errorf("missing title or body")
	}
	if !validImportedMemoryType(m.Type) {
		return Memory{}, fmt.Errorf("invalid memory type %q", m.Type)
	}
	if !validImportedMemoryScope(m.Scope) {
		return Memory{}, fmt.Errorf("invalid memory scope %q", m.Scope)
	}
	if !validImportedConfidence(m.Confidence) {
		return Memory{}, fmt.Errorf(
			"invalid confidence %g; must be between 0 and 1",
			*m.Confidence,
		)
	}
	if m.Evidence.OrdinalStart == nil || m.Evidence.OrdinalEnd == nil {
		return Memory{}, fmt.Errorf("missing evidence ordinal range")
	}
	if *m.Evidence.OrdinalStart < 0 ||
		*m.Evidence.OrdinalEnd < *m.Evidence.OrdinalStart {
		return Memory{}, fmt.Errorf("invalid evidence ordinal range")
	}
	evidence := probeEvidenceToDB(m.CandidateID, m.SessionID, m.Evidence)
	return Memory{
		ID:                 m.CandidateID,
		Type:               m.Type,
		Scope:              m.Scope,
		Status:             "accepted",
		Title:              m.Title,
		Body:               m.Body,
		Trigger:            m.Trigger,
		Confidence:         m.Confidence,
		Uncertainty:        m.Uncertainty,
		Project:            m.Project,
		CWD:                m.CWD,
		GitBranch:          m.GitBranch,
		Agent:              m.Agent,
		SourceSessionID:    m.SessionID,
		SourceEpisodeID:    m.EpisodeID,
		SourceRunID:        m.RunID,
		ExtractorMethod:    m.ExtractorMethod,
		Model:              m.Model,
		SupersedesMemoryID: strings.TrimSpace(m.SupersedesMemoryID),
		Transferable:       true,
		ProvenanceOK:       true,
		Evidence:           evidence,
	}, nil
}

func validateImportedMemoryIdentities(m probeAcceptedMemory) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "candidate_id", value: m.CandidateID},
		{name: "session_id", value: m.SessionID},
		{name: "run_id", value: m.RunID},
		{name: "episode_id", value: m.EpisodeID},
		{name: "supersedes_memory_id", value: m.SupersedesMemoryID},
	} {
		if containsImportedControlCharacter(field.value) {
			return fmt.Errorf(
				"%s must not contain control characters",
				field.name,
			)
		}
	}
	if slices.ContainsFunc(m.Evidence.ToolUseIDs, containsImportedControlCharacter) {
		return fmt.Errorf(
			"tool_use_id must not contain control characters",
		)
	}
	return nil
}

func containsImportedControlCharacter(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validImportedMemoryType(value string) bool {
	switch value {
	case corememory.TypeFact,
		corememory.TypeDecision,
		corememory.TypeProcedure,
		corememory.TypeDebuggingMethod,
		corememory.TypeWarning,
		corememory.TypePreference,
		corememory.TypeOpenQuestion:
		return true
	default:
		return false
	}
}

func validImportedMemoryScope(value string) bool {
	switch value {
	case corememory.ScopeGlobal,
		corememory.ScopeProject,
		corememory.ScopeRepository,
		corememory.ScopeBranch,
		corememory.ScopeFile,
		corememory.ScopeTool,
		corememory.ScopeAgent:
		return true
	default:
		return false
	}
}

func validImportedConfidence(value *float64) bool {
	if value == nil {
		return true
	}
	return *value >= 0 && *value <= 1
}

func probeEvidenceToDB(
	memoryID, sessionID string, e probeMemoryEvidence,
) []MemoryEvidence {
	if len(e.ToolUseIDs) == 0 {
		return []MemoryEvidence{
			probeEvidenceItem(memoryID, sessionID, e, ""),
		}
	}
	items := make([]MemoryEvidence, 0, len(e.ToolUseIDs))
	for _, toolUseID := range e.ToolUseIDs {
		items = append(items, probeEvidenceItem(
			memoryID, sessionID, e, toolUseID,
		))
	}
	return items
}

func probeEvidenceItem(
	memoryID, sessionID string, e probeMemoryEvidence, toolUseID string,
) MemoryEvidence {
	return MemoryEvidence{
		MemoryID:            memoryID,
		SessionID:           sessionID,
		MessageStartOrdinal: *e.OrdinalStart,
		MessageEndOrdinal:   *e.OrdinalEnd,
		ToolUseID:           toolUseID,
		Snippet:             strings.Join(e.Snippets, "\n"),
	}
}
