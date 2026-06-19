package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

const insightBaseCols = `id, type, date_from, date_to,
	project, agent, model, prompt, content,
	kind, schema_version, template_id, template_version,
	aggregate_hash, cache_key, cache_status,
	provenance_json, structured_json, created_at`

func scanInsightRow(rs rowScanner) (Insight, error) {
	var s Insight
	err := rs.Scan(
		&s.ID, &s.Type, &s.DateFrom, &s.DateTo,
		&s.Project, &s.Agent,
		&s.Model, &s.Prompt, &s.Content,
		&s.Kind, &s.SchemaVersion, &s.TemplateID,
		&s.TemplateVersion, &s.AggregateHash, &s.CacheKey,
		&s.CacheStatus, &s.ProvenanceJSON, &s.StructuredJSON,
		&s.CreatedAt,
	)
	return s, err
}

func buildInsightFilter(
	f InsightFilter,
) (string, []any) {
	var preds []string
	var args []any

	if f.Type != "" {
		preds = append(preds, "type = ?")
		args = append(args, f.Type)
	}
	if f.GlobalOnly {
		preds = append(preds, "project IS NULL")
	} else if f.Project != "" {
		preds = append(preds, "project = ?")
		args = append(args, f.Project)
	}
	if f.DateFrom != "" {
		preds = append(preds, "date_from >= ?")
		args = append(args, f.DateFrom)
	}
	if f.DateTo != "" {
		preds = append(preds, "date_to <= ?")
		args = append(args, f.DateTo)
	}

	if len(preds) == 0 {
		return "1=1", nil
	}
	return strings.Join(preds, " AND "), args
}

// InsertInsight inserts an insight and returns its ID.
func (db *DB) InsertInsight(s Insight) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.getWriter().Exec(`
		INSERT INTO insights (
			type, date_from, date_to, project,
			agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Type, s.DateFrom, s.DateTo, s.Project,
		s.Agent, s.Model, s.Prompt, s.Content,
		s.Kind, s.SchemaVersion, s.TemplateID,
		s.TemplateVersion, s.AggregateHash, s.CacheKey,
		s.CacheStatus, s.ProvenanceJSON, s.StructuredJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting insight: %w", err)
	}
	return res.LastInsertId()
}

// GetCachedInsight returns the newest insight saved with cacheKey.
// Returns nil, nil if no cache entry exists.
func (db *DB) GetCachedInsight(
	ctx context.Context, cacheKey string,
) (*Insight, error) {
	if strings.TrimSpace(cacheKey) == "" {
		return nil, nil
	}
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+insightBaseCols+
			" FROM insights WHERE cache_key = ?"+
			" ORDER BY created_at DESC, id DESC LIMIT 1",
		cacheKey,
	)
	s, err := scanInsightRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"getting cached insight: %w", err,
		)
	}
	return &s, nil
}

const maxInsights = 500

// ListInsights returns insights matching the filter,
// ordered by created_at DESC, capped at 500 rows.
func (db *DB) ListInsights(
	ctx context.Context, f InsightFilter,
) ([]Insight, error) {
	where, args := buildInsightFilter(f)
	query := "SELECT " + insightBaseCols +
		" FROM insights WHERE " + where +
		" ORDER BY created_at DESC, id DESC" +
		" LIMIT " + fmt.Sprintf("%d", maxInsights)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying insights: %w", err)
	}
	defer rows.Close()

	var insights []Insight
	for rows.Next() {
		s, err := scanInsightRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning insight: %w", err)
		}
		insights = append(insights, s)
	}
	return insights, rows.Err()
}

// GetInsight returns a single insight by ID.
// Returns nil, nil if not found.
func (db *DB) GetInsight(
	ctx context.Context, id int64,
) (*Insight, error) {
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+insightBaseCols+
			" FROM insights WHERE id = ?",
		id,
	)
	s, err := scanInsightRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"getting insight %d: %w", id, err,
		)
	}
	return &s, nil
}

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

// DeleteInsight removes an insight by ID.
func (db *DB) DeleteInsight(id int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"DELETE FROM insights WHERE id = ?", id,
	)
	return err
}
