package mirror

import "time"

// The types below are the bodies of records the live token monitor writes.
// Unlike the server objects in server.go, AgentsView defines them, so the
// monitor and the parser agree on them here.

// TokenCounts is one breakdown from thread/tokenUsage/updated. Input
// includes cached reads and cache writes, and Output includes
// ReasoningOutput, as OpenAI reports them.
type TokenCounts struct {
	Input           int64 `json:"input"`
	CachedInput     int64 `json:"cached_input"`
	CacheWriteInput int64 `json:"cache_write_input"`
	Output          int64 `json:"output"`
	ReasoningOutput int64 `json:"reasoning_output"`
	Total           int64 `json:"total"`
}

// TokenUsageData is the body of a KindTokenUsage record: one token update
// seen while joined. Total is the thread's running total; Last is the
// latest model call. JoinID names the joined connection that saw it.
type TokenUsageData struct {
	JoinID             string      `json:"join_id"`
	TurnID             string      `json:"turn_id"`
	Total              TokenCounts `json:"total"`
	Last               TokenCounts `json:"last"`
	ModelContextWindow int64       `json:"model_context_window,omitzero"`
	Model              string      `json:"model,omitzero"`
	ObservedAt         time.Time   `json:"observed_at"`
}

// TurnCompletedData is the body of a KindTurnCompleted record: the joined
// connection JoinID saw turn/completed for TurnID.
type TurnCompletedData struct {
	JoinID     string    `json:"join_id"`
	TurnID     string    `json:"turn_id"`
	ObservedAt time.Time `json:"observed_at"`
}
