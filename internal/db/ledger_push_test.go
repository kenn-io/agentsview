package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListLedgerSegmentsForPush(t *testing.T) {
	d := testDB(t)
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 1, 1))
	mustAppend(t, d, "zone-b", testLedgerSegment(t, "host-b", 1, 2))
	mustAppend(t, d, "zone-a", testLedgerSegment(t, "host-a", 2, 1))

	all, err := d.ListLedgerSegmentsForPush(t.Context(), "", nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1].Cursor(), all[i].Cursor()
		assert.LessOrEqual(t, prev.IngestedAt, cur.IngestedAt, "ingest order")
	}

	var paged []LedgerPushSegment
	var cursor *LedgerPushCursor
	for {
		page, err := d.ListLedgerSegmentsForPush(t.Context(), "", cursor, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		c := page[0].Cursor()
		cursor = &c
	}
	require.Len(t, paged, 3, "keyset paging visits every segment once")
	for i := range all {
		assert.True(t, all[i].Segment.ContentMatches(paged[i].Segment))
		assert.Equal(t, all[i].Zone, paged[i].Zone)
	}

	later, err := d.ListLedgerSegmentsForPush(t.Context(), "9999-01-01T00:00:00.000000Z", nil, 0)
	require.NoError(t, err)
	assert.Empty(t, later)
}

func TestListSyncStateByPrefix(t *testing.T) {
	d := testDB(t)
	for key, value := range map[string]string{
		"pg_ledger_push_status_v1:hub":     "a",
		"pg_ledger_push_status_v1:":        "b",
		"pg_ledger_push_status_v1_x:other": "not a match for the literal underscore",
		"pg_ledger_push_statusXv1:y":       "not a match: _ is literal",
		"last_push_at":                     "c",
	} {
		require.NoError(t, d.SetSyncState(t.Context(), key, value))
	}
	got, err := d.ListSyncStateByPrefix(t.Context(), "pg_ledger_push_status_v1:")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"hub": "a", "": "b"}, got)
}
