package postgres

import (
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestPGSearchEntryBytesIncludesAllocations(t *testing.T) {
	now := time.Now()
	source, processor := "source", "processor"
	call, event, image := 1, 2, 3
	keys := make([]pgSearchKey, 1, 4)
	keys[0] = pgSearchKey{id: "session", kind: "image", endedAt: &now, sourceKind: &source, processor: &processor, callIndex: &call, eventIndex: &event, imageIndex: &image}
	entry := pgSearchEntry{token: "token", keys: keys}
	minimum := int64(unsafe.Sizeof(entry)) + int64(cap(keys))*int64(unsafe.Sizeof(keys[0])) + int64(unsafe.Sizeof(now)) + 3*int64(unsafe.Sizeof(call)) + 2*int64(unsafe.Sizeof(source)) + int64(len(entry.token)+len(keys[0].id)+len(keys[0].kind)+len(source)+len(processor))
	require.GreaterOrEqual(t, pgSearchEntryBytes(entry), minimum)
}
