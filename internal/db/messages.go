package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	selectMessageCols = `id, session_id, ordinal, role, content,
		thinking_text,
		COALESCE(timestamp, '') AS timestamp,
		has_thinking, has_tool_use, content_length,
		is_system,
		model, reasoning_effort, token_usage, context_tokens, output_tokens, provider_id,
		has_context_tokens, has_output_tokens,
		claude_message_id, claude_request_id,
		source_type, source_subtype, prompt_source, source_uuid,
		source_parent_uuid, is_sidechain, is_compact_boundary`

	// DefaultMessageLimit is the default number of messages returned.
	DefaultMessageLimit = 100
	// MaxMessageLimit is the maximum number of messages returned.
	MaxMessageLimit = 1000

	// Keep query parameter counts conservative so large sessions
	// do not exceed SQLite variable limits when hydrating tool calls.
	attachToolCallBatchSize = 500

	// Six parameters per row, below SQLite's historical variable limit.
	toolCallAgentStateRowsPerStmt = 166
