//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncFullPushCreatesExpectedRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "mirror.duckdb")
	syncer := newTestSync(t, target, local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)
	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
	assertDuckDBCount(t, syncer.DB(), "messages", 3)
	assertDuckDBCount(t, syncer.DB(), "tool_calls", 1)
	assertDuckDBCount(t, syncer.DB(), "tool_result_events", 1)
	assertDuckDBCount(t, syncer.DB(), "usage_events", 1)
	assertDuckDBCount(t, syncer.DB(), "secret_findings", 1)
	assertDuckDBCount(t, syncer.DB(), "model_pricing", 1)
	assertDuckDBCount(t, syncer.DB(), "starred_sessions", 1)
	assertDuckDBCount(t, syncer.DB(), "pinned_messages", 1)

	var firstMessage string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT first_message FROM sessions WHERE id = ?`,
		fixture.alphaID,
	).Scan(&firstMessage))
	assert.Equal(t, "alpha first", firstMessage)
}

func TestSessionFingerprintsStoreDigestOnly(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "mirror.duckdb"), local, SyncOptions{})
	session, err := local.GetSession(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.NotNil(t, session)

	got, err := syncer.sessionFingerprints(ctx, []db.Session{*session})
	require.NoError(t, err)

	fp := got[fixture.alphaID]
	assert.Len(t, fp, 64)
	assert.False(t, strings.Contains(fp, "alpha first"))
	assert.False(t, strings.Contains(fp, "secret token sk-duckdb"))
	assert.False(t, strings.Contains(fp, "duck result"))
	assert.False(t, strings.Contains(fp, "pin alpha"))
}

func TestDuckSessionFingerprintFieldsDiffer(t *testing.T) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}
	encode := func(s db.Session) string {
		data, err := json.Marshal(duckSessionFingerprintFields(s, "laptop"))
		require.NoError(t, err)
		return string(data)
	}
	fp1 := encode(base)

	tests := []struct {
		name   string
		modify func(s db.Session) db.Session
	}{
		{
			name: "display name change",
			modify: func(s db.Session) db.Session {
				name := "new name"
				s.DisplayName = &name
				return s
			},
		},
		{
			name: "session_name change",
			modify: func(s db.Session) db.Session {
				n := "agent-provided-title"
				s.SessionName = &n
				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEqual(t, fp1, encode(tt.modify(base)))
		})
	}
}

func TestWriteSyncFingerprintsNormalizesRetainedLegacyValues(t *testing.T) {
	local := newLocalDB(t)
	legacy := `{"Messages":[{"content":"secret token sk-legacy"}]}`

	require.NoError(t, writeSyncFingerprints(
		local,
		"2026-01-10T00:00:00.000Z",
		nil,
		map[string]string{"unchanged": legacy},
		nil,
	))

	raw, err := local.GetSyncState(lastPushBoundaryStateKey)
	require.NoError(t, err)
	require.NotContains(t, raw, "secret token sk-legacy")
	var state syncState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	fp := state.Fingerprints["unchanged"]
	require.Len(t, fp, 64)
	assert.NotEqual(t, legacy, fp)
}

func TestSyncUsesFallbackPricingWhenLocalPricingIsEmpty(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-fallback-pricing"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "pricing", "2026-01-10T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "assistant", "pricing", "2026-01-10T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "mirror.duckdb"), local, SyncOptions{})

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	var count int
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM model_pricing WHERE model_pattern = ?`,
		"claude-sonnet-4-6",
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestSyncModelPricingPreservesExistingMirrorRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-preserve-pricing"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "pricing", "2026-01-10T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "assistant", "pricing", "2026-01-10T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, filepath.Join(t.TempDir(), "mirror.duckdb"), local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('other-machine-model', 1, 2, 3, 4, '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)

	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	var input, output float64
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT input_per_mtok, output_per_mtok
		 FROM model_pricing WHERE model_pattern = ?`,
		"other-machine-model",
	).Scan(&input, &output))
	assert.Equal(t, 1.0, input)
	assert.Equal(t, 2.0, output)
}

func TestSyncIncrementalSkipsUnchangedAndPushesChangedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assert.Equal(t, 0, second.MessagesPushed)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha changed", modifiedAt)
	third, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, third.SessionsPushed)
	assert.Equal(t, 1, third.MessagesPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		fixture.alphaID,
	).Scan(&content))
	assert.Equal(t, "alpha changed", content)
}

func TestSyncIncrementalPushesSameLengthContentChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	sessionID := "duck-sync-same-length"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         syncSession(sessionID, "alpha", "first", "2026-01-18T00:00:00.000Z", 1),
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", "abcde", "2026-01-18T00:00:00.000Z")},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, sessionID, "vwxyz", modifiedAt)
	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		sessionID,
	).Scan(&content))
	assert.Equal(t, "vwxyz", content)
}

func TestSyncIncrementalRechecksLastPushBoundary(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	boundary := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha boundary", boundary)
	require.NoError(t, local.SetSyncState(lastPushStateKey, boundary))
	require.NoError(t, local.SetSyncState(lastPushBoundaryStateKey, ""))

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)
	assert.Equal(t, 1, second.MessagesPushed)

	var content string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT content FROM messages WHERE session_id = ? AND ordinal = 0`,
		fixture.alphaID,
	).Scan(&content))
	assert.Equal(t, "alpha boundary", content)
}

