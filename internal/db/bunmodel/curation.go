package bunmodel

import "github.com/uptrace/bun"

type StarredSession struct {
	bun.BaseModel `bun:"table:starred_sessions"`

	SessionID string    `bun:"session_id,pk"`
	CreatedAt Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}

type PinnedMessage struct {
	bun.BaseModel `bun:"table:pinned_messages"`

	ID         int64     `bun:"id,pk,autoincrement"`
	SessionID  string    `bun:"session_id,notnull"`
	MessageID  *int64    `bun:"message_id,nullzero"`
	Ordinal    int       `bun:"ordinal,notnull"`
	SourceUUID string    `bun:"source_uuid,notnull,default:''"`
	Note       *string   `bun:"note,nullzero"`
	CreatedAt  Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}

type ExcludedSession struct {
	bun.BaseModel `bun:"table:excluded_sessions"`

	ID        string    `bun:"id,pk"`
	CreatedAt Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}

type SessionAlias struct {
	bun.BaseModel `bun:"table:session_aliases"`

	SessionID string    `bun:"session_id,pk"`
	AliasID   string    `bun:"alias_id,pk"`
	CreatedAt Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}

type Insight struct {
	bun.BaseModel `bun:"table:insights"`

	ID              int64     `bun:"id,pk,autoincrement"`
	Type            string    `bun:"type,notnull"`
	DateFrom        string    `bun:"date_from,notnull"`
	DateTo          string    `bun:"date_to,notnull"`
	Project         *string   `bun:"project,nullzero"`
	Agent           string    `bun:"agent,notnull"`
	Model           *string   `bun:"model,nullzero"`
	Prompt          *string   `bun:"prompt,nullzero"`
	Content         string    `bun:"content,notnull"`
	Kind            string    `bun:"kind,notnull,default:''"`
	SchemaVersion   string    `bun:"schema_version,notnull,default:''"`
	TemplateID      string    `bun:"template_id,notnull,default:''"`
	TemplateVersion string    `bun:"template_version,notnull,default:''"`
	AggregateHash   string    `bun:"aggregate_hash,notnull,default:''"`
	CacheKey        string    `bun:"cache_key,notnull,default:''"`
	CacheStatus     string    `bun:"cache_status,notnull,default:''"`
	ProvenanceJSON  string    `bun:"provenance_json,notnull,default:''"`
	StructuredJSON  string    `bun:"structured_json,notnull,default:''"`
	CreatedAt       Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}

type SecretFinding struct {
	bun.BaseModel `bun:"table:secret_findings"`

	ID             int64     `bun:"id,pk,autoincrement"`
	SessionID      string    `bun:"session_id,notnull"`
	RuleName       string    `bun:"rule_name,notnull"`
	Confidence     string    `bun:"confidence,notnull"`
	LocationKind   string    `bun:"location_kind,notnull"`
	MessageOrdinal int       `bun:"message_ordinal,notnull"`
	CallIndex      *int      `bun:"call_index,nullzero"`
	EventIndex     *int      `bun:"event_index,nullzero"`
	MatchStart     int       `bun:"match_start,notnull"`
	MatchEnd       int       `bun:"match_end,notnull"`
	MatchIndex     int       `bun:"match_index,notnull"`
	RedactedMatch  string    `bun:"redacted_match,notnull"`
	RulesVersion   string    `bun:"rules_version,notnull"`
	CreatedAt      Timestamp `bun:"created_at,type:TIMESTAMPTZ,notnull"`
}
