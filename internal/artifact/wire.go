// Package artifact implements the local-first artifact ledger: the on-disk
// wire format, name validation, and store contract shared by every peer that
// exports and imports session data.
package artifact

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// Artifact kinds. Each kind maps to a top-level directory in an artifact
// store: checkpoints/, manifests/, segments/, meta/, raw/.
const (
	KindCheckpoints = "checkpoints"
	KindManifests   = "manifests"
	KindSegments    = "segments"
	KindMeta        = "meta"
	KindRaw         = "raw"
)

var (
	ErrArtifactInvalid  = errors.New("invalid artifact")
	ErrArtifactNotFound = errors.New("artifact not found")
	ErrArtifactConflict = errors.New("artifact conflict")
)

var errIncompleteArtifact = errors.New("incomplete artifact")

// Wire versions advance independently because each artifact kind has its own
// schema and consumers. A change to one kind must not invalidate the others.
const (
	checkpointFormatVersion = 1
	// Manifest v2 replaces usage_events[].cost_usd floats with exact
	// integer-microdollar cost objects. Manifest v3 adds the optional
	// session_kind provenance field; v2 manifests still decode, with the
	// field defaulting to empty.
	manifestFormatVersion    = 3
	manifestMinDecodeVersion = 2
	// Segment v2 adds the optional prompt_source provenance field on
	// message records; v1 segments still decode, with the field defaulting
	// to empty.
	messageSegmentFormatVersion    = 2
	messageSegmentMinDecodeVersion = 1
	metadataEventFormatVersion     = 1
)

// metadataEventExtension is the file extension for metadata event artifacts.
const metadataEventExtension = ".json"

func validateArtifactName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: artifact name is required", ErrArtifactInvalid)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("%w: invalid artifact name", ErrArtifactInvalid)
	}
	return nil
}

func normalizeMetadataName(name string) (filename, hash string, err error) {
	base := strings.TrimSuffix(name, metadataEventExtension)
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return "", "", fmt.Errorf("%w: metadata artifact missing hash suffix", ErrArtifactInvalid)
	}
	hash = base[idx+1:]
	if err := validateHashHex(hash); err != nil {
		return "", "", err
	}
	return base + metadataEventExtension, hash, nil
}

func normalizeCheckpointName(name string) (string, error) {
	base := strings.TrimSuffix(name, ".json")
	if _, err := checkpointSequence(base + ".json"); err != nil {
		return "", err
	}
	return base + ".json", nil
}

func checkpointSequence(filename string) (int, error) {
	sequence, err := db.ParseArtifactCheckpointSequence(filename)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	return sequence, nil
}

func validateHashHex(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("%w: invalid artifact hash", ErrArtifactInvalid)
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: invalid artifact hash", ErrArtifactInvalid)
		}
	}
	return nil
}

func validateOriginID(origin string) error {
	return config.ValidateArtifactOriginID(origin)
}

type checkpoint struct {
	Version  int               `json:"v"`
	Origin   string            `json:"origin"`
	Sequence int               `json:"seq"`
	Sessions map[string]string `json:"sessions"`
}

type manifest struct {
	Version         int                  `json:"v"`
	Origin          string               `json:"origin"`
	NativeSessionID string               `json:"native_session_id"`
	Session         manifestSession      `json:"session"`
	SessionName     *string              `json:"session_name,omitempty"`
	Segments        []string             `json:"segments"`
	UsageEvents     []artifactUsageEvent `json:"usage_events,omitempty"`
	RawSource       *RawSourceRef        `json:"raw_source,omitempty"`
	DataVersion     int                  `json:"data_version"`
	Generation      int                  `json:"generation"`
	// Signal state persisted on the session row but absent from the wire
	// Session above, which mirrors only db.Session's JSON-visible fields.
	// Carried explicitly so an imported session keeps its tool-call, context,
	// and quality signal state instead of resetting to false/zero. Secret-scan
	// state is deliberately not carried: findings live outside the manifest,
	// so imported sessions are treated as unscanned.
	SessionHasToolCalls   bool                    `json:"session_has_tool_calls,omitzero"`
	SessionHasContextData bool                    `json:"session_has_context_data,omitzero"`
	SessionQualitySignals *manifestQualitySignals `json:"session_quality_signals,omitempty"`
}

