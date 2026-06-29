package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsights_InsertAndGet(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	want := &Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-15",
		DateTo:   "2025-01-15",
		Project:  new("my-app"),
		Agent:    "claude",
		Model:    new("claude-sonnet-4-20250514"),
		Prompt:   new("What happened today?"),
		Content:  "# Summary\nStuff happened.",
	}

	id, err := d.InsertInsight(*want)
	require.NoError(t, err, "InsertInsight")
	require.Positive(t, id, "expected positive ID")

	got, err := d.GetInsight(ctx, id)
	require.NoError(t, err, "GetInsight")
	require.NotNil(t, got, "expected insight")

	diff := cmp.Diff(want, got, cmpopts.IgnoreFields(Insight{}, "ID", "CreatedAt"))
	assert.Empty(t, diff, "Insight mismatch (-want +got)")
	assert.NotEmpty(t, got.CreatedAt, "expected created_at to be set")
}

func TestInsights_CannedMetadataAndCacheLookup(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	id, err := d.InsertInsight(Insight{
		Type:            "llm_canned",
		DateFrom:        "2025-01-15",
		DateTo:          "2025-01-15",
		Agent:           "claude",
		Model:           new("test-model"),
		Content:         "generated recommendation",
		Kind:            "prompt_maturity_review",
		SchemaVersion:   "llm_insight.v1",
		TemplateID:      "prompt_maturity_review",
		TemplateVersion: "2026-05-26",
		AggregateHash:   "abc123",
		CacheKey:        "prompt_maturity_review:abc123",
		CacheStatus:     "fresh",
		ProvenanceJSON:  `{"template_id":"prompt_maturity_review"}`,
		StructuredJSON:  `{"schema_version":"llm_insight.v1"}`,
	})
	if err != nil {
		t.Fatalf("InsertInsight: %v", err)
	}

	got, err := d.GetCachedInsight(ctx, "prompt_maturity_review:abc123")
	if err != nil {
		t.Fatalf("GetCachedInsight: %v", err)
	}
	if got == nil {
		t.Fatal("expected cached insight")
	}
	if got.ID != id || got.Kind != "prompt_maturity_review" ||
		got.SchemaVersion != "llm_insight.v1" ||
		got.ProvenanceJSON == "" || got.StructuredJSON == "" {
		t.Fatalf("cached insight metadata mismatch: %+v", got)
	}
}

func TestInsights_InsertDateRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	want := &Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-13",
		DateTo:   "2025-01-17",
		Agent:    "claude",
		Content:  "Weekly summary",
	}

	id, err := d.InsertInsight(*want)
	require.NoError(t, err, "InsertInsight")

	got, err := d.GetInsight(ctx, id)
	require.NoError(t, err, "GetInsight")
	require.NotNil(t, got, "expected insight")

	diff := cmp.Diff(want, got, cmpopts.IgnoreFields(Insight{}, "ID", "CreatedAt"))
	assert.Empty(t, diff, "Insight mismatch (-want +got)")
}

func TestInsights_GetNonexistent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	got, err := d.GetInsight(ctx, 99999)
	require.NoError(t, err, "GetInsight")
	assert.Nil(t, got, "expected nil")
}

