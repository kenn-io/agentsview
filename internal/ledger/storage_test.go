package ledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareAppendProjection(t *testing.T) {
	seg := loadFixture(t, "fixture-a-000001.json")
	prep, err := PrepareAppend("default", seg, OriginImport)
	require.NoError(t, err)
	assert.Equal(t, int64(1796932786), prep.Checksum)
	require.Len(t, prep.Events, 5)
	first, second := prep.Events[0], prep.Events[1]
	assert.Nil(t, first.Payload, "a null payload is stored as NULL")
	assert.Empty(t, first.Subsystem)
	assert.Equal(t, "fixture", second.Subsystem)
	assert.Equal(t, "floats and ints", second.Summary)
	assert.Equal(t, "state_change", second.EventClass, "storage uses the serde form")
	require.NotNil(t, second.CorrelationID)
	assert.Equal(t, "0190f5a2-7c3e-7000-8000-0000000000c1", *second.CorrelationID)
	want, err := seg.Events[1].MarshalSerde()
	require.NoError(t, err)
	assert.Equal(t, string(want), second.EventJSON)
	assert.Equal(t, "2026-01-02T03:04:08.123456Z", StorageTimestamp(prep.Events[3].Timestamp),
		"nanoseconds are truncated to PostgreSQL's microseconds")
	assert.True(t, prep.SameContent(1796932786, "2026-01-02T03:05:00Z", prep.EventsJSON))
	assert.False(t, prep.SameContent(1796932786, "2026-01-02T03:05:01Z", prep.EventsJSON))
}

func TestStorageTimestampSortsLexically(t *testing.T) {
	a := StorageTimestamp(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	b := StorageTimestamp(time.Date(2026, 1, 2, 3, 4, 5, 100_000_000, time.UTC))
	assert.Equal(t, "2026-01-02T03:04:05.000000Z", a)
	assert.Less(t, a, b)
}
