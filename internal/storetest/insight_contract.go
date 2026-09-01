package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const insightContractCacheKey = "bun-insight-cache"

// InsightStore is the Task 6 shared insight method family.
type InsightStore interface {
	ListInsights(context.Context, db.InsightFilter) ([]db.Insight, error)
	GetInsight(context.Context, int64) (*db.Insight, error)
	GetCachedInsight(context.Context, string) (*db.Insight, error)
	InsertInsight(db.Insight) (int64, error)
	DeleteInsight(int64) error
}

type InsightFixture struct {
	ID      int64
	Project string
}

type InsightBackend struct {
	Name     string
	Open     func(*testing.T) (InsightStore, InsightFixture)
	Writable bool
}

// RunInsightContract verifies identical canonical insight reads and the
// operation-scoped write policy on every embedded BunStore.
func RunInsightContract(t *testing.T, backend InsightBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store, fixture := backend.Open(t)
		ctx := t.Context()

		listed, err := store.ListInsights(ctx, db.InsightFilter{
			Type: "daily_activity", Project: fixture.Project,
			DateFrom: "2026-08-03", DateTo: "2026-08-03",
		})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assertSeededInsight(t, listed[0], fixture)

		got, err := store.GetInsight(ctx, fixture.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assertSeededInsight(t, *got, fixture)
		missing, err := store.GetInsight(ctx, 999_999)
		require.NoError(t, err)
		assert.Nil(t, missing)
		cached, err := store.GetCachedInsight(ctx, insightContractCacheKey)
		require.NoError(t, err)
		require.NotNil(t, cached)
		assert.Equal(t, fixture.ID, cached.ID)
		blank, err := store.GetCachedInsight(ctx, "   ")
		require.NoError(t, err)
		assert.Nil(t, blank)
		global, err := store.ListInsights(ctx, db.InsightFilter{
			Type: "daily_activity", GlobalOnly: true,
			DateFrom: "2026-08-03", DateTo: "2026-08-03",
		})
		require.NoError(t, err)
		require.Len(t, global, 1)
		assert.Equal(t, int64(4102), global[0].ID)
		assert.Nil(t, global[0].Project)
		ordered, err := store.ListInsights(ctx, db.InsightFilter{
			Type: "ordering", Project: fixture.Project,
		})
		require.NoError(t, err)
		require.Len(t, ordered, 2)
		assert.Equal(t, []int64{4202, 4201}, []int64{ordered[0].ID, ordered[1].ID})
		empty, err := store.ListInsights(ctx, db.InsightFilter{Type: "missing-type"})
		require.NoError(t, err)
		assert.Empty(t, empty)

		if !backend.Writable {
			inserted, err := store.InsertInsight(db.Insight{Content: "forbidden"})
			assert.Zero(t, inserted)
			require.ErrorIs(t, err, db.ErrReadOnly)
			require.ErrorIs(t, store.DeleteInsight(fixture.ID), db.ErrReadOnly)
			stillThere, readErr := store.GetInsight(ctx, fixture.ID)
			require.NoError(t, readErr)
			require.NotNil(t, stillThere)
			return
		}

		insertedID, err := store.InsertInsight(db.Insight{
			Type: "daily_activity", DateFrom: "2026-08-03", DateTo: "2026-08-03",
			Project: &fixture.Project, Agent: "codex", Content: "new common insight",
			CacheKey: insightContractCacheKey, CacheStatus: "hit",
		})
		require.NoError(t, err)
		assert.Positive(t, insertedID)
		assert.NotEqual(t, fixture.ID, insertedID)
		cached, err = store.GetCachedInsight(ctx, insightContractCacheKey)
		require.NoError(t, err)
		require.NotNil(t, cached)
		assert.Equal(t, insertedID, cached.ID)
		assert.Equal(t, "new common insight", cached.Content)
		require.NoError(t, store.DeleteInsight(insertedID))
		deleted, err := store.GetInsight(ctx, insertedID)
		require.NoError(t, err)
		assert.Nil(t, deleted)
	})
}

