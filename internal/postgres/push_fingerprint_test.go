package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPushSessionMessageComparisonsNoSessions(t *testing.T) {
	comparisons, err := readPushSessionMessageComparisons(
		context.Background(), nil, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, comparisons)
	assert.Empty(t, comparisons.MessageAggregates)
	assert.Empty(t, comparisons.MessageContentHash)
	assert.Empty(t, comparisons.MessageRoleTime)
	assert.Empty(t, comparisons.MessageFlags)
	assert.Empty(t, comparisons.MessageSystemOrdinals)
	assert.Empty(t, comparisons.MessageTokenFingerprint)
	assert.Empty(t, comparisons.ToolCallAggregates)
	assert.Empty(t, comparisons.ToolCallFingerprint)
	assert.Empty(t, comparisons.UsageEventFingerprint)
}

func TestShouldSkipSessionMessagesGuardsCountAndNilMaps(t *testing.T) {
	comparisons := &pushMessageComparison{
		MessageAggregates: map[string]pushMessageAggregate{
			"sess": {Count: 1, Sum: 1, Max: 1, Min: 1},
		},
		MessageContentHash:      map[string]string{"sess": ""},
		MessageRoleTime:         map[string]string{"sess": ""},
		MessageFlags:            map[string]string{"sess": ""},
		MessageSystemOrdinals:   map[string]string{"sess": ""},
		MessageTokenFingerprint: map[string]string{"sess": ""},
		ToolCallAggregates:      map[string]pushToolCallAggregate{"sess": {}},
		ToolCallFingerprint:     map[string]string{"sess": ""},
		UsageEventFingerprint:   map[string]string{"sess": ""},
	}
	localFP := pushLocalMessageFingerprint{Sum: 1, Max: 1, Min: 1}
	assert.False(t, shouldSkipSessionMessages(
		"sess", 1, localFP, false, nil,
	))
	assert.True(t, shouldSkipSessionMessages(
		"sess", 1, localFP, false, comparisons,
	))
	assert.False(t, shouldSkipSessionMessages(
		"sess", 2, localFP, false, comparisons,
	))
}