func insertInsightFixtures(t *testing.T, d *DB, entries []Insight) []int64 {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.getWriter().Begin()
	require.NoError(t, err, "begin insights seed tx")
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO insights (
			type, date_from, date_to, project,
			agent, model, prompt, content,
			kind, schema_version, template_id,
			template_version, aggregate_hash, cache_key,
			cache_status, provenance_json, structured_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	require.NoError(t, err, "prepare insights seed")
	defer func() { _ = stmt.Close() }()

	ids := make([]int64, 0, len(entries))
	for i, s := range entries {
		res, err := stmt.Exec(
			s.Type, s.DateFrom, s.DateTo, s.Project,
			s.Agent, s.Model, s.Prompt, s.Content,
			s.Kind, s.SchemaVersion, s.TemplateID,
			s.TemplateVersion, s.AggregateHash, s.CacheKey,
			s.CacheStatus, s.ProvenanceJSON, s.StructuredJSON,
		)
		require.NoError(t, err, "insert insight fixture %d", i)
		id, err := res.LastInsertId()
		require.NoError(t, err, "fixture insight id %d", i)
		ids = append(ids, id)
	}

	require.NoError(t, tx.Commit(), "commit insights seed")
	return ids
}

func TestListInsights(t *testing.T) {
	ctx := context.Background()

	seedFiltersData := func(t *testing.T, d *DB) []int64 {
		entries := []Insight{
			{Type: "daily_activity", DateFrom: "2025-01-15", DateTo: "2025-01-15", Project: new("app-a"), Agent: "claude", Content: "Day 1 app-a"},
			{Type: "daily_activity", DateFrom: "2025-01-15", DateTo: "2025-01-15", Project: new("app-b"), Agent: "claude", Content: "Day 1 app-b"},
			{Type: "agent_analysis", DateFrom: "2025-01-15", DateTo: "2025-01-15", Agent: "claude", Content: "Analysis"},
			{Type: "daily_activity", DateFrom: "2025-01-16", DateTo: "2025-01-16", Project: new("app-a"), Agent: "claude", Content: "Day 2 app-a"},
		}
		ids := make([]int64, 0, len(entries))
		for i, insight := range entries {
			id, err := d.InsertInsight(insight)
			require.NoError(t, err, "InsertInsight %d", i)
			ids = append(ids, id)
		}
		return ids
	}

	tests := []struct {
		name   string
		seed   func(t *testing.T, d *DB) []int64
		filter InsightFilter
		verify func(t *testing.T, got []Insight, ids []int64)
	}{
		{
			name:   "AllInsights",
			seed:   seedFiltersData,
			filter: InsightFilter{},
			verify: func(t *testing.T, got []Insight, _ []int64) {
				wantContent := []string{"Day 2 app-a", "Analysis", "Day 1 app-b", "Day 1 app-a"}
				require.Len(t, got, len(wantContent))
				for i, want := range wantContent {
					assert.Equal(t, want, got[i].Content, "got[%d].Content", i)
				}
			},
		},
		{
			name:   "ByType",
			seed:   seedFiltersData,
			filter: InsightFilter{Type: "daily_activity"},
			verify: func(t *testing.T, got []Insight, _ []int64) {
				wantContent := []string{"Day 2 app-a", "Day 1 app-b", "Day 1 app-a"}
				require.Len(t, got, len(wantContent))
				for i, want := range wantContent {
					assert.Equal(t, want, got[i].Content, "got[%d].Content", i)
				}
			},
		},
		{
			name:   "ByProject",
			seed:   seedFiltersData,
			filter: InsightFilter{Project: "app-a"},
			verify: func(t *testing.T, got []Insight, _ []int64) {
				wantContent := []string{"Day 2 app-a", "Day 1 app-a"}
				require.Len(t, got, len(wantContent))
				for i, want := range wantContent {
					assert.Equal(t, want, got[i].Content, "got[%d].Content", i)
				}
			},
		},
		{
			name:   "GlobalOnly",
			seed:   seedFiltersData,
			filter: InsightFilter{GlobalOnly: true},
			verify: func(t *testing.T, got []Insight, _ []int64) {
				wantContent := []string{"Analysis"}
				require.Len(t, got, len(wantContent))
				for i, want := range wantContent {
					assert.Equal(t, want, got[i].Content, "got[%d].Content", i)
				}
			},
		},
		{
			name:   "NoMatch",
			seed:   seedFiltersData,
			filter: InsightFilter{Type: "agent_analysis", Project: "nonexistent"},
			verify: func(t *testing.T, got []Insight, _ []int64) {
				assert.Empty(t, got, "got insights")
			},
		},
		{
			name: "OrderByCreatedAtDesc",
			seed: func(t *testing.T, d *DB) []int64 {
				ids := make([]int64, 0, 3)
				for _, content := range []string{"first", "second", "third"} {
					id, err := d.InsertInsight(Insight{
						Type:     "daily_activity",
						DateFrom: "2025-01-15", DateTo: "2025-01-15",
						Agent: "claude", Content: content,
					})
					require.NoError(t, err, "InsertInsight")
					ids = append(ids, id)
				}
				return ids
			},
			filter: InsightFilter{},
			verify: func(t *testing.T, got []Insight, ids []int64) {
				require.Len(t, got, 3)
				assert.Equal(t, ids[2], got[0].ID, "first id")
				assert.Equal(t, ids[0], got[2].ID, "last id")
			},
		},
		{
			name: "CappedAt500",
			seed: func(t *testing.T, d *DB) []int64 {
				const total = 502
				entries := make([]Insight, 0, total)
				for i := range total {
					entries = append(entries, Insight{
						Type:     "daily_activity",
						DateFrom: "2025-01-15",
						DateTo:   "2025-01-15",
						Agent:    "claude",
						Content:  fmt.Sprintf("insight %d", i),
					})
				}
				return insertInsightFixtures(t, d, entries)
			},
			filter: InsightFilter{},
			verify: func(t *testing.T, got []Insight, ids []int64) {
				const total = 502
				require.Len(t, got, 500, "capped at 500")

				// Newest (id 502) should be first.
				newestID := ids[total-1]
				assert.Equal(t, newestID, got[0].ID, "first ID (newest)")
				// Oldest retained should be id 3 (skipping 1 and 2).
				oldestRetainedID := ids[total-500]
				assert.Equal(t, oldestRetainedID, got[499].ID,
					"last ID (oldest retained)")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ids := tt.seed(t, d)
			got, err := d.ListInsights(ctx, tt.filter)
			require.NoError(t, err, "ListInsights")
			tt.verify(t, got, ids)
		})
	}
}

func TestListInsights_DateFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.InsertInsight(Insight{Type: "daily_activity",
		DateFrom: "2026-06-16", DateTo: "2026-06-16", Agent: "claude", Content: "x"})
	require.NoError(t, err)
	_, err = d.InsertInsight(Insight{Type: "daily_activity",
		DateFrom: "2026-06-17", DateTo: "2026-06-17", Agent: "claude", Content: "y"})
	require.NoError(t, err)

	got, err := d.ListInsights(ctx, InsightFilter{
		Type: "daily_activity", DateFrom: "2026-06-16", DateTo: "2026-06-16"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "2026-06-16", got[0].DateFrom)
}

func TestListInsights_DistinguishesByDateTo(t *testing.T) {
	d := testDB(t)
	// A single-day insight and a week insight sharing the same date_from.
	_, err := d.InsertInsight(Insight{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Agent: "claude", Content: "day",
	})
	require.NoError(t, err)
	_, err = d.InsertInsight(Insight{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-21",
		Agent: "claude", Content: "week",
	})
	require.NoError(t, err)

	got, err := d.ListInsights(context.Background(), InsightFilter{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "filtering both bounds isolates the single-day insight")
	assert.Equal(t, "day", got[0].Content)
}

func TestInsightsLookupIndexHasDateTo(t *testing.T) {
	d := testDB(t)
	rows, err := d.Reader().Query("PRAGMA index_info(idx_insights_lookup)")
	require.NoError(t, err)
	defer rows.Close()
	var n int
	for rows.Next() {
		n++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 4, n, "lookup index covers type, date_from, date_to, project")
}

func TestInsights_Delete(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	id, err := d.InsertInsight(Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-15", DateTo: "2025-01-15",
		Agent: "claude", Content: "to be deleted",
	})
	require.NoError(t, err, "InsertInsight")

	require.NoError(t, d.DeleteInsight(id), "DeleteInsight")

	got, err := d.GetInsight(ctx, id)
	require.NoError(t, err, "GetInsight after delete")
	assert.Nil(t, got, "expected nil after delete")
}
