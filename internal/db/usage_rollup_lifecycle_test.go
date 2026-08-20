//go:build fts5

package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageTimezoneIdentityUsesNamedZone(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)
	intervals := usageQueryIntervals(UsageFilter{
		From: "2026-03-07", To: "2026-03-09", Timezone: "America/Chicago",
	})

	got := usageTimezoneIdentityFor(loc, intervals)

	assert.Equal(t, "America/Chicago", got.Name)
	assert.NotEmpty(t, got.IntervalFingerprint)
	assert.Equal(t, "America/Chicago:"+got.IntervalFingerprint, got.Key)
}

func TestUsageTimezoneRuleFingerprintTracksNamedZoneRules(t *testing.T) {
	left := usageTimezoneRuleFingerprint(
		"Named/Zone", time.FixedZone("Named/Zone", -6*60*60))
	right := usageTimezoneRuleFingerprint(
		"Named/Zone", time.FixedZone("Named/Zone", 9*60*60))

	assert.NotEqual(t, left, right,
		"changed named-zone rules must select a new rollup generation")
}

// testTZifSingleTransition builds a TZif v1 zone that switches from UTC+0
// "AAA" to UTC+1 "BBB" at the given Unix instant.
func testTZifSingleTransition(t *testing.T, transition int64) *time.Location {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("TZif")
	buf.Write(make([]byte, 16))
	for _, count := range []uint32{0, 0, 0, 1, 2, 8} {
		require.NoError(t, binary.Write(&buf, binary.BigEndian, count))
	}
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int32(transition)))
	buf.WriteByte(1)
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int32(0)))
	buf.Write([]byte{0, 0})
	require.NoError(t, binary.Write(&buf, binary.BigEndian, int32(3600)))
	buf.Write([]byte{1, 4})
	buf.WriteString("AAA\x00BBB\x00")
	location, err := time.LoadLocationFromTZData("Test/Zone", buf.Bytes())
	require.NoError(t, err)
	return location
}

func TestUsageTimezoneRuleFingerprintTracksTransitionInstants(t *testing.T) {
	tuesday := time.Date(1990, 6, 12, 3, 0, 0, 0, time.UTC).Unix()
	wednesday := time.Date(1990, 6, 13, 3, 0, 0, 0, time.UTC).Unix()

	left := usageTimezoneRuleFingerprint(
		"Test/Zone", testTZifSingleTransition(t, tuesday))
	right := usageTimezoneRuleFingerprint(
		"Test/Zone", testTZifSingleTransition(t, wednesday))

	assert.NotEqual(t, left, right,
		"a zoneinfo update that moves a transition must change the fingerprint")
}

func TestUsageTimezoneRuleFingerprintSeparatesAnonymousLocalRules(t *testing.T) {
	left := usageTimezoneRuleFingerprint(
		"Local", time.FixedZone("Local", -6*60*60))
	right := usageTimezoneRuleFingerprint(
		"Local", time.FixedZone("Local", 9*60*60))

	assert.NotEqual(t, left, right)
}

func TestUsageTimezoneIdentityIsIndependentOfQueryWindow(t *testing.T) {
	location, err := time.LoadLocation("America/Denver")
	require.NoError(t, err)
	short := usageTimezoneIdentityFor(location, []usageQueryInterval{{
		FromMillis: 0, ToMillis: 86_400_000,
		FromLocalDate: "2026-01-01", ToLocalDate: "2026-01-01",
	}})
	long := usageTimezoneIdentityFor(location, []usageQueryInterval{{
		FromMillis: 0, ToMillis: 2_592_000_000,
		FromLocalDate: "2026-01-01", ToLocalDate: "2026-01-30",
	}})
	assert.Equal(t, short, long)
}

func TestUsageLocationNameDoesNotRewriteAnonymousFixedZone(t *testing.T) {
	assert.Equal(t, "Local", usageLocationName(time.FixedZone("Local", -6*60*60)))
}

func TestUsageTimezoneIdentityCachesPerZoneNameNotPerPointer(t *testing.T) {
	first, err := time.LoadLocation("Pacific/Chatham")
	require.NoError(t, err)
	second, err := time.LoadLocation("Pacific/Chatham")
	require.NoError(t, err)

	assert.Equal(t,
		usageTimezoneIdentityFor(first, nil),
		usageTimezoneIdentityFor(second, nil))

	entries := 0
	usageTimezoneIdentityCache.Range(func(_, value any) bool {
		if value.(usageTimezoneIdentity).Name == "Pacific/Chatham" {
			entries++
		}
		return true
	})
	assert.Equal(t, 1, entries,
		"repeated LoadLocation calls for one zone must share a cache entry")
}

