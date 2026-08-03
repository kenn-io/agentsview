package bunmodel

import (
	"encoding/json"

	"github.com/uptrace/bun"
)

// Message uses the portable logical key (session_id, ordinal). ID is retained
// only as an optional source-row identifier for the shipped SQLite archive.
type Message struct {
	bun.BaseModel `bun:"table:messages"`

	ID                *int64          `bun:"id,nullzero"`
	SessionID         string          `bun:"session_id,pk"`
	Ordinal           int             `bun:"ordinal,pk"`
	Role              string          `bun:"role,notnull"`
	Content           string          `bun:"content,notnull"`
	ThinkingText      string          `bun:"thinking_text,notnull,default:''"`
	Timestamp         *Timestamp      `bun:"timestamp,type:TIMESTAMPTZ,nullzero"`
	HasThinking       bool            `bun:"has_thinking,notnull,default:false"`
	HasToolUse        bool            `bun:"has_tool_use,notnull,default:false"`
	ContentLength     int             `bun:"content_length,notnull,default:0"`
	IsSystem          bool            `bun:"is_system,notnull,default:false"`
	Model             string          `bun:"model,notnull,default:''"`
	TokenUsage        json.RawMessage `bun:"token_usage,type:TEXT,notnull,default:''"`
	ContextTokens     int             `bun:"context_tokens,notnull,default:0"`
	OutputTokens      int             `bun:"output_tokens,notnull,default:0"`
	HasContextTokens  bool            `bun:"has_context_tokens,notnull,default:false"`
	HasOutputTokens   bool            `bun:"has_output_tokens,notnull,default:false"`
	ClaudeMessageID   string          `bun:"claude_message_id,notnull,default:''"`
	ClaudeRequestID   string          `bun:"claude_request_id,notnull,default:''"`
	SourceType        string          `bun:"source_type,notnull,default:''"`
	SourceSubtype     string          `bun:"source_subtype,notnull,default:''"`
	PromptSource      string          `bun:"prompt_source,notnull,default:''"`
	SourceUUID        string          `bun:"source_uuid,notnull,default:''"`
	SourceParentUUID  string          `bun:"source_parent_uuid,notnull,default:''"`
	IsSidechain       bool            `bun:"is_sidechain,notnull,default:false"`
	IsCompactBoundary bool            `bun:"is_compact_boundary,notnull,default:false"`
}

// ToolCall is located by the canonical message key and call index. The source
// row and SQLite message IDs are optional data, not relationships.
type ToolCall struct {
	bun.BaseModel `bun:"table:tool_calls"`

	ID                  *int64  `bun:"id,nullzero"`
	MessageID           *int64  `bun:"message_id,nullzero"`
	SessionID           string  `bun:"session_id,notnull"`
	MessageOrdinal      int     `bun:"message_ordinal,notnull"`
	ToolName            string  `bun:"tool_name,notnull"`
	Category            string  `bun:"category,notnull"`
	CallIndex           int     `bun:"call_index,notnull,default:0"`
	ToolUseID           string  `bun:"tool_use_id,notnull,default:''"`
	InputJSON           *string `bun:"input_json,nullzero"`
	SkillName           *string `bun:"skill_name,nullzero"`
	ResultContentLength *int    `bun:"result_content_length,nullzero"`
	ResultContent       *string `bun:"result_content,nullzero"`
	SubagentSessionID   *string `bun:"subagent_session_id,nullzero"`
	FilePath            *string `bun:"file_path,nullzero"`
}

// ToolResultEvent is one chronological output update for a tool call.
type ToolResultEvent struct {
	bun.BaseModel `bun:"table:tool_result_events"`

	ID                     *int64     `bun:"id,nullzero"`
	SessionID              string     `bun:"session_id,notnull"`
	ToolCallMessageOrdinal int        `bun:"tool_call_message_ordinal,notnull"`
	CallIndex              int        `bun:"call_index,notnull,default:0"`
	ToolUseID              *string    `bun:"tool_use_id,nullzero"`
	AgentID                *string    `bun:"agent_id,nullzero"`
	SubagentSessionID      *string    `bun:"subagent_session_id,nullzero"`
	Source                 string     `bun:"source,notnull"`
	Status                 string     `bun:"status,notnull"`
	Content                string     `bun:"content,notnull"`
	ContentLength          int        `bun:"content_length,notnull,default:0"`
	Timestamp              *Timestamp `bun:"timestamp,type:TIMESTAMPTZ,nullzero"`
	EventIndex             int        `bun:"event_index,notnull,default:0"`
}
