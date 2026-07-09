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

	corerecall "go.kenn.io/agentsview/internal/recall"
)

type RecallImportResult struct {
	Imported           int                `json:"imported"`
	Skipped            int                `json:"skipped"`
	WouldImport        int                `json:"would_import,omitempty"`
	ImportedEntries    []RecallImportItem `json:"imported_entries,omitempty"`
	WouldImportEntries []RecallImportItem `json:"would_import_entries,omitempty"`
	SkippedEntries     []RecallImportItem `json:"skipped_entries,omitempty"`
}

type RecallImportItem struct {
	CandidateID       string `json:"candidate_id"`
	Title             string `json:"title,omitempty"`
	SourceSessionID   string `json:"source_session_id,omitempty"`
	SupersedesEntryID string `json:"supersedes_entry_id,omitempty"`
	Label             string `json:"label,omitempty"`
	Reason            string `json:"reason,omitempty"`
}

type RecallImportOptions struct {
	DryRun                  bool
	RequireExistingSessions bool
	AllowProductionImport   bool
}

type probeAcceptedRecallEntry struct {
	CandidateID       string              `json:"candidate_id"`
	RunID             string              `json:"run_id"`
	SessionID         string              `json:"session_id"`
	EpisodeID         string              `json:"episode_id"`
	Type              string              `json:"type"`
	Scope             string              `json:"scope"`
	Title             string              `json:"title"`
	Body              string              `json:"body"`
	Trigger           string              `json:"trigger"`
	Confidence        *float64            `json:"confidence"`
	Uncertainty       string              `json:"uncertainty"`
	Project           string              `json:"project"`
	CWD               string              `json:"cwd"`
	GitBranch         string              `json:"git_branch"`
	Agent             string              `json:"agent"`
	ExtractorMethod   string              `json:"extractor_method"`
	Model             string              `json:"model"`
	Label             string              `json:"label"`
	SupersedesEntryID string              `json:"supersedes_entry_id"`
	Transferable      bool                `json:"transferable"`
	ProvenanceOK      bool                `json:"provenance_ok"`
	Evidence          probeRecallEvidence `json:"evidence"`
}

type probeRecallEvidence struct {
	OrdinalStart *int     `json:"ordinal_start"`
	OrdinalEnd   *int     `json:"ordinal_end"`
	ToolUseIDs   []string `json:"tool_use_ids"`
	Snippets     []string `json:"snippets"`
}

func (db *DB) ImportAcceptedRecallEntriesJSONL(
	ctx context.Context, r io.Reader,
) (RecallImportResult, error) {
	return db.ImportAcceptedRecallEntriesJSONLWithOptions(
		ctx, r, RecallImportOptions{},
	)
}

