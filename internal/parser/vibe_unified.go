package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// The unified harness session store (store format
// "mistral.vibe.unified-session-store/v1") replaced the legacy
// messages.jsonl + meta.json layout under <root>/unified/<session-id>/.
// A session directory holds:
//
//	CURRENT                      pointer to the newest generation
//	meta.json                    session metadata (absent for subagents)
//	journal/<seq>.jsonl          records appended since that generation
//	chunks/<sha256>.json         content-addressed entry chunks
//	generations/<seq>/           snapshots, pruned to the newest two
//	  manifest.json              lists the projection chunks and journal segment
//	  projection-state.json      session envelope with aggregate token usage
//	  runtime-state.json         runtime metadata (active model, cwd)
//
// The public transcript is the newest generation's projection, whose entries
// live in the chunks its manifest lists, advanced by the projection records
// in that generation's journal segment. The producer journals each durable
// projection change and folds the journal into a new generation only when
// the session settles, so a running turn exists only in the journal. Each
// chunk is a JSON array of harness public-session-state entries (message,
// reasoning, effect, notice, checkpoint).

const vibeUnifiedStoreFormat = "mistral.vibe.unified-session-store/v1"

var (
	vibeUnifiedSHA256         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	vibeUnifiedJournalSegment = regexp.MustCompile(`^journal/[0-9]{16}\.jsonl$`)
)

type vibeUnifiedCurrent struct {
	Generation       string `json:"generation"`
	StoreFormat      string `json:"store_format"`
	StoreFormatMinor int    `json:"store_format_minor"`
	ManifestSHA256   string `json:"manifest_sha256"`
}

type vibeUnifiedManifest struct {
	ProjectionState struct {
		Chunks []string `json:"chunks"`
	} `json:"projection_state"`
	RecoveryJournalSegment struct {
		Path          string `json:"path"`
		FirstSequence int64  `json:"first_sequence"`
	} `json:"recovery_journal_segment"`
}

// vibeUnifiedGeneration is the generation a session's CURRENT publishes.
type vibeUnifiedGeneration struct {
	sessionDir string
	dir        string
	manifest   vibeUnifiedManifest
}

type vibeUnifiedTokenUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
}

type vibeUnifiedProjectionSession struct {
	Title        string                `json:"title"`
	CreatedAt    int64                 `json:"createdAt"`
	UpdatedAt    int64                 `json:"updatedAt"`
	TokenUsage   vibeUnifiedTokenUsage `json:"tokenUsage"`
	ContextUsage vibeUnifiedTokenUsage `json:"contextUsage"`
}

type vibeUnifiedSnapshot struct {
	Session vibeUnifiedProjectionSession `json:"session"`
	History struct {
		Entries []vibeUnifiedEntry `json:"entries"`
	} `json:"history"`
}

type vibeUnifiedProjectionState struct {
	Snapshot vibeUnifiedSnapshot `json:"snapshot"`
}

// vibeUnifiedRuntimeMetadata carries the identity-bearing fields of
// runtime-state.json. The file also embeds plugin and skill manifests, so
// only these fields are decoded.
type vibeUnifiedRuntimeMetadata struct {
	Identity struct {
		Kind            string `json:"kind"`
		ParentSessionID string `json:"parent_session_id"`
	} `json:"identity"`
	SessionMetadata struct {
		ActiveModel string `json:"active_model"`
		Cwd         string `json:"cwd"`
	} `json:"session_metadata"`
	ImportProvenance *struct {
		Source struct {
			SessionID string `json:"session_id"`
		} `json:"source"`
	} `json:"import_provenance"`
}

type vibeUnifiedEntry struct {
	Type               string                  `json:"type"`
	Role               string                  `json:"role"`
	ID                 string                  `json:"id"`
	TurnID             string                  `json:"turnId"`
	Text               string                  `json:"text"`
	Summary            []string                `json:"summary,omitempty"`
	Content            jsontext.Value          `json:"content,omitempty"`
	UserDisplayContent jsontext.Value          `json:"userDisplayContent,omitempty"`
	CreatedAt          int64                   `json:"createdAt"`
	UpdatedAt          int64                   `json:"updatedAt"`
	Detail             *vibeUnifiedEffect      `json:"detail,omitempty"`
	State              *vibeUnifiedEffectState `json:"state,omitempty"`
}