func assertSeededInsight(t *testing.T, insight db.Insight, fixture InsightFixture) {
	t.Helper()
	assert.Equal(t, fixture.ID, insight.ID)
	assert.Equal(t, "daily_activity", insight.Type)
	assert.Equal(t, "seeded common insight", insight.Content)
	require.NotNil(t, insight.Project)
	assert.Equal(t, fixture.Project, *insight.Project)
	assert.Equal(t, insightContractCacheKey, insight.CacheKey)
	require.NotNil(t, insight.Model)
	assert.Equal(t, "fixture-model", *insight.Model)
	require.NotNil(t, insight.Prompt)
	assert.Equal(t, "fixture prompt", *insight.Prompt)
	assert.Equal(t, "report", insight.Kind)
	assert.Equal(t, "v1", insight.SchemaVersion)
	assert.JSONEq(t, `{"source":"fixture"}`, insight.ProvenanceJSON)
	assert.JSONEq(t, `{"summary":"literal"}`, insight.StructuredJSON)
	assert.Equal(t, "2000-01-01T00:00:00Z", insight.CreatedAt)
}

func InsertBunInsightFixture(
	ctx context.Context, store bun.IDB,
) (InsightFixture, error) {
	project := "insight-project"
	model := "fixture-model"
	prompt := "fixture prompt"
	created := bunmodel.NewTimestamp(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	rows := []bunmodel.Insight{
		{
			ID: 4101, Type: "daily_activity", DateFrom: "2026-08-03",
			DateTo: "2026-08-03", Project: &project, Agent: "claude",
			Model: &model, Prompt: &prompt, Content: "seeded common insight",
			Kind: "report", SchemaVersion: "v1", TemplateID: "daily",
			TemplateVersion: "1", AggregateHash: "fixture-hash",
			CacheKey: insightContractCacheKey, CacheStatus: "fresh",
			ProvenanceJSON: `{"source":"fixture"}`,
			StructuredJSON: `{"summary":"literal"}`, CreatedAt: created,
		},
		{
			ID: 4102, Type: "daily_activity", DateFrom: "2026-08-03",
			DateTo: "2026-08-03", Agent: "codex", Content: "global insight",
			CreatedAt: created,
		},
		{
			ID: 4201, Type: "ordering", DateFrom: "2026-08-01",
			DateTo: "2026-08-03", Project: &project, Agent: "codex",
			Content: "ordering lower id", CreatedAt: created,
		},
		{
			ID: 4202, Type: "ordering", DateFrom: "2026-08-01",
			DateTo: "2026-08-03", Project: &project, Agent: "codex",
			Content: "ordering higher id", CreatedAt: created,
		},
	}
	if _, err := store.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return InsightFixture{}, fmt.Errorf("inserting Bun insight fixture: %w", err)
	}
	return InsightFixture{ID: rows[0].ID, Project: project}, nil
}

func InsertSQLiteInsightFixture(
	ctx context.Context, tx *sql.Tx,
) (InsightFixture, error) {
	project := "insight-project"
	const id int64 = 4101
	_, err := tx.ExecContext(ctx, `
		INSERT INTO insights (
			id, type, date_from, date_to, project, agent, content,
			model, prompt, kind, schema_version, template_id, template_version,
			aggregate_hash, cache_key, cache_status, provenance_json,
			structured_json, created_at
		) VALUES (?, 'daily_activity', '2026-08-03', '2026-08-03', ?, 'claude',
			'seeded common insight', 'fixture-model', 'fixture prompt', 'report',
			'v1', 'daily', '1', 'fixture-hash', ?, 'fresh',
			'{"source":"fixture"}', '{"summary":"literal"}', ?)`,
		id, project, insightContractCacheKey, "2000-01-01T00:00:00Z",
	)
	if err != nil {
		return InsightFixture{}, fmt.Errorf("inserting SQLite insight fixture: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO insights (
			id, type, date_from, date_to, agent, content, created_at
		) VALUES (4102, 'daily_activity', '2026-08-03', '2026-08-03',
			'codex', 'global insight', ?)`,
		"2000-01-01T00:00:00Z",
	)
	if err != nil {
		return InsightFixture{}, fmt.Errorf("inserting SQLite global insight fixture: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO insights (
			id, type, date_from, date_to, project, agent, content, created_at
		) VALUES
			(4201, 'ordering', '2026-08-01', '2026-08-03', ?, 'codex',
				'ordering lower id', ?),
			(4202, 'ordering', '2026-08-01', '2026-08-03', ?, 'codex',
				'ordering higher id', ?)`,
		project, "2000-01-01T00:00:00Z", project, "2000-01-01T00:00:00Z",
	)
	if err != nil {
		return InsightFixture{}, fmt.Errorf("inserting SQLite ordered insight fixture: %w", err)
	}
	return InsightFixture{ID: id, Project: project}, nil
}
