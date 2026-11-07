package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVectorFlushKeysLimitFollowsTransport pins that a flush splits its keys
// for an HTTP mirror, whose server limits each URL parameter, and not for a
// native one, where every extra range only costs another pair of DELETEs.
func TestVectorFlushKeysLimitFollowsTransport(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want int
	}{
		{url: "http://localhost:8123/agentsview", want: 96 << 10},
		{url: "https://mirror.example.com:8443/agentsview", want: 96 << 10},
		{url: "clickhouse://localhost:9000/agentsview", want: 0},
		{url: "clickhouse://mirror.example.com:9440/agentsview?secure=true", want: 0},
	} {
		t.Run(tc.url, func(t *testing.T) {
			s := &Sync{target: Target{URL: tc.url}}
			got, err := s.vectorFlushKeysLimit()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
