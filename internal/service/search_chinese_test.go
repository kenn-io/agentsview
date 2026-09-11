package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// TestDirectSearchSegmentsChineseQuery pins that the service search path hands
// the user's query to the store untouched. Pre-applying db.PrepareFTSQuery
// here quoted every Chinese query, the store read that leading quote as the
// opt-in for an explicit FTS5 expression, and word segmentation was skipped:
// a query then only matched its characters run together verbatim.
func TestDirectSearchSegmentsChineseQuery(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	id := "cn1"
	dbtest.SeedSessionWithMessages(t, d, id, "proj", []db.Message{
		dbtest.UserMsg(id, 0, "这是全文的搜索实现说明。"),
		dbtest.AsstMsg(id, 1, "understood"),
	}, dbtest.WithMessageCounts(3, 2))
	if !d.HasChineseFTS() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	be := service.NewDirectBackend(d, nil)

	// jieba segments 全文搜索 into 全文 AND 搜索, both of which the message
	// contains. As the literal phrase it never matches, because the message
	// separates the two words.
	res, err := be.Search(context.Background(), service.SearchRequest{
		Query: "全文搜索",
		Limit: 20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Results, "segmented Chinese query should match")
	assert.Equal(t, id, res.Results[0].SessionID)
}