func TestSyncResetTargetForcesFullPushWhenWatermarkExists(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	_, err = syncer.DB().ExecContext(ctx, `DELETE FROM sessions`)
	require.NoError(t, err)

	reset, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, reset.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
}

func TestSyncResetTargetIgnoresOtherMachineRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})
	require.NoError(t, syncer.EnsureSchema(ctx))
	insertOtherMachineDuckSession(t, syncer.DB())
	require.NoError(t, local.SetSyncState(
		lastPushStateKey,
		time.Now().UTC().Format(localSyncTimestampLayout),
	))

	reset, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, reset.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "machine = ?", "test-machine", 2)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "machine = ?", "other-machine", 1)
}

func TestSyncFullPushRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	require.NoError(t, local.SoftDeleteSession(fixture.betaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, second.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCount(t, syncer.DB(), "messages", 2)
}

func TestSyncFullPushPreservesOtherMachineRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	insertOtherMachineDuckSession(t, syncer.DB())

	second, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, second.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "starred_sessions", "session_id = ?", "other-session", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "pinned_messages", "session_id = ?", "other-session", 1)
}

func TestSyncIncrementalPushRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	require.NoError(t, local.SoftDeleteSession(fixture.betaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.betaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assertDuckDBCount(t, syncer.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "messages", "session_id = ?", fixture.betaID, 0)
}

func TestSyncIncrementalUpdatesPinsWithoutSessionChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "updated duck pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)

	var got string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		fixture.alphaID, msgs[0].ID,
	).Scan(&got))
	assert.Equal(t, note, got)
}

func TestClearSessionTablesRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)

	tx, err := syncer.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, clearSessionTables(ctx, tx))
	require.NoError(t, tx.Rollback())

	assertDuckDBCount(t, syncer.DB(), "sessions", 2)
	assertDuckDBCount(t, syncer.DB(), "messages", 3)
	assertDuckDBCount(t, syncer.DB(), "usage_events", 1)
}

func TestSyncProjectFiltersMatchPushScope(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)

	include := newTestSync(t,
		filepath.Join(t.TempDir(), "include.duckdb"),
		local, SyncOptions{Projects: []string{"alpha"}})
	result, err := include.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assertDuckDBCount(t, include.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, include.DB(), "sessions", "project = ?", "alpha", 1)

	exclude := newTestSync(t,
		filepath.Join(t.TempDir(), "exclude.duckdb"),
		local, SyncOptions{ExcludeProjects: []string{"alpha"}})
	result, err = exclude.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assertDuckDBCount(t, exclude.DB(), "sessions", 1)
	assertDuckDBCountWhere(t, exclude.DB(), "sessions", "project = ?", "beta", 1)
}