func TestUsageTimezoneIdentitySurvivesLocalInitialization(t *testing.T) {
	before := usageTimezoneIdentityFor(time.Local, nil)
	_, _ = time.Now().In(time.Local).Zone()
	after := usageTimezoneIdentityFor(time.Local, nil)
	assert.Equal(t, before, after)
}

func TestUsageRollupCallKeyIncludesCursorHighWater(t *testing.T) {
	snapshot := usageQuerySnapshot{CursorHighWater: 1, location: time.UTC}
	first := usageRollupCallKey(snapshot, nil, "pricing")
	snapshot.CursorHighWater = 2
	second := usageRollupCallKey(snapshot, nil, "pricing")
	assert.NotEqual(t, first, second)
}

func TestUsageRollupCallKeyIncludesBakedSessionMetadata(t *testing.T) {
	snapshot := usageQuerySnapshot{
		location: time.UTC,
		Versions: []usageSourceVersion{{SessionID: "session-a"}},
		Sessions: []usageQuerySession{{
			ID: "session-a", Agent: "codex", StartedAt: "2026-08-10T08:00:00Z",
		}},
	}
	fills := map[string]usageFillResult{
		"session-a": {InstallRevision: 1},
	}

	original := usageRollupCallKey(snapshot, fills, "pricing")
	snapshot.Sessions[0].Agent = "claude"
	agentChanged := usageRollupCallKey(snapshot, fills, "pricing")
	snapshot.Sessions[0].StartedAt = "2026-08-11T08:00:00Z"
	startedAtChanged := usageRollupCallKey(snapshot, fills, "pricing")

	assert.NotEqual(t, original, agentChanged)
	assert.NotEqual(t, agentChanged, startedAtChanged)
}

func TestUsagePricingHashTracksPricingSemantics(t *testing.T) {
	base := []export.EffectivePricingRow{{
		ModelPattern: "model-*",
		Rates: export.ModelRates{
			InputPerMTok:      money.Money{Microdollars: 1_000_000},
			OutputPerMTok:     money.Money{Microdollars: 2_000_000},
			CacheWritePerMTok: money.Money{Microdollars: 3_000_000},
			CacheReadPerMTok:  money.Money{Microdollars: 4_000_000},
			Source:            export.PricingRowSourceCustom,
			Bands: []export.PricingBand{{
				AboveInputTokens: 200_000,
				InputPerMTok:     money.Money{Microdollars: 5_000_000},
				OutputPerMTok:    money.Money{Microdollars: 6_000_000},
			}},
		},
	}}
	original, err := export.EffectivePricingDigest(base)
	require.NoError(t, err)
	identical, err := export.EffectivePricingDigest(cloneEffectivePricingRowsForTest(base))
	require.NoError(t, err)
	assert.Equal(t, original, identical)
	reordered := cloneEffectivePricingRowsForTest(base)
	reordered = append(reordered, export.EffectivePricingRow{
		ModelPattern: "another-*",
		Rates: export.ModelRates{
			InputPerMTok: money.Money{Microdollars: 7_000_000},
		},
	})
	forward, err := export.EffectivePricingDigest(reordered)
	require.NoError(t, err)
	slices.Reverse(reordered)
	reversed, err := export.EffectivePricingDigest(reordered)
	require.NoError(t, err)
	assert.Equal(t, forward, reversed, "pricing order must not invalidate rollups")

	tests := map[string]func([]export.EffectivePricingRow){
		"pattern": func(rows []export.EffectivePricingRow) {
			rows[0].ModelPattern = "other-*"
		},
		"input rate": func(rows []export.EffectivePricingRow) {
			rows[0].Rates.InputPerMTok.Microdollars++
		},
		"band threshold": func(rows []export.EffectivePricingRow) {
			rows[0].Rates.Bands[0].AboveInputTokens++
		},
		"band rate": func(rows []export.EffectivePricingRow) {
			rows[0].Rates.Bands[0].OutputPerMTok.Microdollars++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := cloneEffectivePricingRowsForTest(base)
			mutate(changed)
			got, err := export.EffectivePricingDigest(changed)
			require.NoError(t, err)
			assert.NotEqual(t, original, got)
		})
	}
}

