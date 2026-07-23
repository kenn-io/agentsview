package artifact

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHLCClockNextPersistsAcrossRestarts(t *testing.T) {
	database := testDB(t)
	now := fixedHLCTime()

	firstClock := NewHLCClock(database, HLCClockOptions{
		Now:      func() time.Time { return now },
		MaxDrift: 5 * time.Minute,
	})
	first, err := firstClock.Next()
	require.NoError(t, err)
	assert.Equal(t, now, first.WallTime)
	assert.Equal(t, uint64(0), first.Logical)

	persisted, err := database.GetSyncState(metadataHLCStateKey)
	require.NoError(t, err)
	assert.Equal(t, "2026-06-14T010203.000000001Z-00000000000000000000", persisted)

	secondClock := NewHLCClock(database, HLCClockOptions{
		Now:      func() time.Time { return now },
		MaxDrift: 5 * time.Minute,
	})
	second, err := secondClock.Next()
	require.NoError(t, err)
	assert.Equal(t, now, second.WallTime)
	assert.Equal(t, uint64(1), second.Logical)
}

func TestHLCClockNextMonotonicCases(t *testing.T) {
	base := fixedHLCTime()

	tests := []struct {
		name string
		last HLCTimestamp
		now  time.Time
		want HLCTimestamp
	}{
		{
			name: "same wall time increments logical counter",
			last: HLCTimestamp{WallTime: base, Logical: 7},
			now:  base,
			want: HLCTimestamp{WallTime: base, Logical: 8},
		},
		{
			name: "physical time ahead resets logical counter",
			last: HLCTimestamp{WallTime: base, Logical: 7},
			now:  base.Add(time.Nanosecond),
			want: HLCTimestamp{WallTime: base.Add(time.Nanosecond), Logical: 0},
		},
		{
			name: "backward skew within bound increments logical counter",
			last: HLCTimestamp{WallTime: base, Logical: 7},
			now:  base.Add(-2 * time.Minute),
			want: HLCTimestamp{WallTime: base, Logical: 8},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			require.NoError(t, database.SetSyncState(metadataHLCStateKey, tt.last.String()))
			clock := NewHLCClock(database, HLCClockOptions{
				Now:      func() time.Time { return tt.now },
				MaxDrift: 5 * time.Minute,
			})

			got, err := clock.Next()
			require.NoError(t, err)

			assert.Equal(t, 0, tt.want.Compare(got))
			persisted, err := database.GetSyncState(metadataHLCStateKey)
			require.NoError(t, err)
			assert.Equal(t, tt.want.String(), persisted)
		})
	}
}

func TestHLCClockNextRejectsBackwardSkewBeyondBound(t *testing.T) {
	database := testDB(t)
	base := fixedHLCTime()
	last := HLCTimestamp{WallTime: base, Logical: 7}
	require.NoError(t, database.SetSyncState(metadataHLCStateKey, last.String()))
	clock := NewHLCClock(database, HLCClockOptions{
		Now:      func() time.Time { return base.Add(-10 * time.Minute) },
		MaxDrift: 5 * time.Minute,
	})

	got, err := clock.Next()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHLCDrift)

	assert.Equal(t, HLCTimestamp{}, got)
	assert.Contains(t, err.Error(), "persisted HLC wall time")
	persisted, err := database.GetSyncState(metadataHLCStateKey)
	require.NoError(t, err)
	assert.Equal(t, last.String(), persisted)
}

