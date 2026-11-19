package postgres

import (
	"strings"
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

func TestPGActivityDiskInputsRoundTrip(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	intern := stringutil.Interner{}
	builder := activity.NewSessionRunsBuilder(300)
	builder.AddPiece("model", 1, 2, false)
	runs := builder.Finish()
	row := pgActivityUsageRow{sessionID: intern.Intern("session"), usageSource: intern.Intern("source"), model: intern.Intern("model"), providerID: intern.Intern("provider"), agent: intern.Intern("agent"), output: 3}
	second := row
	second.output = 7
	input := pgActivityInputs{
		identity: "source", usage: []pgActivityUsageRow{second, row, second, row},
		sessions: map[string]pgActivitySession{"session": {runs: runs, users: []int64{1}}}, candidates: map[string]bool{"session": true},
		priced: &pgActivityPriced{rangeStart: time.Unix(1, 0).UTC(), rangeEnd: time.Unix(2, 0).UTC(), effectiveEnd: time.Unix(3, 0).UTC(), next: time.Unix(4, 0).UTC(), pricingDigest: "prices", block: &export.PricingBlock{Source: "catalog", Digest: "digest", Fallback: export.PricingFallback{Models: []string{}}, Models: map[string]export.ModelPricingProvenance{}}, rows: []pgActivityPricedRow{{attribution: intern.Intern("model"), cost: 11, index: 0, webSearches: 1, reported: true}, {attribution: intern.Intern("model"), cost: 22, index: 1, webSearches: 2, priced: true}, {attribution: intern.Intern("model"), cost: 33, index: 2, webSearches: 3, contributes: true}, {attribution: intern.Intern("model"), cost: 44, index: 3, webSearches: 4, sessionCost: true}}},
	}
	var first Store
	require.NoError(t, first.activityDisk.Open("roundtrip", "mirror", 0, 0))
	first.runtimeState().activityInputs.Put("slot", "version", []pgActivityInputs{input})
	first.saveActivityInputs()
	var reopened Store
	require.NoError(t, reopened.activityDisk.Open("roundtrip", "mirror", 0, 0))
	got, ok := reopened.loadActivityInputs("slot", "version", "source")
	require.True(t, ok)
	assert.Equal(t, input.usage, got.usage)
	assert.Equal(t, input.sessions, got.sessions)
	assert.Equal(t, input.candidates, got.candidates)
	assert.Equal(t, input.priced, got.priced)
	version := pgActivityDiskVersion + "version"
	data, ok := first.activityDisk.Get("slot", version)
	require.True(t, ok)
	for _, tc := range []struct {
		name    string
		indexes []int32
	}{
		{"bad index", []int32{4, 1}},
		{"duplicate index", []int32{0, 0}},
		{"nil block", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
			require.True(t, ok)
			require.NotNil(t, rows[0].Value.Priced)
			if tc.indexes == nil {
				// Simulate a corrupt snapshot with a null pricing block.
				rows[0].Value.Priced.Block = []byte("null")
			}
			for i, index := range tc.indexes {
				rows[0].Value.Priced.Rows[i].Index = index
			}
			corrupt, err := kept.EncodeRows(rows, 256<<20, func(kept.Part[pgActivityDiskPart]) int64 { return 0 })
			require.NoError(t, err)
			first.activityDisk.Put("slot", version, corrupt)
			loaded, ok := first.loadActivityInputs("slot", "version", "source")
			require.True(t, ok)
			require.Nil(t, loaded.priced)
		})
	}
}

func TestPGActivityDiskKeepsRawInputsWhenPricesExceedLimit(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk kept.Disk
	require.NoError(t, disk.Open("file", "mirror", 2000, 0))
	rows := []kept.Part[pgActivityDiskPart]{{Value: pgActivityDiskPart{Identity: "source", Usage: []pgActivityDiskRow{{SessionID: "session", Output: 42}}, Priced: &pgActivityDiskPriced{Block: []byte(strings.Repeat("x", 5000))}}}}
	savePGActivityParts(&disk, "slot", "version", rows)
	data, ok := disk.Get("slot", "version")
	require.True(t, ok)
	got, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
	require.True(t, ok)
	require.Len(t, got, 1)
	assert.Equal(t, rows[0].Value.Usage, got[0].Value.Usage)
	assert.Nil(t, got[0].Value.Priced)
}
