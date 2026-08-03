package bunmodel

import "github.com/uptrace/bun"

// UsageEvent stores session- or message-level token accounting.
type UsageEvent struct {
	bun.BaseModel `bun:"table:usage_events"`

	ID                       int64      `bun:"id,pk,autoincrement"`
	SessionID                string     `bun:"session_id,notnull"`
	MessageOrdinal           *int       `bun:"message_ordinal,nullzero"`
	Source                   string     `bun:"source,notnull"`
	Model                    string     `bun:"model,notnull"`
	InputTokens              int        `bun:"input_tokens,notnull,default:0"`
	OutputTokens             int        `bun:"output_tokens,notnull,default:0"`
	CacheCreationInputTokens int        `bun:"cache_creation_input_tokens,notnull,default:0"`
	CacheReadInputTokens     int        `bun:"cache_read_input_tokens,notnull,default:0"`
	ReasoningTokens          int        `bun:"reasoning_tokens,notnull,default:0"`
	CostMicrodollars         *int64     `bun:"cost_microdollars,nullzero"`
	CostStatus               string     `bun:"cost_status,notnull,default:''"`
	CostSource               string     `bun:"cost_source,notnull,default:''"`
	OccurredAt               *Timestamp `bun:"occurred_at,type:TIMESTAMPTZ,nullzero"`
	DedupKey                 string     `bun:"dedup_key,notnull,default:''"`
}

// CursorUsageEvent stores authoritative Cursor admin usage data.
type CursorUsageEvent struct {
	bun.BaseModel `bun:"table:cursor_usage_events"`

	ID                         *int64    `bun:"id,nullzero"`
	OccurredAt                 Timestamp `bun:"occurred_at,type:TIMESTAMPTZ,notnull"`
	Model                      string    `bun:"model,notnull"`
	Kind                       string    `bun:"kind,notnull,default:''"`
	InputTokens                int       `bun:"input_tokens,notnull,default:0"`
	OutputTokens               int       `bun:"output_tokens,notnull,default:0"`
	CacheWriteTokens           int       `bun:"cache_write_tokens,notnull,default:0"`
	CacheReadTokens            int       `bun:"cache_read_tokens,notnull,default:0"`
	ChargedMicrodollars        int64     `bun:"charged_microdollars,notnull,default:0"`
	CursorTokenFeeMicrodollars int64     `bun:"cursor_token_fee_microdollars,notnull,default:0"`
	UserID                     string    `bun:"user_id,notnull,default:''"`
	UserEmail                  string    `bun:"user_email,notnull,default:''"`
	IsHeadless                 bool      `bun:"is_headless,notnull,default:false"`
	DedupKey                   string    `bun:"dedup_key,notnull,default:''"`
}
