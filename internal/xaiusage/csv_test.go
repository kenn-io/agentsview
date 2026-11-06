package xaiusage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleCSV = `Billing Item ID,Event Time,Product,Charge Category,Usage Type,Principal Type,Principal ID,Usage,Pricing Currency,Pricing Contracted,Model,User Email
item-a,2026-11-05T00:30:00Z,grok-build,usage,included,user,user-a,"{""Cached prompt text tokens"":""128"",""Completion text tokens"":""7"",""Prompt text tokens"":""12""}",USD,0.0000000000,,member@example.com
item-b,2026-11-04T08:00:00-05:00,grok-bot,usage,overage,user,user-a,"{""cache-write-text-tokens"":""32"",""cached-prompt-text-tokens"":""64"",""completion-text-tokens"":""5"",""prompt-text-tokens"":""11""}",USD,0.0123456789,,member@example.com
item-c,2026-11-04T13:00:00Z,cursor,usage,included,user,user-a,"{""completion-text-tokens"":""3"",""prompt-text-tokens"":""9""}",USD,0,grok-4.7-high-fast,member@example.com
`

func TestReadCSVPreservesBillingUsage(t *testing.T) {
	events, err := ReadCSV(strings.NewReader(sampleCSV))
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, "grok", events[0].Agent)
	assert.Empty(t, events[0].Model, "the export does not identify this model")
	assert.Equal(t, "2026-11-05T00:30:00Z", events[0].OccurredAt)
	assert.Equal(t, 12, events[0].InputTokens)
	assert.Equal(t, 128, events[0].CacheReadTokens)
	assert.Equal(t, 7, events[0].OutputTokens)
	assert.Zero(t, events[0].Charged.Microdollars)
	assert.Equal(t, "xai:item-a", events[0].DedupKey)
	assert.Equal(t, "grok-build:usage:included", events[0].Kind)
	assert.Equal(t, "grok-bot", events[1].Agent)
	assert.Equal(t, "2026-11-04T13:00:00Z", events[1].OccurredAt)
	assert.Equal(t, 32, events[1].CacheWriteTokens)
	assert.Equal(t, int64(12346), events[1].Charged.Microdollars)
	assert.Equal(t, "cursor", events[2].Agent)
	assert.Equal(t, "grok-4.7-high-fast", events[2].Model)
	assert.Equal(t, "user-a", events[2].UserID)
}

func TestReadCSVRejectsInvalidRowsWithoutPartialImport(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"missing identity", "item-b,", ","},
		{"invalid timestamp", "2026-11-04T08:00:00-05:00", "yesterday"},
		{"unsupported currency", ",USD,", ",EUR,"},
		{"unsupported product", ",grok-bot,", ",unknown-product,"},
		{"negative tokens", `""11""`, `""-11""`},
		{"invalid usage JSON", `""cache-write-text-tokens""`, "not-json"},
		{"invalid charge", "0.0123456789", "not-money"},
		{"unknown token counter", "cache-write-text-tokens", "image-tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := ReadCSV(strings.NewReader(strings.Replace(sampleCSV, tc.old, tc.replacement, 1)))
			require.Error(t, err)
			assert.Empty(t, events)
		})
	}
	_, err := ReadCSV(strings.NewReader("Event Time,Product\n2026-11-04T13:00:00Z,grok-build\n"))
	require.Error(t, err)
}