func (db *DB) ImportAcceptedRecallEntriesJSONLWithOptions(
	ctx context.Context, r io.Reader, opts RecallImportOptions,
) (RecallImportResult, error) {
	var result RecallImportResult
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: invalid JSON: %w",
				lineNo, err,
			)
		}
		if _, ok := fields["review_state"]; ok {
			return result, fmt.Errorf(
				"importing recall line %d: review_state is host-controlled",
				lineNo,
			)
		}
		var item probeAcceptedRecallEntry
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: invalid JSON: %w",
				lineNo, err,
			)
		}
		item = normalizeProbeAcceptedRecallEntry(item)
		if reason := probeRecallEntrySkipReason(item); reason != "" {
			result.Skipped++
			result.SkippedEntries = append(
				result.SkippedEntries,
				probeRecallImportItem(item, reason),
			)
			continue
		}
		recall, err := probeRecallEntryToDB(item)
		if err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: %w", lineNo, err,
			)
		}
		existing, err := db.GetRecallEntry(ctx, recall.ID)
		if err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: checking duplicate: %w",
				lineNo, err,
			)
		}
		if existing != nil {
			result.Skipped++
			result.SkippedEntries = append(
				result.SkippedEntries,
				probeRecallImportItem(item, "duplicate"),
			)
			continue
		}
		if _, dup := seen[recall.ID]; dup {
			// A duplicate candidate_id earlier in this stream: a real
			// import inserts the first and skips the rest, so the dry-run
			// must skip it too instead of double-counting WouldImport.
			result.Skipped++
			result.SkippedEntries = append(
				result.SkippedEntries,
				probeRecallImportItem(item, "duplicate"),
			)
			continue
		}
		if strings.TrimSpace(recall.SupersedesEntryID) != "" {
			superseded, err := db.GetRecallEntry(ctx, recall.SupersedesEntryID)
			if err != nil {
				return result, fmt.Errorf(
					"importing recall line %d: checking superseded entry: %w",
					lineNo, err,
				)
			}
			if superseded == nil {
				return result, fmt.Errorf(
					"importing recall line %d: superseded entry %s not found",
					lineNo, recall.SupersedesEntryID,
				)
			}
		}
		if opts.RequireExistingSessions {
			if err := db.requireRecallImportSession(ctx, item); err != nil {
				return result, fmt.Errorf(
					"importing recall line %d: %w", lineNo, err,
				)
			}
			if err := db.requireRecallImportEvidence(ctx, recall); err != nil {
				return result, fmt.Errorf(
					"importing recall line %d: %w", lineNo, err,
				)
			}
		} else {
			recall.ProvenanceOK = false
		}
		seen[recall.ID] = struct{}{}
		if opts.DryRun {
			result.WouldImport++
			result.WouldImportEntries = append(
				result.WouldImportEntries,
				probeRecallImportItem(item, ""),
			)
			continue
		}
		if err := db.ensureRecallImportSession(ctx, item); err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: preparing source session: %w",
				lineNo, err,
			)
		}
		if recall.SupersedesEntryID != "" {
			_, err = db.SupersedeRecallEntry(ctx, recall.SupersedesEntryID, recall)
		} else {
			_, err = db.InsertRecallEntry(recall)
		}
		if err != nil {
			return result, fmt.Errorf(
				"importing recall line %d: %w", lineNo, err,
			)
		}
		result.Imported++
		result.ImportedEntries = append(
			result.ImportedEntries,
			probeRecallImportItem(item, ""),
		)
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("reading entries JSONL: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func normalizeProbeAcceptedRecallEntry(m probeAcceptedRecallEntry) probeAcceptedRecallEntry {
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
	m.SupersedesEntryID = strings.TrimSpace(m.SupersedesEntryID)
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

func (db *DB) requireRecallImportSession(
	ctx context.Context, m probeAcceptedRecallEntry,
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

func (db *DB) requireRecallImportEvidence(
	ctx context.Context, recall RecallEntry,
) error {
	if len(recall.Evidence) == 0 {
		return fmt.Errorf("missing recall evidence")
	}
	rangesChecked := map[string]struct{}{}
	toolUsesChecked := map[string]struct{}{}
	for _, evidence := range recall.Evidence {
		rangeKey := fmt.Sprintf(
			"%s:%d-%d",
			evidence.SessionID,
			evidence.MessageStartOrdinal,
			evidence.MessageEndOrdinal,
		)
		if _, ok := rangesChecked[rangeKey]; !ok {
			if err := db.requireRecallEvidenceRange(ctx, evidence); err != nil {
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
		if err := db.requireRecallEvidenceToolUse(ctx, evidence); err != nil {
			return err
		}
		toolUsesChecked[toolKey] = struct{}{}
	}
	return nil
}

func (db *DB) requireRecallEvidenceRange(
	ctx context.Context, evidence RecallEvidence,
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

func (db *DB) requireRecallEvidenceToolUse(
	ctx context.Context, evidence RecallEvidence,
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

func (db *DB) ensureRecallImportSession(
	ctx context.Context, m probeAcceptedRecallEntry,
) error {
	// Insert the placeholder only when the session is absent. A plain upsert
	// would clobber a real session synced in the window between a presence
	// check and the write with placeholder metadata (message_count=0, etc.).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	firstMessage := "Recall import placeholder for " + m.SessionID
	displayName := firstMessage
	return db.insertSessionIfAbsent(ctx, Session{
		ID:                m.SessionID,
		Project:           m.Project,
		Machine:           "recall-import",
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
		SourceVersion:     "recall-import-placeholder",
	})
}

func probeRecallEntrySkipReason(m probeAcceptedRecallEntry) string {
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

func probeRecallImportItem(m probeAcceptedRecallEntry, reason string) RecallImportItem {
	return RecallImportItem{
		CandidateID:       m.CandidateID,
		Title:             m.Title,
		SourceSessionID:   m.SessionID,
		SupersedesEntryID: m.SupersedesEntryID,
		Label:             m.Label,
		Reason:            reason,
	}
}

func probeRecallEntryToDB(m probeAcceptedRecallEntry) (RecallEntry, error) {
	if m.CandidateID == "" {
		return RecallEntry{}, fmt.Errorf("missing candidate_id")
	}
	if m.SessionID == "" {
		return RecallEntry{}, fmt.Errorf("missing session_id")
	}
	if err := validateImportedRecallEntryIdentities(m); err != nil {
		return RecallEntry{}, err
	}
	if m.Title == "" || m.Body == "" {
		return RecallEntry{}, fmt.Errorf("missing title or body")
	}
	if !validImportedRecallEntryType(m.Type) {
		return RecallEntry{}, fmt.Errorf("invalid recall type %q", m.Type)
	}
	if !validImportedRecallEntryScope(m.Scope) {
		return RecallEntry{}, fmt.Errorf("invalid recall scope %q", m.Scope)
	}
	if !validImportedConfidence(m.Confidence) {
		return RecallEntry{}, fmt.Errorf(
			"invalid confidence %g; must be between 0 and 1",
			*m.Confidence,
		)
	}
	if m.Evidence.OrdinalStart == nil || m.Evidence.OrdinalEnd == nil {
		return RecallEntry{}, fmt.Errorf("missing evidence ordinal range")
	}
	if *m.Evidence.OrdinalStart < 0 ||
		*m.Evidence.OrdinalEnd < *m.Evidence.OrdinalStart {
		return RecallEntry{}, fmt.Errorf("invalid evidence ordinal range")
	}
	evidence := probeEvidenceToDB(m.CandidateID, m.SessionID, m.Evidence)
	return RecallEntry{
		ID:                m.CandidateID,
		Type:              m.Type,
		Scope:             m.Scope,
		Status:            "accepted",
		ReviewState:       corerecall.ReviewStateHumanReviewed,
		Title:             m.Title,
		Body:              m.Body,
		Trigger:           m.Trigger,
		Confidence:        m.Confidence,
		Uncertainty:       m.Uncertainty,
		Project:           m.Project,
		CWD:               m.CWD,
		GitBranch:         m.GitBranch,
		Agent:             m.Agent,
		SourceSessionID:   m.SessionID,
		SourceEpisodeID:   m.EpisodeID,
		SourceRunID:       m.RunID,
		ExtractorMethod:   m.ExtractorMethod,
		Model:             m.Model,
		SupersedesEntryID: strings.TrimSpace(m.SupersedesEntryID),
		Transferable:      true,
		ProvenanceOK:      true,
		Evidence:          evidence,
	}, nil
}

func validateImportedRecallEntryIdentities(m probeAcceptedRecallEntry) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "candidate_id", value: m.CandidateID},
		{name: "session_id", value: m.SessionID},
		{name: "run_id", value: m.RunID},
		{name: "episode_id", value: m.EpisodeID},
		{name: "supersedes_entry_id", value: m.SupersedesEntryID},
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

func validImportedRecallEntryType(value string) bool {
	switch value {
	case corerecall.TypeFact,
		corerecall.TypeDecision,
		corerecall.TypeProcedure,
		corerecall.TypeDebuggingMethod,
		corerecall.TypeWarning,
		corerecall.TypePreference,
		corerecall.TypeOpenQuestion:
		return true
	default:
		return false
	}
}

func validImportedRecallEntryScope(value string) bool {
	switch value {
	case corerecall.ScopeGlobal,
		corerecall.ScopeProject,
		corerecall.ScopeRepository,
		corerecall.ScopeBranch,
		corerecall.ScopeFile,
		corerecall.ScopeTool,
		corerecall.ScopeAgent:
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
	recallID, sessionID string, e probeRecallEvidence,
) []RecallEvidence {
	if len(e.ToolUseIDs) == 0 {
		return []RecallEvidence{
			probeEvidenceItem(recallID, sessionID, e, ""),
		}
	}
	items := make([]RecallEvidence, 0, len(e.ToolUseIDs))
	for _, toolUseID := range e.ToolUseIDs {
		items = append(items, probeEvidenceItem(
			recallID, sessionID, e, toolUseID,
		))
	}
	return items
}

func probeEvidenceItem(
	recallID, sessionID string, e probeRecallEvidence, toolUseID string,
) RecallEvidence {
	return RecallEvidence{
		EntryID:             recallID,
		SessionID:           sessionID,
		MessageStartOrdinal: *e.OrdinalStart,
		MessageEndOrdinal:   *e.OrdinalEnd,
		ToolUseID:           toolUseID,
		Snippet:             strings.Join(e.Snippets, "\n"),
	}
}
