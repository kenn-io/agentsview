//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestStoreSearchILIKE(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) == 0 {
		t.Error("expected at least 1 search result")
	}
	for _, r := range page.Results {
		if r.SessionID != "store-test-001" {
			t.Errorf("unexpected session %q", r.SessionID)
		}
	}
}
