package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPGUsageRowCacheCreation1hTokens(t *testing.T) {
	tests := []struct {
		name        string
		usageSource string
		tokenJSON   string
		cacheCrTok  int
		want        int
	}{
		{
			name:        "issue 1452 sample: all writes are 1h",
			usageSource: "message",
			tokenJSON: `{"input_tokens":2,"output_tokens":62,` +
				`"cache_creation_input_tokens":8989,` +
				`"cache_read_input_tokens":15892,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":8989,` +
				`"ephemeral_5m_input_tokens":0}}`,
			cacheCrTok: 8989,
			want:       8989,
		},
		{
			name:        "mixed TTLs keep only the 1h subset",
			usageSource: "message",
			tokenJSON: `{"cache_creation_input_tokens":250,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":150,` +
				`"ephemeral_1h_input_tokens":100}}`,
			cacheCrTok: 250,
			want:       100,
		},
		{
			name:        "subset clamps to the flat total",
			usageSource: "message",
			tokenJSON: `{"cache_creation_input_tokens":250,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":500}}`,
			cacheCrTok: 250,
			want:       250,
		},
		{
			name:        "no nested breakdown",
			usageSource: "message",
			tokenJSON:   `{"cache_creation_input_tokens":250}`,
			cacheCrTok:  250,
			want:        0,
		},
		{
			name:        "usage events never carry the breakdown",
			usageSource: "session",
			tokenJSON:   "",
			cacheCrTok:  250,
			want:        0,
		},
		{
			name:        "negative counts read as zero",
			usageSource: "message",
			tokenJSON: `{"cache_creation_input_tokens":250,` +
				`"cache_creation":{"ephemeral_1h_input_tokens":-3}}`,
			cacheCrTok: 250,
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pgUsageRowCacheCreation1hTokens(
				tt.usageSource, tt.tokenJSON, tt.cacheCrTok))
		})
	}
}