func TestSyncFilteredFullClearsGlobalWatermarkForLaterUnfilteredPush(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	ok, err := local.StarSession(fixture.betaID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))
	require.NoError(t, local.SetSyncState(lastPushBoundaryStateKey, `{"fingerprints":{"stale":"stale"}}`))

	target := filepath.Join(t.TempDir(), "filtered.duckdb")
	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	first, err := filtered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	assertDuckDBCountWhere(t, filtered.DB(), "sessions", "id = ?", fixture.alphaID, 1)
	assertDuckDBCountWhere(t, filtered.DB(), "sessions", "id = ?", fixture.betaID, 0)
	assertDuckDBCount(t, filtered.DB(), "starred_sessions", 1)
	assertDuckDBCountWhere(t, filtered.DB(), "starred_sessions", "session_id = ?", fixture.betaID, 0)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)
	fingerprints, err := readSyncFingerprints(local)
	require.NoError(t, err)
	assert.Contains(t, fingerprints, fixture.alphaID)
	assert.NotContains(t, fingerprints, "stale")
	require.NoError(t, filtered.Close())

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	second, err := unfiltered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, second.SessionsPushed)
	assertDuckDBCountWhere(t, unfiltered.DB(), "sessions", "id = ?", fixture.betaID, 1)
}

func TestSyncFilteredFullPushKeepsNextFilteredPushIncremental(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "filtered-incremental.duckdb"),
		local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assert.Equal(t, 0, second.MessagesPushed)

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Empty(t, watermark)
}

func TestSyncFilteredIncrementalPersistsFingerprintsWithoutAdvancingWatermark(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	target := filepath.Join(t.TempDir(), "filtered-watermark.duckdb")

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	first, err := unfiltered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	require.NoError(t, unfiltered.Close())

	watermark, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	require.NotEmpty(t, watermark)

	time.Sleep(time.Millisecond)
	modifiedAt := time.Now().UTC().Format(localSyncTimestampLayout)
	updateSession(t, local, fixture.alphaID, "alpha filtered change", modifiedAt)

	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	second, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, second.SessionsPushed)

	got, err := local.GetSyncState(lastPushStateKey)
	require.NoError(t, err)
	assert.Equal(t, watermark, got)

	third, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, third.SessionsPushed)
}

