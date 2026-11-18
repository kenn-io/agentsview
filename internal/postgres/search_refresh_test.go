package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

func TestPGSearchEntryByteLimitCountsAllocatedKeys(t *testing.T) {
	now := time.Now()
	source, Processor := "source", "processor"
	call, event, image := 1, 2, 3
	keys := make([]pgSearchKey, 1, 4)
	keys[0] = pgSearchKey{ID: "session", Kind: "image", EndedAt: &now, SourceKind: &source, Processor: &Processor, CallIndex: &call, EventIndex: &event, ImageIndex: &image}
	memo := kept.Memo[pgSearchEntry]{Size: pgSearchEntryBytes, MaxBytes: 500}
	memo.Put("search", "v", []pgSearchEntry{{Token: "token", Keys: keys}})
	_, ok := memo.Get("search", "v")
	require.False(t, ok)
	keys = append([]pgSearchKey(nil), keys...)
	memo.Put("search", "v", []pgSearchEntry{{Token: "token", Keys: keys}})
	_, ok = memo.Get("search", "v")
	require.True(t, ok)
}