func TestUsageQuerySnapshotPinsPricingRows(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "snapshot-model",
		InputPerMTok: money.Money{Microdollars: 1_000_000},
	}}))

	before, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	beforeRate := effectivePricingRateForTest(t, before.PricingRows, "snapshot-model")

	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "snapshot-model",
		InputPerMTok: money.Money{Microdollars: 9_000_000},
	}}))
	after, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	afterRate := effectivePricingRateForTest(t, after.PricingRows, "snapshot-model")

	assert.Equal(t, int64(1_000_000), beforeRate.InputPerMTok.Microdollars)
	assert.Equal(t, int64(9_000_000), afterRate.InputPerMTok.Microdollars)
}

func TestUsageRollupAgentChangeInvalidatesInstalledRows(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	for _, id := range []string{"rollup-session", "rollup-peer"} {
		insertSession(t, database, id, "project-a", func(session *Session) {
			session.Agent = "codex"
			session.StartedAt = &started
		})
	}
	require.NoError(t, database.InsertMessages([]Message{
		{
			SessionID: "rollup-session", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model-a",
			TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			SourceUUID: "shared-source",
		},
		{
			SessionID: "rollup-peer", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:01:00Z", Model: "model-a",
			TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			SourceUUID: "shared-source",
		},
	}))

	firstSnapshot, firstFills, cache := prepareUsageRollupTest(t, database)
	first, _, err := cache.rollup.Ensure(
		t.Context(), firstSnapshot, firstFills,
		export.NewPricingResolver(firstSnapshot.PricingRows),
	)
	require.NoError(t, err)
	firstInstall := first["rollup-session"]
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}
	beforeDaily, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 2, beforeDaily.Totals.InputTokens,
		"same-agent source identities deduplicate")
	assert.Equal(t, 3, beforeDaily.Totals.OutputTokens)

	var beforeVersion usageSourceVersion
	for _, version := range firstSnapshot.Versions {
		if version.SessionID == "rollup-session" {
			beforeVersion = version
			break
		}
	}
	require.Equal(t, "rollup-session", beforeVersion.SessionID)
	_, err = database.getWriter().Exec(`UPDATE sessions SET agent = 'claude'
		WHERE id = 'rollup-session'`)
	require.NoError(t, err)
	secondSnapshot, secondFills, _ := prepareUsageRollupTest(t, database)
	var afterVersion usageSourceVersion
	for _, version := range secondSnapshot.Versions {
		if version.SessionID == "rollup-session" {
			afterVersion = version
			break
		}
	}
	require.Equal(t, "rollup-session", afterVersion.SessionID)
	assert.True(t, beforeVersion.Equal(afterVersion),
		"agent is intentionally outside the normalized-facts fingerprint")

	second, _, err := cache.rollup.Ensure(
		t.Context(), secondSnapshot, secondFills,
		export.NewPricingResolver(secondSnapshot.PricingRows),
	)
	require.NoError(t, err)
	assert.Greater(t, second["rollup-session"].InstallRevision,
		firstInstall.InstallRevision)
	afterDaily, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 4, afterDaily.Totals.InputTokens,
		"changing the baked agent must split the source identity groups")
	assert.Equal(t, 6, afterDaily.Totals.OutputTokens)
}

func TestUsageRollupStartedAtChangeInvalidatesInstalledRows(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "rollup-start", "project-a", func(session *Session) {
		session.Agent = "codex"
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "rollup-start", Ordinal: 0, Role: "assistant",
		Model:      "model-a",
		TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
	}}))
	_, err := database.getWriter().Exec(`UPDATE sessions
		SET file_mtime = '2099-01-01T00:00:00Z' WHERE id = 'rollup-start'`)
	require.NoError(t, err)

	firstSnapshot, firstFills, cache := prepareUsageRollupTest(t, database)
	first, _, err := cache.rollup.Ensure(
		t.Context(), firstSnapshot, firstFills,
		export.NewPricingResolver(firstSnapshot.PricingRows),
	)
	require.NoError(t, err)
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-11", Timezone: "UTC",
	}
	beforeDaily, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, beforeDaily.Daily, 1)
	assert.Equal(t, "2026-08-10", beforeDaily.Daily[0].Date)
	_, err = database.getWriter().Exec(`UPDATE sessions
		SET started_at = '2026-08-11T08:30:00Z' WHERE id = 'rollup-start'`)
	require.NoError(t, err)
	secondSnapshot, secondFills, _ := prepareUsageRollupTest(t, database)
	assert.True(t, firstSnapshot.Versions[0].Equal(secondSnapshot.Versions[0]),
		"a later file_mtime masks the started_at change in sync_marker")

	second, _, err := cache.rollup.Ensure(
		t.Context(), secondSnapshot, secondFills,
		export.NewPricingResolver(secondSnapshot.PricingRows),
	)
	require.NoError(t, err)
	assert.Greater(t, second["rollup-start"].InstallRevision,
		first["rollup-start"].InstallRevision)
	afterDaily, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, afterDaily.Daily, 1)
	assert.Equal(t, "2026-08-11", afterDaily.Daily[0].Date,
		"null-timestamp usage must move with the rebuilt session start")
}