func TestSyncFilteredIncrementalUpdatesPinsWithoutSessionChange(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "filtered-pins.duckdb"),
		local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))

	msgs, err := local.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	note := "filtered updated pin"
	_, err = local.PinMessage(fixture.alphaID, msgs[0].ID, &note)
	require.NoError(t, err)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)

	var got string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT note FROM pinned_messages WHERE session_id = ? AND message_id = ?`,
		fixture.alphaID, msgs[0].ID,
	).Scan(&got))
	assert.Equal(t, note, got)
}

func TestSyncFilteredIncrementalRemovesHardDeletedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "filtered-delete.duckdb"),
		local, SyncOptions{Projects: []string{"alpha"}})

	first, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, first.SessionsPushed)
	require.NoError(t, local.SetSyncState(lastPushStateKey, time.Now().UTC().Format(localSyncTimestampLayout)))

	require.NoError(t, local.SoftDeleteSession(fixture.alphaID))
	deleted, err := local.DeleteSessionIfTrashed(fixture.alphaID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	second, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second.SessionsPushed)
	assertDuckDBCountWhere(t, syncer.DB(), "sessions", "id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_calls", "session_id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "tool_result_events", "session_id = ?", fixture.alphaID, 0)
	assertDuckDBCountWhere(t, syncer.DB(), "starred_sessions", "session_id = ?", fixture.alphaID, 0)
}

func TestSyncFilteredPushPreservesOutOfScopeStarredSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	fixture := seedDuckDBSyncFixture(t, local)
	ok, err := local.StarSession(fixture.betaID)
	require.NoError(t, err)
	require.True(t, ok)
	target := filepath.Join(t.TempDir(), "filtered-stars.duckdb")

	unfiltered := newTestSync(t, target, local, SyncOptions{})
	first, err := unfiltered.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	assertDuckDBCount(t, unfiltered.DB(), "starred_sessions", 2)
	require.NoError(t, unfiltered.Close())

	filtered := newTestSync(t, target, local, SyncOptions{Projects: []string{"alpha"}})
	second, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	assertDuckDBCount(t, filtered.DB(), "starred_sessions", 2)
	assertDuckDBCountWhere(t, filtered.DB(), "starred_sessions", "session_id = ?", fixture.betaID, 1)
	assert.Equal(t, 0, second.SessionsPushed)
}

func TestSyncStatusCountsDuckDBRows(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	insertOtherMachineDuckSession(t, syncer.DB())

	status, err := syncer.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-machine", status.Machine)
	assert.NotEmpty(t, status.LastPushAt)
	assert.Equal(t, 2, status.DuckDBSessions)
	assert.Equal(t, 3, status.DuckDBMessages)
}

type syncFixture struct {
	alphaID string
	betaID  string
}

func newLocalDB(t *testing.T) *db.DB {
	t.Helper()
	local, err := db.Open(filepath.Join(t.TempDir(), "local.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, local.Close())
	})
	return local
}

func newTestSync(
	t *testing.T, path string, local *db.DB, opts SyncOptions,
) *Sync {
	t.Helper()
	syncer, err := New(path, local, "test-machine", opts)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, syncer.Close())
	})
	return syncer
}

func insertOtherMachineDuckSession(t *testing.T, duck *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := duck.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent,
			message_count, user_message_count, relationship_type, created_at
		) VALUES (
			'other-session', 'alpha', 'other-machine', 'claude',
			1, 1, '', current_timestamp
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp
		) VALUES (
			2, 'other-session', 0, 'assistant', 'from other machine',
			current_timestamp
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category,
			call_index, tool_use_id, input_json
		) VALUES (
			9001, 2, 'other-session', 'wrong-session-tool', 'other',
			0, 'other-tool-use', '{"cmd":"wrong-session-tool"}'
		)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO starred_sessions (session_id, created_at)
		VALUES ('other-session', current_timestamp)`)
	require.NoError(t, err)
	_, err = duck.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			id, session_id, message_id, ordinal, note, created_at
		) VALUES (
			9001, 'other-session', 2, 0, 'other pin', current_timestamp
		)`)
	require.NoError(t, err)
}

func seedDuckDBSyncFixture(t *testing.T, local *db.DB) syncFixture {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-test",
		InputPerMTok:         3,
		OutputPerMTok:        15,
		CacheCreationPerMTok: 1,
		CacheReadPerMTok:     0.5,
	}}))
	alphaID := "duck-sync-alpha"
	betaID := "duck-sync-beta"
	alphaSecret := "secret token sk-duckdb"
	callIndex := 0
	writes := []db.SessionBatchWrite{
		{
			Session: syncSession(alphaID, "alpha", "alpha first", "2026-01-10T00:00:00.000Z", 2),
			Messages: []db.Message{
				syncMessage(alphaID, 0, "user", "alpha first", "2026-01-10T00:00:00.000Z"),
				syncMessage(alphaID, 1, "assistant", alphaSecret, "2026-01-10T00:01:00.000Z",
					db.ToolCall{
						ToolName:  "search",
						Category:  "search",
						ToolUseID: "tool-alpha",
						InputJSON: `{"query":"duck"}`,
						ResultEvents: []db.ToolResultEvent{{
							Source:        "tool",
							Status:        "complete",
							Content:       "duck result",
							Timestamp:     "2026-01-10T00:01:30.000Z",
							EventIndex:    0,
							ContentLength: len("duck result"),
						}},
					}),
			},
			UsageEvents: []db.UsageEvent{{
				Source:       "hermes",
				Model:        "claude-test",
				InputTokens:  10,
				OutputTokens: 5,
				OccurredAt:   "2026-01-10T00:02:00.000Z",
				DedupKey:     "alpha-usage",
			}},
			Findings: []db.SecretFinding{{
				SessionID:      alphaID,
				RuleName:       "test_secret",
				Confidence:     "definite",
				LocationKind:   "message",
				MessageOrdinal: 1,
				CallIndex:      &callIndex,
				MatchStart:     len("secret token "),
				MatchEnd:       len(alphaSecret),
				MatchIndex:     0,
				RedactedMatch:  "sk-duckdb...",
				RulesVersion:   "test-rules",
			}},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(betaID, "beta", "beta first", "2026-01-11T00:00:00.000Z", 1),
			Messages: []db.Message{
				syncMessage(betaID, 0, "user", "beta first", "2026-01-11T00:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	ok, err := local.StarSession(alphaID)
	require.NoError(t, err)
	require.True(t, ok)
	msgs, err := local.GetAllMessages(ctx, alphaID)
	require.NoError(t, err)
	note := "pin alpha"
	_, err = local.PinMessage(alphaID, msgs[0].ID, &note)
	require.NoError(t, err)
	return syncFixture{alphaID: alphaID, betaID: betaID}
}

func syncSession(id, project, first, ts string, messageCount int) db.Session {
	firstValue := first
	startedAt := ts
	endedAt := ts
	localModifiedAt := ts
	return db.Session{
		ID:                id,
		Project:           project,
		Machine:           "local",
		Agent:             "claude",
		FirstMessage:      &firstValue,
		StartedAt:         &startedAt,
		EndedAt:           &endedAt,
		CreatedAt:         ts,
		LocalModifiedAt:   &localModifiedAt,
		MessageCount:      messageCount,
		UserMessageCount:  1,
		RelationshipType:  "root",
		Outcome:           "success",
		OutcomeConfidence: "high",
		EndedWithRole:     "assistant",
		DataVersion:       1,
	}
}

func syncMessage(
	sessionID string, ordinal int, role, content, ts string,
	calls ...db.ToolCall,
) db.Message {
	msg := db.Message{
		SessionID:        sessionID,
		Ordinal:          ordinal,
		Role:             role,
		Content:          content,
		Timestamp:        ts,
		ContentLength:    len(content),
		HasToolUse:       len(calls) > 0,
		ToolCalls:        calls,
		Model:            "claude-test",
		TokenUsage:       []byte(`{"input_tokens":1,"output_tokens":2}`),
		ContextTokens:    1,
		OutputTokens:     2,
		HasContextTokens: true,
		HasOutputTokens:  true,
	}
	return msg
}

func updateSession(t *testing.T, local *db.DB, sessionID, content, modifiedAt string) {
	t.Helper()
	sess := syncSession(sessionID, "alpha", content, "2026-01-10T00:00:00.000Z", 1)
	localModifiedAt := modifiedAt
	sess.LocalModifiedAt = &localModifiedAt
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{syncMessage(sessionID, 0, "user", content, modifiedAt)},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

func assertDuckDBCount(t *testing.T, conn *sql.DB, table string, want int) {
	t.Helper()
	var got int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&got))
	assert.Equal(t, want, got, table)
}

func assertDuckDBCountWhere(
	t *testing.T, conn *sql.DB, table, where string, arg any, want int,
) {
	t.Helper()
	var got int
	require.NoError(t, conn.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE `+where, arg,
	).Scan(&got))
	assert.Equal(t, want, got, table)
}

func TestSyncResultDurationIsSet(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "mirror.duckdb"),
		local, SyncOptions{})

	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Greater(t, result.Duration, time.Duration(0))
}