type vibeUnifiedEffect struct {
	Kind           string         `json:"kind,omitempty"`
	ToolName       string         `json:"toolName,omitempty"`
	Input          jsontext.Value `json:"input,omitempty"`
	ChildSessionID string         `json:"childSessionId,omitempty"`
}

type vibeUnifiedEffectState struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Output     *vibeUnifiedEffectOutput `json:"output,omitempty"`
	OutputText string                   `json:"outputText,omitempty"`
}

type vibeUnifiedEffectOutput struct {
	Content           jsontext.Value `json:"content,omitempty"`
	StructuredContent jsontext.Value `json:"structured_content,omitempty"`
}

// vibeIsUnifiedAnchor reports whether path is the CURRENT anchor of a unified
// session directory (.../unified/<session-id>/CURRENT).
func vibeIsUnifiedAnchor(path string) bool {
	return filepath.Base(path) == "CURRENT" &&
		filepath.Base(filepath.Dir(filepath.Dir(path))) == "unified"
}

// VibeUnifiedSessionID returns the archive session ID for a unified anchor.
func VibeUnifiedSessionID(path string) (string, bool) {
	if !vibeIsUnifiedAnchor(path) {
		return "", false
	}
	return "vibe:" + filepath.Base(filepath.Dir(path)), true
}

// vibeUnifiedSessionDirFromRel maps CURRENT, meta.json, and journal segments
// to their session directory.
func vibeUnifiedSessionDirFromRel(rel, root string) (string, bool) {
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[0] != "unified" {
		return "", false
	}
	inner := strings.Join(parts[2:], "/")
	if inner != "CURRENT" && inner != "meta.json" && !vibeUnifiedJournalSegment.MatchString(inner) {
		return "", false
	}
	return filepath.Join(filepath.Clean(root), "unified", parts[1]), true
}

// loadVibeUnifiedGeneration resolves the generation CURRENT names and reads
// its manifest, which CURRENT pins by digest.
func loadVibeUnifiedGeneration(sessionDir string) (vibeUnifiedGeneration, error) {
	raw, err := os.ReadFile(filepath.Join(sessionDir, "CURRENT"))
	if err != nil {
		return vibeUnifiedGeneration{}, fmt.Errorf("reading Vibe unified CURRENT: %w", err)
	}
	current := vibeUnifiedCurrent{StoreFormat: vibeUnifiedStoreFormat, StoreFormatMinor: 1}
	if err := json.Unmarshal(raw, &current); err != nil {
		return vibeUnifiedGeneration{}, fmt.Errorf("parsing Vibe unified CURRENT: %w", err)
	}
	if current.StoreFormat != vibeUnifiedStoreFormat || current.StoreFormatMinor < 1 || current.StoreFormatMinor > 8 {
		return vibeUnifiedGeneration{}, fmt.Errorf("%w: store %s minor %d", errVibeUnsupportedVersion, current.StoreFormat, current.StoreFormatMinor)
	}
	if !isSafeSinglePathComponent(current.Generation) {
		return vibeUnifiedGeneration{}, errors.New("invalid generation in Vibe unified CURRENT")
	}
	gen := vibeUnifiedGeneration{
		sessionDir: sessionDir,
		dir:        filepath.Join(sessionDir, "generations", current.Generation),
	}
	raw, err = os.ReadFile(filepath.Join(gen.dir, "manifest.json"))
	if err != nil {
		return vibeUnifiedGeneration{}, fmt.Errorf("reading Vibe unified manifest.json: %w", err)
	}
	// Stored documents end in a newline that their digest excludes.
	digest := sha256.Sum256(bytes.TrimSuffix(raw, []byte("\n")))
	if hex.EncodeToString(digest[:]) != current.ManifestSHA256 {
		return vibeUnifiedGeneration{}, errors.New("manifest digest does not match Vibe unified CURRENT")
	}
	if err := decodeVibeUnifiedDoc("manifest.json", raw, &gen.manifest); err != nil {
		return vibeUnifiedGeneration{}, err
	}
	return gen, nil
}