type artifactUsageEvent struct {
	MessageOrdinal           *int         `json:"message_ordinal,omitempty"`
	Source                   string       `json:"source"`
	Model                    string       `json:"model"`
	InputTokens              int          `json:"input_tokens,omitzero"`
	OutputTokens             int          `json:"output_tokens,omitzero"`
	CacheCreationInputTokens int          `json:"cache_creation_input_tokens,omitzero"`
	CacheReadInputTokens     int          `json:"cache_read_input_tokens,omitzero"`
	ReasoningTokens          int          `json:"reasoning_tokens,omitzero"`
	Cost                     *money.Money `json:"cost,omitempty"`
	CostStatus               string       `json:"cost_status,omitempty"`
	CostSource               string       `json:"cost_source,omitempty"`
	OccurredAt               string       `json:"occurred_at,omitempty"`
	DedupKey                 string       `json:"dedup_key,omitempty"`
}

// RawSourceRef identifies one bounded raw provider artifact without embedding
// its contents in a portable manifest.
type RawSourceRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
	Path      string `json:"path,omitempty"`
}

type metadataEvent struct {
	Version    int            `json:"v"`
	HLC        string         `json:"hlc"`
	Origin     string         `json:"origin"`
	SessionGID string         `json:"session_gid"`
	Op         string         `json:"op"`
	Value      jsontext.Value `json:"value,omitempty"`
	Pin        *MetadataPin   `json:"pin,omitempty"`
}

// MetadataPin identifies a pinned message with stable source coordinates.
type MetadataPin struct {
	SourceUUID string  `json:"source_uuid,omitempty"`
	Ordinal    int     `json:"ordinal"`
	Note       *string `json:"note,omitempty"`
}

type segmentMessage struct {
	Version           int               `json:"v"`
	Ordinal           int               `json:"ordinal"`
	Role              string            `json:"role"`
	Content           string            `json:"content"`
	ThinkingText      string            `json:"thinking_text,omitempty"`
	Timestamp         string            `json:"timestamp,omitempty"`
	HasThinking       bool              `json:"has_thinking,omitzero"`
	HasToolUse        bool              `json:"has_tool_use,omitzero"`
	ContentLength     int               `json:"content_length,omitzero"`
	Model             string            `json:"model,omitempty"`
	TokenUsage        jsontext.Value    `json:"token_usage,omitempty"`
	ContextTokens     int               `json:"context_tokens,omitzero"`
	OutputTokens      int               `json:"output_tokens,omitzero"`
	HasContextTokens  bool              `json:"has_context_tokens,omitzero"`
	HasOutputTokens   bool              `json:"has_output_tokens,omitzero"`
	ClaudeMessageID   string            `json:"claude_message_id,omitempty"`
	ClaudeRequestID   string            `json:"claude_request_id,omitempty"`
	ToolCalls         []segmentToolCall `json:"tool_calls,omitempty"`
	IsSystem          bool              `json:"is_system,omitzero"`
	SourceType        string            `json:"source_type,omitempty"`
	SourceSubtype     string            `json:"source_subtype,omitempty"`
	PromptSource      string            `json:"prompt_source,omitempty"`
	SourceUUID        string            `json:"source_uuid,omitempty"`
	SourceParentUUID  string            `json:"source_parent_uuid,omitempty"`
	IsSidechain       bool              `json:"is_sidechain,omitzero"`
	IsCompactBoundary bool              `json:"is_compact_boundary,omitzero"`
}

type segmentToolCall struct {
	CallIndex           int                  `json:"call_index"`
	ToolName            string               `json:"tool_name"`
	Category            string               `json:"category,omitempty"`
	ToolUseID           string               `json:"tool_use_id,omitempty"`
	InputJSON           string               `json:"input_json,omitempty"`
	FilePath            string               `json:"file_path,omitempty"`
	SkillName           string               `json:"skill_name,omitempty"`
	ResultContentLength int                  `json:"result_content_length,omitempty"`
	ResultContent       string               `json:"result_content,omitempty"`
	SubagentSessionID   string               `json:"subagent_session_id,omitempty"`
	ResultEvents        []segmentResultEvent `json:"result_events,omitempty"`
}

