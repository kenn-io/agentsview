//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreInsightCRUD(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	project := "insight-project"
	cacheKey := "insight-cache-key"

	firstID, err := store.InsertInsight(db.Insight{
		Type:        "daily_activity",
		DateFrom:    "2026-03-12",
		DateTo:      "2026-03-12",
		Project:     &project,
		Agent:       "claude",
		Content:     "first insight",
		CacheKey:    cacheKey,
		CacheStatus: "fresh",
	})
	require.NoError(t, err, "InsertInsight first")
	require.NotZero(t, firstID)

	time.Sleep(10 * time.Millisecond)

	secondID, err := store.InsertInsight(db.Insight{
		Type:        "daily_activity",
		DateFrom:    "2026-03-12",
		DateTo:      "2026-03-12",
		Project:     &project,
		Agent:       "claude",
		Content:     "second insight",
		CacheKey:    cacheKey,
		CacheStatus: "hit",
	})
	require.NoError(t, err, "InsertInsight second")
	require.NotZero(t, secondID)
	assert.NotEqual(t, firstID, secondID)

	listed, err := store.ListInsights(ctx, db.InsightFilter{
		Type:    "daily_activity",
		Project: project,
	})
	require.NoError(t, err, "ListInsights")
	require.Len(t, listed, 2)
	assert.Equal(t, secondID, listed[0].ID)
	assert.Equal(t, firstID, listed[1].ID)

	got, err := store.GetInsight(ctx, firstID)
	require.NoError(t, err, "GetInsight")
	require.NotNil(t, got)
	assert.Equal(t, "first insight", got.Content)
	require.NotNil(t, got.Project)
	assert.Equal(t, project, *got.Project)

	cached, err := store.GetCachedInsight(ctx, cacheKey)
	require.NoError(t, err, "GetCachedInsight")
	require.NotNil(t, cached)
	assert.Equal(t, secondID, cached.ID)
	assert.Equal(t, "hit", cached.CacheStatus)

	require.NoError(t, store.DeleteInsight(firstID), "DeleteInsight first")
	got, err = store.GetInsight(ctx, firstID)
	require.NoError(t, err, "GetInsight after delete")
	assert.Nil(t, got)
}
