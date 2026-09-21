package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

var usageColumns = []struct{ name, typ, expression string }{
	{"usage_present", "UInt8", "token_usage != ''"},
	{"usage_input", "Int64", "JSONExtractInt(token_usage, 'input_tokens')"},
	{"usage_output", "Int64", "JSONExtractInt(token_usage, 'output_tokens')"},
	{"usage_cache_create", "Int64", "JSONExtractInt(token_usage, 'cache_creation_input_tokens')"},
	{"usage_cache_create_1h", "Int64", "JSONExtractInt(token_usage, 'cache_creation', 'ephemeral_1h_input_tokens')"},
	{"usage_cache_read", "Int64", "JSONExtractInt(token_usage, 'cache_read_input_tokens')"},
	{"usage_reasoning", "Int64", "JSONExtractInt(token_usage, 'reasoning_tokens')"},
	{"usage_web", "Int64", "JSONExtractInt(token_usage, 'server_tool_use', 'web_search_requests')"},
}

// Keep computed columns outside mirrorTables: push payloads contain only
// archive columns, and ClickHouse computes these for old and new clients alike.
func ensureUsageColumns(ctx context.Context, conn *sql.DB) error {
	const marker = "materialized_message_usage"
	metadata, err := readMetadata(ctx, conn, marker)
	if err != nil {
		return err
	}
	if metadata[marker] == "1" {
		return nil
	}
	var add, materialize []string
	for _, c := range usageColumns {
		add = append(add, "ADD COLUMN IF NOT EXISTS "+c.name+" "+c.typ+" MATERIALIZED "+c.expression)
		materialize = append(materialize, "MATERIALIZE COLUMN "+c.name)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE messages "+strings.Join(add, ", ")); err != nil {
		return fmt.Errorf("adding stored message usage: %w", err)
	}
	log.Print("ClickHouse: computing stored message usage for existing rows; waiting before serving")
	if _, err := conn.ExecContext(ctx, "ALTER TABLE messages "+strings.Join(materialize, ", ")+" SETTINGS mutations_sync = 1"); err != nil {
		return fmt.Errorf("materializing message usage (startup can retry): %w", err)
	}
	// Record completion only after existing parts have been rewritten. Repeating
	// this mutation after an interrupted startup leaves the source columns intact.
	return writeMetadata(ctx, conn, map[string]string{marker: "1"})
}
