package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenAIPricingPreservesUpstreamJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	want := GenAIPricingDocument{
		Version:   "genai-prices-test",
		SourceRef: "upstream-ref",
		Source:    GenAIPricingSourceFetched,
		Data: []byte(`[
  {"id":"provider","unknown_future_field":{"nested":[1,2,3]}}
]`),
	}

	require.NoError(database.UpsertGenAIPricing(t.Context(), want))
	got, err := database.GetGenAIPricing(t.Context())
	require.NoError(err)
	require.NotNil(got)

	assert.Equal(want.Version, got.Version)
	assert.Equal(want.SourceRef, got.SourceRef)
	assert.Equal(want.Source, got.Source)
	assert.Equal(want.Data, got.Data,
		"unknown fields and upstream formatting must survive storage")
	assert.NotEmpty(got.UpdatedAt)
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
			assert := assert.New(t)
			require := require.New(t)

			database := testDB(t)
			require.NoError(database.InsertMissingGenAIPricing(
				t.Context(), initial,
			))
			require.NoError(database.InsertMissingGenAIPricing(
				t.Context(), tt.next,
			))

			got, err := database.GetGenAIPricing(t.Context())
			require.NoError(err)
			require.NotNil(got)
			assert.Equal(tt.next.Version, got.Version)
			assert.Equal(tt.next.SourceRef, got.SourceRef)
			assert.Equal(tt.next.Source, got.Source)
			assert.Equal(tt.next.Data, got.Data)
		})
	}
}

func TestInsertMissingGenAIPricingPreservesFetchedDocument(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	fetched := GenAIPricingDocument{
		Version:   "fetched-version",
		SourceRef: "fetched-ref",
		Source:    GenAIPricingSourceFetched,
		Data:      []byte(`[{"id":"fetched"}]`),
	}
	require.NoError(database.UpsertGenAIPricing(
		t.Context(), fetched,
	))
	require.NoError(database.InsertMissingGenAIPricing(
		t.Context(), GenAIPricingDocument{
			Version:   "embedded-version",
			SourceRef: "embedded-ref",
			Source:    GenAIPricingSourceEmbedded,
			Data:      []byte(`[{"id":"embedded"}]`),
		},
	))

	got, err := database.GetGenAIPricing(t.Context())
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(fetched.Version, got.Version)
	assert.Equal(fetched.SourceRef, got.SourceRef)
	assert.Equal(fetched.Source, got.Source)
	assert.Equal(fetched.Data, got.Data)
}
