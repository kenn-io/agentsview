package db

import (
	"context"
	"fmt"
)

// Insight represents a row in the insights table.
type Insight struct {
	ID              int64   `json:"id"`
	Type            string  `json:"type"`
	DateFrom        string  `json:"date_from"`
	DateTo          string  `json:"date_to"`
	Project         *string `json:"project"`
	Agent           string  `json:"agent"`
	Model           *string `json:"model"`
	Prompt          *string `json:"prompt"`
	Content         string  `json:"content"`
	Kind            string  `json:"kind,omitempty"`
	SchemaVersion   string  `json:"schema_version,omitempty"`
	TemplateID      string  `json:"template_id,omitempty"`
	TemplateVersion string  `json:"template_version,omitempty"`
	AggregateHash   string  `json:"aggregate_hash,omitempty"`
	CacheKey        string  `json:"cache_key,omitempty"`
	CacheStatus     string  `json:"cache_status,omitempty"`
	ProvenanceJSON  string  `json:"provenance_json,omitempty"`
	StructuredJSON  string  `json:"structured_json,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

// InsightFilter specifies how to query insights.
type InsightFilter struct {
	Type       string // "daily_activity" or "agent_analysis"
	Project    string // "" = no filter
	GlobalOnly bool   // true = project IS NULL only
	DateFrom   string // YYYY-MM-DD, "" = no filter
	DateTo     string // YYYY-MM-DD, "" = no filter
}

const maxInsights = 500

// CopyInsightsFrom copies all insights from the database at
// sourcePath into this database using ATTACH/DETACH.
func (db *DB) CopyInsightsFrom(sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Pin a single connection for the ATTACH/INSERT/DETACH
	// sequence. database/sql's pool doesn't guarantee the
	// same underlying connection across separate Exec calls,
	// and ATTACH is connection-scoped.
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(
			ctx, "DETACH DATABASE old_db",
		)
	}()

	hasCol := func(name string) bool {
		var count int
		err := conn.QueryRowContext(ctx,
			`SELECT count(*)
			 FROM old_db.pragma_table_info('insights')
			 WHERE name = ?`,
			name,
		).Scan(&count)
		return err == nil && count > 0
	}
	colExpr := func(name string) string {
		if hasCol(name) {
			return "COALESCE(" + name + ", '')"
		}
		return "''"
	}

	_, err = conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO insights
			(type, date_from, date_to, project,
			 agent, model, prompt, content,
			 kind, schema_version, template_id,
			 template_version, aggregate_hash, cache_key,
			 cache_status, provenance_json, structured_json,
			 created_at)
		SELECT type, date_from, date_to, project,
			agent, model, prompt, content,
			`+colExpr("kind")+`,
			`+colExpr("schema_version")+`,
			`+colExpr("template_id")+`,
			`+colExpr("template_version")+`,
			`+colExpr("aggregate_hash")+`,
			`+colExpr("cache_key")+`,
			`+colExpr("cache_status")+`,
			`+colExpr("provenance_json")+`,
			`+colExpr("structured_json")+`,
			created_at
		FROM old_db.insights`)
	if err != nil {
		return fmt.Errorf("copying insights: %w", err)
	}
	return nil
}