// chunkPaths lists the projection chunks in transcript order. Chunk names are
// SHA-256 digests, so they cannot leave the chunk pool.
func (g vibeUnifiedGeneration) chunkPaths() ([]string, error) {
	paths := make([]string, 0, len(g.manifest.ProjectionState.Chunks))
	for _, chunk := range g.manifest.ProjectionState.Chunks {
		if !vibeUnifiedSHA256.MatchString(chunk) {
			return nil, fmt.Errorf("invalid Vibe unified chunk hash %q", chunk)
		}
		paths = append(paths, filepath.Join(g.sessionDir, "chunks", chunk+".json"))
	}
	return paths, nil
}

// journalPath returns the journal segment that extends the generation.
func (g vibeUnifiedGeneration) journalPath() (string, error) {
	path := g.manifest.RecoveryJournalSegment.Path
	if path == "" {
		return "", nil
	}
	if !vibeUnifiedJournalSegment.MatchString(path) {
		return "", fmt.Errorf("invalid Vibe unified journal segment %q", path)
	}
	return filepath.Join(g.sessionDir, filepath.FromSlash(path)), nil
}

// snapshot returns the generation's projection advanced by its journal.
func (g vibeUnifiedGeneration) snapshot() (vibeUnifiedSnapshot, error) {
	var projection vibeUnifiedProjectionState
	if err := readVibeUnifiedDoc(g.dir, "projection-state.json", &projection); err != nil {
		return vibeUnifiedSnapshot{}, err
	}
	snapshot := projection.Snapshot
	// A null chunk list keeps the transcript inline, as before chunk pooling.
	if g.manifest.ProjectionState.Chunks != nil {
		paths, err := g.chunkPaths()
		if err != nil {
			return vibeUnifiedSnapshot{}, err
		}
		snapshot.History.Entries = nil
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				return vibeUnifiedSnapshot{}, fmt.Errorf("reading Vibe unified chunk: %w", err)
			}
			var entries []vibeUnifiedEntry
			if err := json.Unmarshal(raw, &entries); err != nil {
				return vibeUnifiedSnapshot{}, fmt.Errorf("parsing Vibe unified chunk %s: %w", filepath.Base(path), err)
			}
			snapshot.History.Entries = append(snapshot.History.Entries, entries...)
		}
	}
	journal, err := g.journalPath()
	if err != nil || journal == "" {
		return snapshot, err
	}
	err = replayVibeUnifiedJournal(journal, g.manifest.RecoveryJournalSegment.FirstSequence, &snapshot)
	return snapshot, err
}

// VibeUnifiedJournalPath returns the journal segment that extends the
// generation a unified anchor publishes.
func VibeUnifiedJournalPath(anchor string) (string, bool) {
	if !vibeIsUnifiedAnchor(anchor) {
		return "", false
	}
	gen, err := loadVibeUnifiedGeneration(filepath.Dir(anchor))
	if err != nil {
		return "", false
	}
	path, err := gen.journalPath()
	return path, err == nil && path != ""
}

type vibeUnifiedJournalRecord struct {
	Version  int            `json:"recovery_journal_record_version"`
	Sequence int64          `json:"sequence"`
	Type     string         `json:"type"`
	Previous *string        `json:"previous_record_sha256"`
	Digest   string         `json:"record_sha256"`
	Payload  jsontext.Value `json:"payload"`
}

type vibeUnifiedProjectionOp struct {
	Op      string              `json:"op"`
	ID      string              `json:"id"`
	Entry   vibeUnifiedEntry    `json:"entry"`
	Entries []vibeUnifiedEntry  `json:"entries"`
	State   vibeUnifiedSnapshot `json:"state"`
}

var vibeUnifiedRecordDigestField = []byte(`,"record_sha256":"`)

