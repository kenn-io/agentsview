package db

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Splitting the feed must preserve open runs, identities and offsets.
func TestUnitReducerPartitionInvariant(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers(0, math.MaxInt)) % 65
		runCap := hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))
		rows := make([]UnitRow, size)
		session := "a"
		for i := range rows {
			if hegel.Draw(ht, hegel.Booleans()) {
				session = "b"
			}
			rows[i] = UnitRow{
				SessionID: session, Ordinal: i,
				Role:        hegel.Draw(ht, hegel.SampledFrom([]string{"user", "assistant"})),
				Content:     hegel.Draw(ht, hegel.Text()),
				SourceUUID:  hegel.Draw(ht, hegel.SampledFrom([]string{"", "dup", "other"})),
				IsSystem:    hegel.Draw(ht, hegel.Booleans()),
				IsSidechain: hegel.Draw(ht, hegel.Booleans()), Subordinate: session == "b",
			}
		}
		reduce := func(parts [][]UnitRow) []EmbeddableUnit {
			red := UnitReducer{MaxRunRunes: runCap}
			var units []EmbeddableUnit
			emit := func(u EmbeddableUnit) error { units = append(units, u); return nil }
			for _, part := range parts {
				var batch []EmbeddableUnit
				batchEmit := func(u EmbeddableUnit) error { batch = append(batch, u); return nil }
				for _, row := range part {
					require.NoError(ht, red.Push(row, batchEmit))
				}
				units = append(units, batch...)
			}
			require.NoError(ht, red.Flush(emit))
			return units
		}
		var parts [][]UnitRow
		for start := 0; start < len(rows); {
			end := start + 1 + hegel.Draw(ht, hegel.Integers(0, math.MaxInt))%(len(rows)-start)
			parts = append(parts, rows[start:end])
			start = end
		}
		assert.Equal(ht, reduce([][]UnitRow{rows}), reduce(parts))
	}, hegel.WithTestCases(1000))
}

func TestUnitReducerOffsetsAndSystemRows(t *testing.T) {
	red := UnitReducer{MaxRunRunes: 6}
	var got []EmbeddableUnit
	emit := func(u EmbeddableUnit) error { got = append(got, u); return nil }
	for _, row := range []UnitRow{
		{SessionID: "a", Role: "assistant", Ordinal: 0, Content: "界界", SourceUUID: "first"},
		{SessionID: "a", Role: "user", Ordinal: 1, Content: "hidden", IsSystem: true},
		{SessionID: "a", Role: "assistant", Ordinal: 2, Content: "éé"},
		{SessionID: "a", Role: "assistant", Ordinal: 3, Content: "tail"},
	} {
		require.NoError(t, red.Push(row, emit))
	}
	require.NoError(t, red.Flush(emit))
	require.Equal(t, []EmbeddableUnit{
		{
			SessionID: "a", Kind: "run", SourceUUID: "first",
			Ordinal: 0, OrdinalEnd: 2, Content: "界界\n\néé",
			Offsets: []UnitOffset{{Ordinal: 0}, {Ordinal: 2, RuneStart: 4, ByteStart: 8}},
		},
		{SessionID: "a", Kind: "run", Ordinal: 3, OrdinalEnd: 3, Content: "tail", Offsets: []UnitOffset{{Ordinal: 3}}},
	}, got)
}

func TestUnitReducerUserProjection(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		row := UnitRow{
			SessionID: hegel.Draw(ht, hegel.Text()), Role: "user",
			SourceUUID: hegel.Draw(ht, hegel.Text()), Content: hegel.Draw(ht, hegel.Text()),
			Ordinal:     hegel.Draw(ht, hegel.Integers(0, math.MaxInt)),
			Subordinate: hegel.Draw(ht, hegel.Booleans()), IsSidechain: hegel.Draw(ht, hegel.Booleans()),
		}
		red := UnitReducer{MaxRunRunes: hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))}
		var got []EmbeddableUnit
		emit := func(u EmbeddableUnit) error { got = append(got, u); return nil }
		require.NoError(ht, red.Push(row, emit))
		require.NoError(ht, red.Flush(emit))
		assert.Equal(ht, []EmbeddableUnit{{
			SessionID: row.SessionID, Kind: "user", SourceUUID: row.SourceUUID,
			Ordinal: row.Ordinal, OrdinalEnd: row.Ordinal, Content: row.Content,
			Subordinate: row.Subordinate || row.IsSidechain,
		}}, got)
	}, hegel.WithTestCases(1000))
}

func TestUnitReducerRunOffsets(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		first, second := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
		red := UnitReducer{MaxRunRunes: math.MaxInt}
		var got []EmbeddableUnit
		emit := func(u EmbeddableUnit) error { got = append(got, u); return nil }
		require.NoError(ht, red.Push(UnitRow{SessionID: "s", Role: "assistant", Ordinal: 2, Content: first}, emit))
		require.NoError(ht, red.Push(UnitRow{SessionID: "s", Role: "assistant", Ordinal: 5, Content: second}, emit))
		require.NoError(ht, red.Flush(emit))
		require.Len(ht, got, 1)
		require.Len(ht, got[0].Offsets, 2)
		assert.Equal(ht, first+"\n\n"+second, got[0].Content)
		offset := got[0].Offsets[1]
		assert.Equal(ht, 5, offset.Ordinal)
		assert.Equal(ht, second, got[0].Content[offset.ByteStart:])
		assert.Equal(ht, []rune(second), []rune(got[0].Content)[offset.RuneStart:])
	}, hegel.WithTestCases(1000))
}

func TestUnitReducerCallbackErrors(t *testing.T) {
	sentinel := errors.New("stop")
	for _, tc := range []struct {
		name  string
		cap   int
		next  UnitRow
		flush bool
	}{
		{name: "user", cap: 100, next: UnitRow{Role: "user"}},
		{name: "session boundary", cap: 100, next: UnitRow{Role: "assistant", SessionID: "next"}},
		{name: "sidechain boundary", cap: 100, next: UnitRow{Role: "assistant", IsSidechain: true}},
		{name: "cap", cap: 1, next: UnitRow{Role: "assistant", Content: "x"}},
		{name: "flush", cap: 100, flush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			red := UnitReducer{MaxRunRunes: tc.cap}
			if tc.name != "user" {
				require.NoError(t, red.Push(UnitRow{Role: "assistant"}, func(EmbeddableUnit) error { return nil }))
			}
			calls := 0
			emit := func(EmbeddableUnit) error { calls++; return sentinel }
			if tc.flush {
				require.ErrorIs(t, red.Flush(emit), sentinel)
			} else {
				require.ErrorIs(t, red.Push(tc.next, emit), sentinel)
			}
			assert.Equal(t, 1, calls)
		})
	}
}
