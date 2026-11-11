package parser

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

func TestSetRecordedCostKeepsTokenCounters(t *testing.T) {
	tests := []struct {
		name  string
		usage string
		cost  int64
		want  usagefacts.ParsedTokenUsage
	}{
		{
			name:  "empty usage",
			usage: "",
			cost:  1_250,
			want:  usagefacts.ParsedTokenUsage{},
		},
		{
			name:  "existing counters",
			usage: `{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":3}`,
			cost:  0,
			want: usagefacts.ParsedTokenUsage{
				InputTokens: 10, OutputTokens: 4, CacheReadTokens: 3,
			},
		},
		{
			name:  "replaces an earlier cost",
			usage: `{"recorded_cost_microdollars":99,"output_tokens":7}`,
			cost:  42,
			want:  usagefacts.ParsedTokenUsage{OutputTokens: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := ParsedMessage{TokenUsage: jsontext.Value(tt.usage)}
			msg.SetRecordedCost(money.Money{Microdollars: tt.cost})

			require.True(t, msg.TokenUsage.IsValid(), "usage %s", msg.TokenUsage)
			got := usagefacts.ParseTokenUsage(string(msg.TokenUsage))
			require.NotNil(t, got.RecordedCostMicrodollars)
			assert.Equal(t, tt.cost, *got.RecordedCostMicrodollars)
			got.RecordedCostMicrodollars = nil
			assert.Equal(t, tt.want, got)
			cost, ok := msg.RecordedCost()
			assert.True(t, ok)
			assert.Equal(t, money.Money{Microdollars: tt.cost}, cost)
		})
	}
}

func TestSetRecordedCostIgnoresNegativeCost(t *testing.T) {
	msg := ParsedMessage{TokenUsage: jsontext.Value(`{"output_tokens":1}`)}
	msg.SetRecordedCost(money.Money{Microdollars: -1})
	assert.JSONEq(t, `{"output_tokens":1}`, string(msg.TokenUsage))
	_, ok := msg.RecordedCost()
	assert.False(t, ok)
}
