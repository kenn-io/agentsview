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
