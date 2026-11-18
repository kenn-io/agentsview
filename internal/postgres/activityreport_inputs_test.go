package postgres

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/stringutil"
)

func TestPGActivityInputBytesIncludesPrices(t *testing.T) {
	raw := pgActivityInputs{}
	withPrices := pgActivityInputs{priced: &pgActivityPriced{rows: make([]pgActivityPricedRow, 2), block: &export.PricingBlock{}}}
	assert.Greater(t, pgActivityInputBytes(withPrices), pgActivityInputBytes(raw))
}

func TestPGActivityPricedCovers(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	effective := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	next := effective.Add(time.Hour)
	priced := pgActivityPriced{rangeStart: start, rangeEnd: end, effectiveEnd: effective, next: next, pricingDigest: "prices"}
	for _, tc := range []struct {
		name   string
		query  activity.Query
		digest string
		want   bool
	}{
		{"same", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "prices", true},
		{"rewind within a microsecond", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective.Add(-time.Nanosecond)}, "prices", false},
		{"start changes", activity.Query{RangeStart: start.Add(time.Microsecond), RangeEnd: end, EffectiveEnd: effective}, "prices", false},
		{"end changes", activity.Query{RangeStart: start, RangeEnd: end.Add(time.Microsecond), EffectiveEnd: effective}, "prices", false},
		{"prices change", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "new prices", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, priced.covers(tc.query, tc.digest))
		})
	}
	priced.next = time.Time{}
	assert.True(t, priced.covers(activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: end}, "prices"))
}

func TestPGActivityDiskRowRoundTrip(t *testing.T) {
	intern := stringutil.Interner{}
	row := pgActivityUsageRow{
		sessionID: intern.Intern("session"), usageSource: intern.Intern("source"), model: intern.Intern("model"), providerID: intern.Intern("provider"), agent: intern.Intern("agent"),
		claudeMessageID: "message", claudeRequestID: "request", sourceUUID: "uuid", usageDedupKey: "dedup",
		us: 1, cost: 2, webSearches: 3, input: 4, output: 5, cacheCreate: 6, cacheRead: 7, reasoning: 8,
		ordinal: 9, cacheCreate1h: 10, ordinalValid: true, costValid: true, copilotReported: true, pricingTimeAbsent: true, costRecorded: true,
	}
	assert.Equal(t, row, pgActivityRowToDisk(row).activityRow(stringutil.Interner{}))
}

func TestPGActivityDiskDropsPricesWhenOverLimit(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	intern := stringutil.Interner{}
	runs, err := activity.NewSessionRuns([]int64{1, 2}, nil, []activity.SessionRunPiece{{Model: "model", Last: 1}})
	require.NoError(t, err)
	input := pgActivityInputs{
		identity: "source", usage: []pgActivityUsageRow{{sessionID: intern.Intern("session"), usageSource: intern.Intern("source"), model: intern.Intern("model"), providerID: intern.Intern("provider"), agent: intern.Intern("agent")}},
		sessions: map[string]pgActivitySession{"session": {runs: runs, users: []int64{1}}}, candidates: map[string]bool{"session": true},
		priced: &pgActivityPriced{rangeStart: time.Unix(1, 0).UTC(), rangeEnd: time.Unix(2, 0).UTC(), effectiveEnd: time.Unix(3, 0).UTC(), next: time.Unix(4, 0).UTC(), pricingDigest: "prices", block: &export.PricingBlock{Source: "catalog", Digest: "digest", Fallback: export.PricingFallback{Models: []string{}}, Models: map[string]export.ModelPricingProvenance{}}, rows: []pgActivityPricedRow{{attribution: intern.Intern("model"), index: 0}}},
	}
	var probe Store
	require.NoError(t, probe.activityDisk.Open("probe", "mirror", 0, 0))
	probe.runtimeState().activityInputs.Put("slot", "version", []pgActivityInputs{input})
	probe.saveActivityInputs()
	priced, ok := probe.activityDisk.Get("slot", pgActivityDiskVersion+"version")
	require.True(t, ok)
	rawInput := input
	rawInput.priced = nil
	probe.runtimeState().activityInputs.Put("slot", "version", []pgActivityInputs{rawInput})
	probe.saveActivityInputs()
	raw, ok := probe.activityDisk.Get("slot", pgActivityDiskVersion+"version")
	require.True(t, ok)
	files, err := filepath.Glob(filepath.Join(cache, "probe", "mirror", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	header := info.Size() - int64(len(raw))
	require.Greater(t, len(priced), len(raw))

	for _, tc := range []struct {
		name         string
		limit        int64
		want, prices bool
	}{
		{"fits", int64(len(priced)) + header, true, true},
		{"header exceeds limit", int64(len(priced)) + header - 1, true, false},
		{"raw exceeds limit", int64(len(raw)) + header - 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first Store
			require.NoError(t, first.activityDisk.Open(tc.name, "mirror", tc.limit, 0))
			first.runtimeState().activityInputs.Put("slot", "version", []pgActivityInputs{input})
			first.saveActivityInputs()
			var reopened Store
			require.NoError(t, reopened.activityDisk.Open(tc.name, "mirror", tc.limit, 0))
			got, ok := reopened.loadActivityInputs("slot", "version", "source")
			require.Equal(t, tc.want, ok)
			if !ok {
				return
			}
			assert.Equal(t, input.usage, got.usage)
			assert.Equal(t, input.sessions, got.sessions)
			assert.Equal(t, input.candidates, got.candidates)
			if tc.prices {
				assert.Equal(t, input.priced, got.priced)
			} else {
				assert.Nil(t, got.priced)
			}
		})
	}
}

func TestPGActivityDiskRejectsInvalidPricedIndexes(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	for _, tc := range []struct {
		name    string
		indexes []int32
	}{
		{"bad index", []int32{2, 1}},
		{"duplicate index", []int32{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var store Store
			require.NoError(t, store.activityDisk.Open(tc.name, "mirror", 0, 0))
			intern := stringutil.Interner{}
			row := pgActivityUsageRow{sessionID: intern.Intern("session"), usageSource: intern.Intern("source"), model: intern.Intern("model"), providerID: intern.Intern("provider"), agent: intern.Intern("agent")}
			input := pgActivityInputs{identity: "source", usage: []pgActivityUsageRow{row, row}, priced: &pgActivityPriced{block: &export.PricingBlock{}, rows: []pgActivityPricedRow{{attribution: intern.Intern("model"), index: 0}, {attribution: intern.Intern("model"), index: 1}}}}
			store.runtimeState().activityInputs.Put("slot", "version", []pgActivityInputs{input})
			store.saveActivityInputs()
			version := pgActivityDiskVersion + "version"
			data, ok := store.activityDisk.Get("slot", version)
			require.True(t, ok)
			rows, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
			require.True(t, ok)
			require.NotNil(t, rows[0].Value.Priced)
			for i, index := range tc.indexes {
				rows[0].Value.Priced.Rows[i].Index = index
			}
			data, err := kept.EncodeRows(rows, 256<<20, func(kept.Part[pgActivityDiskPart]) int64 { return 0 })
			require.NoError(t, err)
			require.NoError(t, store.activityDisk.Put("slot", version, data))
			loaded, ok := store.loadActivityInputs("slot", "version", "source")
			require.True(t, ok)
			require.Nil(t, loaded.priced)
		})
	}
}