// replayVibeUnifiedJournal applies a journal segment's projection records to
// snapshot. Records chain by digest from the segment's first sequence, and a
// final record without its newline is an append still in progress.
func replayVibeUnifiedJournal(path string, sequence int64, snapshot *vibeUnifiedSnapshot) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading Vibe unified journal: %w", err)
	}
	previous := ""
	for {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			return nil
		}
		line := data[:end]
		data = data[end+1:]
		var record vibeUnifiedJournalRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("parsing Vibe unified journal record %d: %w", sequence, err)
		}
		if record.Version != 1 {
			return fmt.Errorf("%w: journal record version %d", errVibeUnsupportedVersion, record.Version)
		}
		if record.Sequence != sequence || (record.Previous == nil) != (previous == "") ||
			(record.Previous != nil && *record.Previous != previous) ||
			vibeUnifiedRecordDigest(line) != record.Digest {
			return fmt.Errorf("vibe unified journal record %d breaks the digest chain", sequence)
		}
		if err := applyVibeUnifiedJournalRecord(record, snapshot); err != nil {
			return fmt.Errorf("applying Vibe unified journal record %d: %w", sequence, err)
		}
		previous = record.Digest
		sequence++
	}
}

// vibeUnifiedRecordDigest recomputes a journal record's digest, which covers
// the record without its own record_sha256 member.
func vibeUnifiedRecordDigest(line []byte) string {
	start := bytes.LastIndex(line, vibeUnifiedRecordDigestField)
	end := start + len(vibeUnifiedRecordDigestField) + 2*sha256.Size
	if start < 0 || end >= len(line) || line[end] != '"' {
		return ""
	}
	hash := sha256.New()
	_, _ = hash.Write(line[:start])
	_, _ = hash.Write(line[end+1:])
	return hex.EncodeToString(hash.Sum(nil))
}