func TestHLCClockObserveCases(t *testing.T) {
	base := fixedHLCTime()

	tests := []struct {
		name   string
		last   *HLCTimestamp
		now    time.Time
		remote HLCTimestamp
		want   HLCTimestamp
	}{
		{
			name:   "remote future within bound is absorbed",
			now:    base,
			remote: HLCTimestamp{WallTime: base.Add(time.Minute), Logical: 3},
			want:   HLCTimestamp{WallTime: base.Add(time.Minute), Logical: 4},
		},
		{
			name:   "same wall uses max logical counter",
			last:   &HLCTimestamp{WallTime: base, Logical: 5},
			now:    base,
			remote: HLCTimestamp{WallTime: base, Logical: 7},
			want:   HLCTimestamp{WallTime: base, Logical: 8},
		},
		{
			name:   "local physical time wins when ahead",
			last:   &HLCTimestamp{WallTime: base, Logical: 7},
			now:    base.Add(time.Minute),
			remote: HLCTimestamp{WallTime: base.Add(30 * time.Second), Logical: 9},
			want:   HLCTimestamp{WallTime: base.Add(time.Minute), Logical: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			if tt.last != nil {
				require.NoError(t, database.SetSyncState(metadataHLCStateKey, tt.last.String()))
			}
			clock := NewHLCClock(database, HLCClockOptions{
				Now:      func() time.Time { return tt.now },
				MaxDrift: 5 * time.Minute,
			})

			got, err := clock.Observe(tt.remote)
			require.NoError(t, err)

			assert.Equal(t, 0, tt.want.Compare(got))
			persisted, err := database.GetSyncState(metadataHLCStateKey)
			require.NoError(t, err)
			assert.Equal(t, tt.want.String(), persisted)
		})
	}
}

func TestHLCClockObserveRejectsRemoteFutureBeyondBound(t *testing.T) {
	database := testDB(t)
	base := fixedHLCTime()
	last := HLCTimestamp{WallTime: base, Logical: 7}
	require.NoError(t, database.SetSyncState(metadataHLCStateKey, last.String()))
	clock := NewHLCClock(database, HLCClockOptions{
		Now:      func() time.Time { return base },
		MaxDrift: 5 * time.Minute,
	})

	got, err := clock.Observe(HLCTimestamp{
		WallTime: base.Add(10 * time.Minute),
		Logical:  3,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHLCDrift)

	assert.Equal(t, HLCTimestamp{}, got)
	assert.Contains(t, err.Error(), "remote HLC wall time")
	persisted, err := database.GetSyncState(metadataHLCStateKey)
	require.NoError(t, err)
	assert.Equal(t, last.String(), persisted)
}

func TestHLCTimestampOrderingKeyAndParse(t *testing.T) {
	base := fixedHLCTime()
	stamp := HLCTimestamp{WallTime: base, Logical: 42}

	text := stamp.String()
	parsed, err := ParseHLCTimestamp(text)
	require.NoError(t, err)

	assert.Equal(t, "2026-06-14T010203.000000001Z-00000000000000000042", text)
	assert.Equal(t, 0, stamp.Compare(parsed))
	assert.Equal(t, -1, stamp.Compare(HLCTimestamp{WallTime: base, Logical: 43}))
	assert.Equal(t, -1, stamp.Compare(HLCTimestamp{WallTime: base.Add(time.Nanosecond), Logical: 0}))
	assert.Equal(t, 1, stamp.Compare(HLCTimestamp{WallTime: base.Add(-time.Nanosecond), Logical: 99}))
	assert.Less(t, stamp.OrderingKey("a111"), stamp.OrderingKey("b222"))
	assert.Less(t,
		HLCTimestamp{WallTime: base, Logical: 41}.OrderingKey("ffff"),
		stamp.OrderingKey("0000"),
	)
}

func TestParseHLCTimestampRejectsMalformedValues(t *testing.T) {
	tests := []string{
		"",
		"2026-06-14T010203Z-00000000000000000000",
		"2026-06-14T010203.000000001Z-42",
		"2026-06-14T010203.000000001Z-0000000000000000000x",
		"2026-06-14T01:02:03.000000001Z-00000000000000000000",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			_, err := ParseHLCTimestamp(tt)
			require.Error(t, err)
		})
	}
}

func fixedHLCTime() time.Time {
	return time.Date(2026, 6, 14, 1, 2, 3, 1, time.UTC)
}
