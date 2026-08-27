package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenAIPricingPreservesUpstreamJSON(t *testing.T) {
	database := testDB(t)
	want := GenAIPricingDocument{
		Version:   "genai-prices-test",
		SourceRef: "upstream-ref",
		Source:    GenAIPricingSourceFetched,
		Data: []byte(`[
  {"id":"provider","unknown_future_field":{"nested":[1,2,3]}}
]`),
	}

	require.NoError(t, database.UpsertGenAIPricing(context.Background(), want))
	got, err := database.GetGenAIPricing(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, want.Version, got.Version)
	assert.Equal(t, want.SourceRef, got.SourceRef)
	assert.Equal(t, want.Source, got.Source)
	assert.Equal(t, want.Data, got.Data,
		"unknown fields and upstream formatting must survive storage")
	assert.NotEmpty(t, got.UpdatedAt)
}

func TestInsertMissingGenAIPricingRefreshesEmbeddedDocument(t *testing.T) {
	initial := GenAIPricingDocument{
		Version:   "old-version",
		SourceRef: "old-ref",
		Source:    GenAIPricingSourceEmbedded,
		Data:      []byte(`[{"id":"old"}]`),
	}
	tests := []struct {
		name string
		next GenAIPricingDocument
	}{
		{
			name: "version changed",
			next: GenAIPricingDocument{
				Version: "new-version", SourceRef: "old-ref",
				Source: GenAIPricingSourceEmbedded, Data: []byte(`[{"id":"old"}]`),
			},
		},
		{
			name: "source ref changed",
			next: GenAIPricingDocument{
				Version: "old-version", SourceRef: "new-ref",
				Source: GenAIPricingSourceEmbedded, Data: []byte(`[{"id":"old"}]`),
			},
		},
		{
			name: "data changed",
			next: GenAIPricingDocument{
				Version: "old-version", SourceRef: "old-ref",
				Source: GenAIPricingSourceEmbedded, Data: []byte(`[{"id":"new"}]`),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			require.NoError(t, database.InsertMissingGenAIPricing(
				context.Background(), initial,
			))
			require.NoError(t, database.InsertMissingGenAIPricing(
				context.Background(), tt.next,
			))

			got, err := database.GetGenAIPricing(context.Background())
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.next.Version, got.Version)
			assert.Equal(t, tt.next.SourceRef, got.SourceRef)
			assert.Equal(t, tt.next.Source, got.Source)
			assert.Equal(t, tt.next.Data, got.Data)
		})
	}
}

func TestInsertMissingGenAIPricingPreservesFetchedDocument(t *testing.T) {
	database := testDB(t)
	fetched := GenAIPricingDocument{
		Version:   "fetched-version",
		SourceRef: "fetched-ref",
		Source:    GenAIPricingSourceFetched,
		Data:      []byte(`[{"id":"fetched"}]`),
	}
	require.NoError(t, database.UpsertGenAIPricing(
		context.Background(), fetched,
	))
	require.NoError(t, database.InsertMissingGenAIPricing(
		context.Background(), GenAIPricingDocument{
			Version:   "embedded-version",
			SourceRef: "embedded-ref",
			Source:    GenAIPricingSourceEmbedded,
			Data:      []byte(`[{"id":"embedded"}]`),
		},
	))

	got, err := database.GetGenAIPricing(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, fetched.Version, got.Version)
	assert.Equal(t, fetched.SourceRef, got.SourceRef)
	assert.Equal(t, fetched.Source, got.Source)
	assert.Equal(t, fetched.Data, got.Data)
}