func TestUsageRollupPricingChangeInvalidatesInstalledRows(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "rollup-price", "project-a", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "rollup-price", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "priced-model",
		TokenUsage: json.RawMessage(`{"input_tokens":1000000}`),
	}}))
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "priced-model",
		InputPerMTok: money.Money{Microdollars: 1_000_000},
	}}))

	firstSnapshot, firstFills, cache := prepareUsageRollupTest(t, database)
	first, _, err := cache.rollup.Ensure(t.Context(), firstSnapshot, firstFills,
		export.NewPricingResolver(firstSnapshot.PricingRows))
	require.NoError(t, err)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "priced-model",
		InputPerMTok: money.Money{Microdollars: 2_000_000},
	}}))
	secondSnapshot, secondFills, _ := prepareUsageRollupTest(t, database)
	second, _, err := cache.rollup.Ensure(t.Context(), secondSnapshot, secondFills,
		export.NewPricingResolver(secondSnapshot.PricingRows))
	require.NoError(t, err)
	assert.Greater(t, second["rollup-price"].InstallRevision,
		first["rollup-price"].InstallRevision)

	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2_000_000), daily.Totals.TotalCost.Microdollars)
}

func TestUsageRollupConnectedSnapshotChangeRebuildsOnlyChangedSession(t *testing.T) {
	database := testDB(t)
	for index, id := range []string{"session-a", "session-b"} {
		started := []string{"2026-08-10T08:00:00Z", "2026-08-10T08:01:00Z"}[index]
		insertSession(t, database, id, "project-a", func(session *Session) {
			session.Agent = "claude"
			session.StartedAt = &started
		})
	}
	message := func(sessionID string, output int) Message {
		return Message{
			SessionID: sessionID, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model-a",
			TokenUsage: json.RawMessage(fmt.Sprintf(
				`{"input_tokens":2,"output_tokens":%d}`, output)),
			ClaudeMessageID: "message-id", ClaudeRequestID: "request-id",
		}
	}
	require.NoError(t, database.InsertMessages([]Message{
		message("session-a", 10), message("session-b", 20),
	}))

	firstSnapshot, firstFills, cache := prepareUsageRollupTest(t, database)
	first, _, err := cache.rollup.Ensure(
		t.Context(), firstSnapshot, firstFills,
		export.NewPricingResolver(firstSnapshot.PricingRows),
	)
	require.NoError(t, err)
	require.NoError(t, database.ReplaceSessionMessages(
		"session-b", []Message{message("session-b", 30)},
	))
	secondSnapshot, secondFills, _ := prepareUsageRollupTest(t, database)
	second, _, err := cache.rollup.Ensure(
		t.Context(), secondSnapshot, secondFills,
		export.NewPricingResolver(secondSnapshot.PricingRows),
	)
	require.NoError(t, err)
	assert.Equal(t, first["session-a"].InstallRevision,
		second["session-a"].InstallRevision)
	assert.Greater(t, second["session-b"].InstallRevision,
		first["session-b"].InstallRevision)

	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, daily.Totals.InputTokens)
	assert.Equal(t, 30, daily.Totals.OutputTokens,
		"the changed sibling must immediately replace the snapshot winner")
}

func prepareUsageRollupTest(
	t *testing.T, database *DB,
) (usageQuerySnapshot, map[string]usageFillResult, *usageCache) {
	t.Helper()
	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(
		t.Context(), snapshot.Versions, snapshot.CursorHighWater,
	)
	require.NoError(t, err)
	return snapshot, fills, cache
}

func cloneEffectivePricingRowsForTest(
	rows []export.EffectivePricingRow,
) []export.EffectivePricingRow {
	cloned := append([]export.EffectivePricingRow(nil), rows...)
	for index := range cloned {
		cloned[index].Rates.Bands = append(
			[]export.PricingBand(nil), rows[index].Rates.Bands...,
		)
	}
	return cloned
}

func effectivePricingRateForTest(
	t *testing.T, rows []export.EffectivePricingRow, pattern string,
) export.ModelRates {
	t.Helper()
	for _, row := range rows {
		if row.ModelPattern == pattern {
			return row.Rates
		}
	}
	require.FailNow(t, "pricing row not found", pattern)
	return export.ModelRates{}
}