// applyVibeUnifiedJournalRecord folds one projection record into snapshot.
// Records for runtime state do not touch the transcript.
func applyVibeUnifiedJournalRecord(record vibeUnifiedJournalRecord, snapshot *vibeUnifiedSnapshot) error {
	switch record.Type {
	case "projection_advanced":
		var payload struct {
			Snapshot vibeUnifiedSnapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return err
		}
		*snapshot = payload.Snapshot
	case "projection_delta":
		var payload struct {
			Delta []vibeUnifiedProjectionOp `json:"delta"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return err
		}
		for _, op := range payload.Delta {
			if err := op.apply(snapshot); err != nil {
				return err
			}
		}
	}
	return nil
}

func (op vibeUnifiedProjectionOp) apply(snapshot *vibeUnifiedSnapshot) error {
	entries := snapshot.History.Entries
	matches := func(entry vibeUnifiedEntry) bool { return entry.ID == op.ID }
	switch op.Op {
	case "append_entry":
		entries = append(entries, op.Entry)
	case "replace_entry":
		i := slices.IndexFunc(entries, matches)
		if i < 0 {
			return fmt.Errorf("replace_entry names absent entry %q", op.ID)
		}
		entries[i] = op.Entry
	case "remove_entry":
		count := len(entries)
		if entries = slices.DeleteFunc(entries, matches); len(entries) == count {
			return fmt.Errorf("remove_entry names absent entry %q", op.ID)
		}
	case "set_history_entries":
		entries = op.Entries
	case "set_envelope":
		snapshot.Session = op.State.Session
	default:
		return fmt.Errorf("%w: projection op %q", errVibeUnsupportedVersion, op.Op)
	}
	snapshot.History.Entries = entries
	return nil
}

// parseVibeUnifiedResultFile parses a unified harness session directory,
// anchored on its CURRENT pointer, into a ParseResult.
func parseVibeUnifiedResultFile(anchorPath string, fileInfo FileInfo) (ParseResult, error) {
	sessionDir := filepath.Dir(anchorPath)
	dirName := filepath.Base(sessionDir)

	result := ParseResult{
		Session: ParsedSession{
			Agent:           AgentVibe,
			File:            fileInfo,
			ID:              "vibe:" + dirName,
			SourceSessionID: dirName,
			Project:         "vibe",
			EndedAt:         time.Unix(0, fileInfo.Mtime),
		},
	}

	gen, err := loadVibeUnifiedGeneration(sessionDir)
	if err != nil {
		return result, err
	}
	snapshot, err := gen.snapshot()
	if err != nil {
		return result, err
	}
	projSession := snapshot.Session

	_, _, _, err = applyVibeMetadata(&result, sessionDir)
	if err != nil {
		return result, err
	}
	result.Session.ID = "vibe:" + dirName
	result.Session.SourceSessionID = dirName
	var parentID string

	var runtimeMeta vibeUnifiedRuntimeMetadata
	if err := readVibeUnifiedDoc(gen.dir, "runtime-state.json", &runtimeMeta); err != nil {
		return result, err
	}
	if result.Session.Cwd == "" {
		result.Session.Cwd = runtimeMeta.SessionMetadata.Cwd
		if project := ExtractProjectFromCwdWithBranch(result.Session.Cwd, result.Session.GitBranch); project != "" {
			result.Session.Project = project
		}
	}
	runtimeKind := runtimeMeta.Identity.Kind
	if runtimeKind == "subagent" || runtimeKind == "fork" {
		parentID = runtimeMeta.Identity.ParentSessionID
	}

	if runtimeMeta.ImportProvenance != nil {
		parentID = runtimeMeta.ImportProvenance.Source.SessionID
	}

	if result.Session.SessionName == "" {
		result.Session.SessionName = projSession.Title
	}
	if result.Session.StartedAt.IsZero() {
		result.Session.StartedAt = time.Unix(0, fileInfo.Mtime)
		if projSession.CreatedAt > 0 {
			result.Session.StartedAt = time.UnixMilli(projSession.CreatedAt)
		}
	}
	if projSession.UpdatedAt > 0 {
		result.Session.EndedAt = time.UnixMilli(projSession.UpdatedAt)
	}

	if parentID != "" {
		result.Session.ParentSessionID = "vibe:" + parentID
		switch {
		case runtimeKind == "subagent":
			result.Session.RelationshipType = RelSubagent
		case runtimeKind == "fork":
			result.Session.RelationshipType = RelFork
		case runtimeMeta.ImportProvenance != nil:
			result.Session.RelationshipType = RelContinuation
		}
	}

	if projSession.TokenUsage.OutputTokens > 0 {
		result.Session.HasTotalOutputTokens = true
		result.Session.TotalOutputTokens = projSession.TokenUsage.OutputTokens
	}
	if contextTokens := projSession.ContextUsage.InputTokens + projSession.ContextUsage.OutputTokens; contextTokens > 0 {
		result.Session.HasPeakContextTokens = true
		result.Session.PeakContextTokens = contextTokens
	}
	sessionModel := runtimeMeta.SessionMetadata.ActiveModel
	if sessionModel == "" {
		sessionModel, err = vibeUnifiedParentModel(sessionDir, runtimeMeta.Identity.ParentSessionID)
		if err != nil {
			return result, err
		}
	}
	result.Messages = vibeUnifiedMessages(snapshot.History.Entries, sessionModel)
	setVibeMessageMetadata(&result)

	stats := VibeStats{
		SessionPromptTokens:     projSession.TokenUsage.InputTokens,
		SessionCompletionTokens: projSession.TokenUsage.OutputTokens,
		SessionCachedTokens:     projSession.TokenUsage.CachedInputTokens,
	}
	result.UsageEvents = vibeUsageEvents(
		stats, sessionModel, result.Session.ID,
		result.Session.StartedAt, result.Session.EndedAt,
	)

	return result, nil
}

// vibeUnifiedMessages converts public history entries into messages.
func vibeUnifiedMessages(entries []vibeUnifiedEntry, model string) []ParsedMessage {
	var messages []ParsedMessage
	var thinking, thinkingTurn string
	var thinkingCreatedAt int64
	flushThinking := func() {
		if thinking != "" {
			msg := ParsedMessage{
				Ordinal: len(messages), Role: RoleAssistant, Model: model,
				ThinkingText: thinking, HasThinking: true,
			}
			if thinkingCreatedAt > 0 {
				msg.Timestamp = time.UnixMilli(thinkingCreatedAt)
			}
			messages = append(messages, msg)
			thinking = ""
		}
	}
	for _, entry := range entries {
		if entry.Type == "notice" || entry.Type == "checkpoint" {
			continue
		}
		if thinking != "" && entry.TurnID != thinkingTurn {
			flushThinking()
		}
		switch entry.Type {
		case "message":
			msg := vibeUnifiedEntryMessage(entry)
			msg.Ordinal = len(messages)
			if msg.Role == RoleAssistant {
				msg.Model = model
				msg.ThinkingText = thinking
				msg.HasThinking = thinking != ""
				thinking = ""
			}
			messages = append(messages, msg)
		case "reasoning":
			thinkingTurn = entry.TurnID
			thinkingCreatedAt = entry.CreatedAt
			if entry.Text != "" {
				thinking += entry.Text
			} else {
				thinking += strings.Join(entry.Summary, "")
			}
		case "effect":
			call := vibeUnifiedEffectMessages(entry, len(messages))
			if call != nil {
				call.Model = model
				call.ThinkingText = thinking
				call.HasThinking = call.ThinkingText != ""
				thinking = ""
				messages = append(messages, *call)
			}
		}
	}
	flushThinking()
	return messages
}

// vibeUnifiedEntryMessage converts a public message entry.
func vibeUnifiedEntryMessage(entry vibeUnifiedEntry) ParsedMessage {
	content := gjson.Parse(string(entry.Content))
	if entry.Role == "user" {
		display := gjson.Parse(string(entry.UserDisplayContent))
		if !display.IsObject() {
			for _, block := range content.Array() {
				if meta := block.Get(`_meta.vibe\.userDisplayContent`); meta.IsObject() {
					display = meta
					break
				}
			}
		}
		if literal := display.Get("content"); literal.IsArray() {
			content = literal
		}
	}
	text := vibeUnifiedContentText(content, "")
	msg := ParsedMessage{
		Role:          RoleType(entry.Role),
		Content:       text,
		ContentLength: len(text),
		SourceUUID:    entry.ID,
	}
	if entry.CreatedAt > 0 {
		msg.Timestamp = time.UnixMilli(entry.CreatedAt)
	}
	if entry.Role != "user" && entry.Role != "assistant" {
		msg.IsSystem = true
	}
	return msg
}

func vibeUnifiedContentText(content gjson.Result, separator string) string {
	if content.IsArray() {
		var parts []string
		for _, block := range content.Array() {
			switch block.Get("type").Str {
			case "image":
				parts = append(parts, "[image]")
			case "audio":
				parts = append(parts, "[audio]")
			case "resource", "resource_link":
				parts = append(parts, firstNonEmptyJSONLString(block.Get("resource.text").Str, "[resource]"))
			default:
				parts = append(parts, firstNonEmptyJSONLString(block.Get("text").Str, block.Get("result").Str))
			}
		}
		return strings.Join(parts, separator)
	}
	return decodeContent(content)
}

// Execution events carry output for running and terminal effects.
func vibeUnifiedEffectMessages(entry vibeUnifiedEntry, ordinal int) *ParsedMessage {
	if entry.ID == "" || entry.Detail == nil {
		return nil
	}
	toolName := entry.Detail.ToolName
	if toolName == "" {
		toolName = entry.Detail.Kind
	}
	if toolName == "" {
		return nil
	}
	inputJSON := string(entry.Detail.Input)
	if inputJSON == "" || strings.TrimSpace(inputJSON) == "null" {
		inputJSON = "{}"
	}

	call := &ParsedMessage{
		Ordinal:    ordinal,
		Role:       RoleAssistant,
		HasToolUse: true,
		SourceUUID: entry.ID,
		ToolCalls: []ParsedToolCall{{
			ToolUseID: entry.ID,
			ToolName:  toolName,
			Category:  vibeToolCategory(toolName),
			InputJSON: inputJSON,
		}},
	}
	if entry.CreatedAt > 0 {
		call.Timestamp = time.UnixMilli(entry.CreatedAt)
	}
	if toolName == "subagent.spawn" {
		if entry.Detail.ChildSessionID != "" {
			call.ToolCalls[0].SubagentSessionID = "vibe:" + entry.Detail.ChildSessionID
		}
	}

	resultText := ""
	if entry.State != nil {
		if output := entry.State.Output; output != nil {
			resultText = firstNonEmptyJSONLString(
				vibeUnifiedToolResultContent(gjson.Parse(string(output.Content))),
				vibeUnifiedStructuredResult(output.StructuredContent),
			)
		}
		resultText = firstNonEmptyJSONLString(resultText, entry.State.OutputText, entry.State.Reason, entry.State.Error.Message)
		status := entry.State.Status
		switch status {
		case "failed", "skipped":
			status = "errored"
		}
		if status == "running" || status == "completed" || status == "errored" || status == "cancelled" {
			event := ParsedToolResultEvent{ToolUseID: entry.ID, Source: "tool_execution", Status: status, Content: resultText}
			if entry.UpdatedAt > 0 {
				event.Timestamp = time.UnixMilli(entry.UpdatedAt)
			}
			call.ToolCalls[0].ResultEvents = []ParsedToolResultEvent{
				{ToolUseID: entry.ID, Source: "tool_execution", Status: "started", Timestamp: call.Timestamp},
				event,
			}
		}
	}
	return call
}

var errVibeUnsupportedVersion = errors.New("unsupported Vibe unified version")

func readVibeUnifiedDoc(genDir, name string, v any) error {
	raw, err := os.ReadFile(filepath.Join(genDir, name))
	if err != nil {
		return fmt.Errorf("reading Vibe unified %s: %w", name, err)
	}
	return decodeVibeUnifiedDoc(name, raw, v)
}

func decodeVibeUnifiedDoc(name string, raw []byte, v any) error {
	versions := struct {
		Manifest   int `json:"manifest_version"`
		Projection int `json:"projection_state_version"`
		Runtime    int `json:"runtime_state_version"`
	}{Manifest: 1, Projection: 1}
	if err := json.Unmarshal(raw, &versions); err != nil {
		return fmt.Errorf("parsing Vibe unified %s: %w", name, err)
	}
	if (name == "manifest.json" && versions.Manifest != 1) || (name == "projection-state.json" && versions.Projection != 1) || (name == "runtime-state.json" && versions.Runtime != 3) {
		return fmt.Errorf("%w: %s", errVibeUnsupportedVersion, name)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parsing Vibe unified %s: %w", name, err)
	}
	return nil
}

func vibeUnifiedToolResultContent(content gjson.Result) string {
	hasImages := false
	for _, block := range content.Array() {
		if block.Get("type").Str == "image" {
			hasImages = true
			break
		}
	}
	if !hasImages {
		return vibeUnifiedContentText(content, "\n")
	}
	var blocks []map[string]string
	for _, block := range content.Array() {
		if block.Get("type").Str == "image" {
			blocks = append(blocks, map[string]string{
				"type": "input_image", "image_url": "data:" + block.Get("mimeType").Str + ";base64," + block.Get("data").Str,
			})
		} else {
			text := vibeUnifiedContentText(gjson.Parse("["+block.Raw+"]"), "")
			blocks = append(blocks, map[string]string{"type": "text", "text": text})
		}
	}
	encoded, _ := json.Marshal(blocks, json.Deterministic(true))
	return string(encoded)
}

// vibeUnifiedStructuredResult renders structured_content, the only output of
// builtin tools. bash reports stdout and stderr, read_file the file text, and
// other tools a small JSON object that is kept as written.
func vibeUnifiedStructuredResult(raw jsontext.Value) string {
	value := gjson.ParseBytes(raw)
	switch {
	case value.Type == gjson.String:
		return value.Str
	case !value.IsObject():
		if value.Type == gjson.Null {
			return ""
		}
		return value.Raw
	}
	stdout, stderr := value.Get("stdout"), value.Get("stderr")
	if stdout.Type == gjson.String || stderr.Type == gjson.String {
		text := stdout.Str
		if stderr.Str != "" && text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text + stderr.Str
	}
	if content := value.Get("content"); content.Type == gjson.String {
		return content.Str
	}
	return value.Raw
}

// vibeUnifiedParentModel reads the model a subagent's direct parent pins.
// Subagents record no model of their own and run on their parent's, so the
// model is resolved when the subagent itself is parsed and kept afterward.
func vibeUnifiedParentModel(sessionDir, parentID string) (string, error) {
	if !isSafeSinglePathComponent(parentID) {
		return "", nil
	}
	parent, err := loadVibeUnifiedGeneration(filepath.Join(filepath.Dir(sessionDir), parentID))
	if err == nil {
		var parentMeta vibeUnifiedRuntimeMetadata
		if err = readVibeUnifiedDoc(parent.dir, "runtime-state.json", &parentMeta); err == nil {
			return parentMeta.SessionMetadata.ActiveModel, nil
		}
	}
	if errors.Is(err, errVibeUnsupportedVersion) {
		return "", err
	}
	return "", nil
}

// vibeUnifiedParseInputs lists every file a parse of the session reads.
func vibeUnifiedParseInputs(sessionDir string) ([]string, error) {
	gen, err := loadVibeUnifiedGeneration(sessionDir)
	if err != nil {
		return nil, err
	}
	paths := []string{
		filepath.Join(sessionDir, "CURRENT"),
		filepath.Join(sessionDir, "meta.json"),
		filepath.Join(gen.dir, "manifest.json"),
		filepath.Join(gen.dir, "projection-state.json"),
		filepath.Join(gen.dir, "runtime-state.json"),
	}
	chunks, err := gen.chunkPaths()
	if err != nil {
		return nil, err
	}
	paths = append(paths, chunks...)
	journal, err := gen.journalPath()
	if err != nil {
		return nil, err
	}
	if journal != "" {
		paths = append(paths, journal)
	}
	var runtimeMeta vibeUnifiedRuntimeMetadata
	if err := readVibeUnifiedDoc(gen.dir, "runtime-state.json", &runtimeMeta); err != nil {
		return nil, err
	}
	if parentID := runtimeMeta.Identity.ParentSessionID; runtimeMeta.SessionMetadata.ActiveModel == "" && isSafeSinglePathComponent(parentID) {
		parentDir := filepath.Join(filepath.Dir(sessionDir), parentID)
		parent, err := loadVibeUnifiedGeneration(parentDir)
		if errors.Is(err, errVibeUnsupportedVersion) {
			return nil, err
		}
		if err == nil {
			paths = append(paths,
				filepath.Join(parentDir, "CURRENT"),
				filepath.Join(parent.dir, "manifest.json"),
				filepath.Join(parent.dir, "runtime-state.json"),
			)
		}
	}
	return paths, nil
}

// vibeUnifiedFingerprint covers each input that changes in place. CURRENT
// pins the generation documents by digest, meta.json is rewritten, and the
// journal segment grows by appends. The journal can reach tens of megabytes,
// so it contributes its size and mtime rather than a content hash.
func vibeUnifiedFingerprint(sessionDir string) (SourceFingerprint, error) {
	var size, mtime int64
	hash := sha256.New()
	for _, path := range []string{
		filepath.Join(sessionDir, "CURRENT"),
		filepath.Join(sessionDir, "meta.json"),
	} {
		info, err := os.Stat(path)
		if filepath.Base(path) == "meta.json" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return SourceFingerprint{}, err
		}
		size += info.Size()
		mtime = max(mtime, info.ModTime().UnixNano())
		if err := addSiblingMetadataFingerprintPart(hash, filepath.Base(path), path, info); err != nil {
			return SourceFingerprint{}, err
		}
	}
	gen, err := loadVibeUnifiedGeneration(sessionDir)
	if err != nil {
		return SourceFingerprint{}, err
	}
	journal, err := gen.journalPath()
	if err != nil {
		return SourceFingerprint{}, err
	}
	if journal != "" {
		info, err := os.Stat(journal)
		if err != nil {
			return SourceFingerprint{}, err
		}
		size += info.Size()
		mtime = max(mtime, info.ModTime().UnixNano())
		_, _ = fmt.Fprintf(hash, "journal:%s:%d:%d\n", filepath.Base(journal), info.Size(), info.ModTime().UnixNano())
	}
	return SourceFingerprint{Size: size, MTimeNS: mtime, Hash: hex.EncodeToString(hash.Sum(nil))}, nil
}
