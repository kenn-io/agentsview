package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// InsertInsight stores a dashboard insight through the operation-scoped common
// writer and returns the engine-generated canonical ID.
func (s *BunStore) InsertInsight(insight Insight) (int64, error) {
	ctx := context.Background()
	row := insightToBunRow(insight)
	var id int64
	err := s.update(ctx, WriteInsight, func(store bun.IDB) error {
		return store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := tx.NewInsert().Model(&row).
				ExcludeColumn("id").Value("created_at", "current_timestamp").
				Returning("id").
				Scan(ctx, &id); err != nil {
				return fmt.Errorf("inserting insight: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// DeleteInsight removes a dashboard insight by canonical ID.
func (s *BunStore) DeleteInsight(id int64) error {
	ctx := context.Background()
	return s.update(ctx, WriteInsightDelete, func(store bun.IDB) error {
		if _, err := store.NewDelete().Model((*bunmodel.Insight)(nil)).
			Where("id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("deleting insight %d: %w", id, err)
		}
		return nil
	})
}

// ListInsights returns the newest canonical insights matching the filter.
func (s *BunStore) ListInsights(
	ctx context.Context, filter InsightFilter,
) ([]Insight, error) {
	var rows []bunmodel.Insight
	err := s.view(ctx, func(store bun.IDB) error {
		query := applyBunInsightFilter(store.NewSelect().Model(&rows), filter)
		return query.OrderExpr(s.bunInsightTimeOrder() + " DESC").OrderExpr("id DESC").
			Limit(maxInsights).Scan(ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("querying insights: %w", err)
	}
	insights := make([]Insight, len(rows))
	for i, row := range rows {
		insights[i] = insightFromBunRow(row)
	}
	return insights, nil
}

// GetInsight returns one insight by ID or nil when it does not exist.
func (s *BunStore) GetInsight(
	ctx context.Context, id int64,
) (*Insight, error) {
	var row bunmodel.Insight
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting insight %d: %w", id, err)
	}
	insight := insightFromBunRow(row)
	return &insight, nil
}

// GetCachedInsight returns the newest insight for a non-empty cache key.
func (s *BunStore) GetCachedInsight(
	ctx context.Context, cacheKey string,
) (*Insight, error) {
	if strings.TrimSpace(cacheKey) == "" {
		return nil, nil
	}
	var row bunmodel.Insight
	err := s.view(ctx, func(store bun.IDB) error {
		return store.NewSelect().Model(&row).Where("cache_key = ?", cacheKey).
			OrderExpr(s.bunInsightTimeOrder() + " DESC").OrderExpr("id DESC").
			Limit(1).Scan(ctx)
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting cached insight: %w", err)
	}
	insight := insightFromBunRow(row)
	return &insight, nil
}

func (s *BunStore) bunInsightTimeOrder() string {
	dialect := s.backend.SessionQueryDialect()
	if dialect.timestampOrderExpr != nil {
		return dialect.timestampOrderExpr("created_at")
	}
	return "created_at"
}

func applyBunInsightFilter(
	query *bun.SelectQuery, filter InsightFilter,
) *bun.SelectQuery {
	if filter.Type != "" {
		query = query.Where("type = ?", filter.Type)
	}
	if filter.GlobalOnly {
		query = query.Where("project IS NULL")
	} else if filter.Project != "" {
		query = query.Where("project = ?", filter.Project)
	}
	if filter.DateFrom != "" {
		query = query.Where("date_from >= ?", filter.DateFrom)
	}
	if filter.DateTo != "" {
		query = query.Where("date_to <= ?", filter.DateTo)
	}
	return query
}

func insightToBunRow(insight Insight) bunmodel.Insight {
	return bunmodel.Insight{
		ID: insight.ID, Type: insight.Type, DateFrom: insight.DateFrom,
		DateTo: insight.DateTo, Project: insight.Project, Agent: insight.Agent,
		Model: insight.Model, Prompt: insight.Prompt, Content: insight.Content,
		Kind: insight.Kind, SchemaVersion: insight.SchemaVersion,
		TemplateID: insight.TemplateID, TemplateVersion: insight.TemplateVersion,
		AggregateHash: insight.AggregateHash, CacheKey: insight.CacheKey,
		CacheStatus: insight.CacheStatus, ProvenanceJSON: insight.ProvenanceJSON,
		StructuredJSON: insight.StructuredJSON,
	}
}

func insightFromBunRow(row bunmodel.Insight) Insight {
	return Insight{
		ID: row.ID, Type: row.Type, DateFrom: row.DateFrom, DateTo: row.DateTo,
		Project: row.Project, Agent: row.Agent, Model: row.Model, Prompt: row.Prompt,
		Content: row.Content, Kind: row.Kind, SchemaVersion: row.SchemaVersion,
		TemplateID: row.TemplateID, TemplateVersion: row.TemplateVersion,
		AggregateHash: row.AggregateHash, CacheKey: row.CacheKey,
		CacheStatus: row.CacheStatus, ProvenanceJSON: row.ProvenanceJSON,
		StructuredJSON: row.StructuredJSON,
		CreatedAt:      formatBunCurationTime(row.CreatedAt.Time),
	}
}
