// Package service provides the canonical session-operation interface
// shared by the HTTP handlers and the CLI. Both are thin JSON encoders
// that delegate to a SessionService implementation.
package service

import (
	"context"
	"io"

	"go.kenn.io/agentsview/internal/db"
)

// SessionService is the canonical per-session operation interface.
// Two implementations: directBackend (wraps *db.DB) and httpBackend
// (proxies to a running daemon).
type SessionService interface {
	Get(ctx context.Context, id string) (*SessionDetail, error)
	List(ctx context.Context, f ListFilter) (*SessionList, error)
	Messages(ctx context.Context, id string, f MessageFilter) (*MessageList, error)
	ToolCalls(ctx context.Context, id string) (*ToolCallList, error)
	Sync(ctx context.Context, in SyncInput) (*SessionDetail, error)
	Watch(ctx context.Context, id string) (<-chan Event, error)
	Stats(ctx context.Context, f StatsFilter) (*SessionStats, error)
	SearchContent(ctx context.Context, req ContentSearchRequest) (*ContentSearchResult, error)
	ListSecrets(ctx context.Context, f SecretListFilter) (*SecretFindingList, error)
	ScanSecrets(ctx context.Context, in SecretScanInput,
		progress func(SecretScanProgress)) (*SecretScanSummary, error)
}

// SecretScanInput parameterises ScanSecrets (mirrors sync.SecretScanInput).
type SecretScanInput struct {
	Backfill bool
	Project  string
	Agent    string
	DateFrom string
	DateTo   string
}

// SecretScanProgress is one progress tick (mirrors sync.SecretScanProgress).
type SecretScanProgress struct {
	Scanned int `json:"scanned"`
	Total   int `json:"total"`
}

// SecretScanSummary is the final scan result (mirrors sync.SecretScanSummary).
type SecretScanSummary struct {
	Scanned           int `json:"scanned"`
	WithSecrets       int `json:"with_secrets"`
	TotalFindings     int `json:"total_findings"`
	DefiniteFindings  int `json:"definite_findings"`
	CandidateFindings int `json:"candidate_findings"`
}

// SecretListFilter parameterises ListSecrets.
type SecretListFilter struct {
	Project    string
	Agent      string
	DateFrom   string
	DateTo     string
	Rule       string
	Confidence string
	Reveal     bool
	Limit      int
	Cursor     int
}

// SecretFindingList is a page of secret findings for transport. When the
// request set Reveal, each row's RedactedMatch holds the full value (or a
// "source changed" marker) instead of the redacted form.
type SecretFindingList struct {
	Findings   []db.SecretFindingRow `json:"findings"`
	NextCursor int                   `json:"next_cursor,omitempty"`
}

// ContentSearchRequest is the transport-neutral content-search input.
type ContentSearchRequest struct {
	Pattern       string   `json:"pattern"`
	Mode          string   `json:"mode,omitempty"` // substring|regex|fts
	Sources       []string `json:"sources,omitempty"`
	ExcludeSystem bool     `json:"exclude_system,omitempty"`
	Reveal        bool     `json:"reveal,omitempty"`

	Project, ExcludeProject, Machine, Agent           string
	Date, DateFrom, DateTo, ActiveSince               string
	IncludeChildren, IncludeAutomated, IncludeOneShot bool

	Limit  int `json:"limit,omitempty"`
	Cursor int `json:"cursor,omitempty"`
}

// ContentSearchResult mirrors db.ContentSearchPage for transport.
type ContentSearchResult struct {
	Matches    []db.ContentMatch `json:"matches"`
	NextCursor int               `json:"next_cursor,omitempty"`
}

// SessionDetail mirrors the HTTP GetSession response shape: a
// db.Session plus the computed health-breakdown fields that the
// detail endpoint enriches. Both direct and HTTP backends return
// this type so CLI JSON output is transport-neutral.
type SessionDetail struct {
	db.Session
	HealthScoreBasis []string       `json:"health_score_basis,omitempty"`
	HealthPenalties  map[string]int `json:"health_penalties,omitempty"`
}

// SessionList mirrors GET /api/v1/sessions.
type SessionList struct {
	Sessions   []db.Session `json:"sessions"`
	NextCursor string       `json:"next_cursor,omitempty"`
	Total      int          `json:"total"`
}

// ListFilter mirrors the HTTP query parameters in handleListSessions.
// Field names map to HTTP query param names via json tags.
type ListFilter struct {
	Project          string `json:"project,omitempty"`
	ExcludeProject   string `json:"exclude_project,omitempty"`
	Machine          string `json:"machine,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Date             string `json:"date,omitempty"`
	DateFrom         string `json:"date_from,omitempty"`
	DateTo           string `json:"date_to,omitempty"`
	ActiveSince      string `json:"active_since,omitempty"`
	MinMessages      int    `json:"min_messages,omitempty"`
	MaxMessages      int    `json:"max_messages,omitempty"`
	MinUserMessages  int    `json:"min_user_messages,omitempty"`
	IncludeOneShot   bool   `json:"include_one_shot,omitempty"`
	IncludeAutomated bool   `json:"include_automated,omitempty"`
	IncludeChildren  bool   `json:"include_children,omitempty"`
	Outcome          string `json:"outcome,omitempty"`      // comma-separated
	HealthGrade      string `json:"health_grade,omitempty"` // comma-separated
	Termination      string `json:"termination,omitempty"`  // comma-separated
	MinToolFailures  *int   `json:"min_tool_failures,omitempty"`
	HasSecret        bool   `json:"has_secret,omitempty"`
	Starred          bool   `json:"starred,omitempty"`
	Cursor           string `json:"cursor,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	// OrderBy selects the sort column ("" = recent activity). Descending
	// overrides the sort key's canonical direction when non-nil.
	OrderBy    string `json:"order_by,omitempty"`
	Descending *bool  `json:"descending,omitempty"`
}

// MessageFilter mirrors GET /api/v1/sessions/{id}/messages query params.
// From is a pointer so callers can distinguish "omitted" from "0". An
// omitted From in descending mode means "start from the newest message";
// an explicit 0 means "start at ordinal 0".
type MessageFilter struct {
	From      *int   `json:"from,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Direction string `json:"direction,omitempty"` // "asc" (default) or "desc"
}

// MessageList mirrors {messages, count}.
type MessageList struct {
	Messages []db.Message `json:"messages"`
	Count    int          `json:"count"`
}

// ToolCall mirrors a flattened tool call with its enclosing message's
// ordinal/timestamp attached. Serialized from parser.ParsedToolCall.
type ToolCall struct {
	Ordinal           int    `json:"ordinal"`
	Timestamp         string `json:"timestamp"` // RFC3339
	ToolUseID         string `json:"tool_use_id"`
	ToolName          string `json:"tool_name"`
	Category          string `json:"category"`
	InputJSON         string `json:"input_json"`
	SkillName         string `json:"skill_name,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	ResultLength      int    `json:"result_length"`
}

// ToolCallList mirrors {tool_calls, count}.
type ToolCallList struct {
	ToolCalls []ToolCall `json:"tool_calls"`
	Count     int        `json:"count"`
}

// SyncInput carries the payload for a per-session sync.
// Exactly one of Path or ID must be set.
type SyncInput struct {
	Path string `json:"path,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Event is the CLI-side NDJSON wrapper for SSE events from
// /api/v1/sessions/{id}/watch. See spec "watch" section.
type Event struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

// ExportFiles is a filesystem helper, not on SessionService.
// Used by the CLI `session export` command to stream raw JSONL.
type ExportFiles interface {
	FilePath(id string) string
	Open(path string) (io.ReadCloser, error)
}
