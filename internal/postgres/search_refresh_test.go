package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

func TestPGSearchEntryByteLimitCountsAllocatedKeys(t *testing.T) {
	now := time.Now()
	source, processor := "source", "processor"
	call, event, image := 1, 2, 3
	keys := make([]pgSearchKey, 1, 4)
	keys[0] = pgSearchKey{id: "session", kind: "image", endedAt: &now, sourceKind: &source, processor: &processor, callIndex: &call, eventIndex: &event, imageIndex: &image}
	memo := kept.Memo[pgSearchEntry]{Size: pgSearchEntryBytes, MaxBytes: 500}
	memo.Put("search", "v", []pgSearchEntry{{token: "token", keys: keys}})
	_, ok := memo.Get("search", "v")
	require.False(t, ok)
	keys = append([]pgSearchKey(nil), keys...)
	memo.Put("search", "v", []pgSearchEntry{{token: "token", keys: keys}})
	_, ok = memo.Get("search", "v")
	require.True(t, ok)
}