type segmentResultEvent struct {
	ToolUseID         string `json:"tool_use_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	Content           string `json:"content"`
	ContentLength     int    `json:"content_length,omitempty"`
	Timestamp         string `json:"timestamp,omitempty"`
	EventIndex        int    `json:"event_index"`
}

// rawSourceMediaTypes is the fixed allowlist for raw source snapshots.
// application/jsonl covers JSONL agent session files (claude, codex).
var rawSourceMediaTypes = map[string]bool{
	"application/jsonl": true,
}

// maxRawSourceSize bounds one raw source snapshot (1 GiB).
const maxRawSourceSize = int64(1 << 30)

// ValidateRawSource checks a manifest's optional raw_source reference
// against the stable wire contract. A nil reference is valid (raw capture is
// optional).
func ValidateRawSource(raw *RawSourceRef) error {
	if raw == nil {
		return nil
	}
	if err := validateHashHex(raw.Hash); err != nil {
		return err
	}
	if raw.Size < 0 || raw.Size > maxRawSourceSize {
		return fmt.Errorf("%w: raw source size out of range", ErrArtifactInvalid)
	}
	if raw.MediaType != "" && !rawSourceMediaTypes[raw.MediaType] {
		return fmt.Errorf("%w: unsupported raw source media type %q",
			ErrArtifactInvalid, raw.MediaType)
	}
	if raw.Path != "" {
		if strings.Contains(raw.Path, "\\") || strings.Contains(raw.Path, "..") {
			return fmt.Errorf("%w: invalid raw source path", ErrArtifactInvalid)
		}
		// A colon anywhere rejects Windows drive-qualified (C:/...) and
		// volume-relative (C:...) paths, NTFS alternate data streams, and
		// URI schemes in one platform-neutral rule.
		if strings.HasPrefix(raw.Path, "/") || strings.Contains(raw.Path, ":") {
			return fmt.Errorf("%w: raw source path must be relative",
				ErrArtifactInvalid)
		}
	}
	return nil
}

func encodeSegment(msgs []db.Message) ([]byte, error) {
	var buf bytes.Buffer
	for _, msg := range msgs {
		data, err := canonicalJSON(segmentMessageFromDB(msg))
		if err != nil {
			return nil, fmt.Errorf("encoding message segment: %w", err)
		}
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

func segmentMessageFromDB(msg db.Message) segmentMessage {
	record := segmentMessage{
		Version:           messageSegmentFormatVersion,
		Ordinal:           msg.Ordinal,
		Role:              msg.Role,
		Content:           msg.Content,
		ThinkingText:      msg.ThinkingText,
		Timestamp:         msg.Timestamp,
		HasThinking:       msg.HasThinking,
		HasToolUse:        msg.HasToolUse,
		ContentLength:     msg.ContentLength,
		Model:             msg.Model,
		TokenUsage:        msg.TokenUsage,
		ContextTokens:     msg.ContextTokens,
		OutputTokens:      msg.OutputTokens,
		HasContextTokens:  msg.HasContextTokens,
		HasOutputTokens:   msg.HasOutputTokens,
		ClaudeMessageID:   msg.ClaudeMessageID,
		ClaudeRequestID:   msg.ClaudeRequestID,
		IsSystem:          msg.IsSystem,
		SourceType:        msg.SourceType,
		SourceSubtype:     msg.SourceSubtype,
		PromptSource:      msg.PromptSource,
		SourceUUID:        msg.SourceUUID,
		SourceParentUUID:  msg.SourceParentUUID,
		IsSidechain:       msg.IsSidechain,
		IsCompactBoundary: msg.IsCompactBoundary,
	}
	if len(msg.ToolCalls) > 0 {
		record.ToolCalls = make([]segmentToolCall, len(msg.ToolCalls))
		for i, call := range msg.ToolCalls {
			record.ToolCalls[i] = segmentToolCall{
				CallIndex:           i,
				ToolName:            call.ToolName,
				Category:            call.Category,
				ToolUseID:           call.ToolUseID,
				InputJSON:           call.InputJSON,
				FilePath:            call.FilePath,
				SkillName:           call.SkillName,
				ResultContentLength: call.ResultContentLength,
				ResultContent:       call.ResultContent,
				SubagentSessionID:   call.SubagentSessionID,
			}
			if len(call.ResultEvents) > 0 {
				record.ToolCalls[i].ResultEvents = make([]segmentResultEvent, len(call.ResultEvents))
				for j, ev := range call.ResultEvents {
					record.ToolCalls[i].ResultEvents[j] = segmentResultEvent{
						ToolUseID:         ev.ToolUseID,
						AgentID:           ev.AgentID,
						SubagentSessionID: ev.SubagentSessionID,
						Source:            ev.Source,
						Status:            ev.Status,
						Content:           ev.Content,
						ContentLength:     ev.ContentLength,
						Timestamp:         ev.Timestamp,
						EventIndex:        ev.EventIndex,
					}
				}
			}
		}
	}
	return record
}
