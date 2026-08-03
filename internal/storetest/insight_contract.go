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
	assert.Equal(t, "2026-08-03T12:00:00Z", insight.CreatedAt)
}

func InsertBunInsightFixture(
	ctx context.Context, store bun.IDB,
) (InsightFixture, error) {
	project := "insight-project"
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	row := bunmodel.Insight{
		ID: 4101, Type: "daily_activity", DateFrom: "2026-08-03",
		DateTo: "2026-08-03", Project: &project, Agent: "claude",
		Content: "seeded common insight", CacheKey: insightContractCacheKey,
		CacheStatus: "fresh", CreatedAt: created,
	}
	if _, err := store.NewInsert().Model(&row).Exec(ctx); err != nil {
		return InsightFixture{}, fmt.Errorf("inserting Bun insight fixture: %w", err)
	}
	return InsightFixture{ID: row.ID, Project: project}, nil
}

func InsertSQLiteInsightFixture(
	ctx context.Context, tx *sql.Tx,
) (InsightFixture, error) {
	project := "insight-project"
	const id int64 = 4101
	_, err := tx.ExecContext(ctx, `
		INSERT INTO insights (
			id, type, date_from, date_to, project, agent, content,
			cache_key, cache_status, created_at
		) VALUES (?, 'daily_activity', '2026-08-03', '2026-08-03', ?,
			'claude', 'seeded common insight', ?, 'fresh', ?)`,
		id, project, insightContractCacheKey, "2026-08-03T12:00:00Z",
	)
	if err != nil {
		return InsightFixture{}, fmt.Errorf("inserting SQLite insight fixture: %w", err)
	}
	return InsightFixture{ID: id, Project: project}, nil
}
