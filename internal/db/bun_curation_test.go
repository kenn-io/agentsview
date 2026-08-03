package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type writableCurationTestBackend struct {
	store bun.IDB
}

func (*writableCurationTestBackend) Name() string { return "curation-test" }

func (*writableCurationTestBackend) ReadOnly() bool { return false }

func (*writableCurationTestBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{Writes: map[WriteOperation]bool{
		WriteCuration:      true,
		WriteInsight:       true,
		WriteInsightDelete: true,
	}}
}

func (*writableCurationTestBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
}

func (*writableCurationTestBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *writableCurationTestBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store)
}

func (b *writableCurationTestBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store)
}

func (b *writableCurationTestBackend) Update(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.store)
}

func TestBunCurationWritesSupplyCanonicalCreationTime(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "writer-archive", SourceArchiveSalt: "writer-salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	_, err = store.NewInsert().Model(&bunmodel.Session{
		ID: "writer-session", Project: "writer", Machine: "host", Agent: "codex",
		CreatedAt: created, SourceArchiveID: "writer-archive",
		SourceDatabaseGeneration: "writer-generation",
	}).Exec(t.Context())
	require.NoError(t, err)
	messageID := int64(81)
	_, err = store.NewInsert().Model(&bunmodel.Message{
		ID: &messageID, SessionID: "writer-session", Ordinal: 1,
		Role: "assistant", Content: "canonical writer",
	}).Exec(t.Context())
	require.NoError(t, err)

	common := NewBunStore(&writableCurationTestBackend{store: store})
	starred, err := common.StarSession("writer-session")
	require.NoError(t, err)
	assert.True(t, starred)
	pinID, err := common.PinMessage("writer-session", messageID, nil)
	require.NoError(t, err)
	assert.Positive(t, pinID)
}

func TestBunInsightWriteSuppliesCanonicalCreationTime(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	common := NewBunStore(&writableCurationTestBackend{store: store})

	id, err := common.InsertInsight(Insight{
		Type: "daily_activity", DateFrom: "2026-08-03", DateTo: "2026-08-03",
		Agent: "codex", Content: "canonical insight writer",
	})
	require.NoError(t, err)
	assert.Positive(t, id)
	got, err := common.GetInsight(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.CreatedAt)
}

func TestUpsertPinnedMessageRowsGeneratesTargetIDOnSourceCollision(t *testing.T) {
	database := testDB(t)
	for _, sessionID := range []string{"target-pin", "replicated-pin", "generated-pin"} {
		insertSession(t, database, sessionID, "curation")
		insertMessages(t, database, asstMsg(sessionID, 0, sessionID))
	}
	targetMessages, err := database.GetMessages(
		t.Context(), "target-pin", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, targetMessages, 1)
	targetPinID, err := database.PinMessage("target-pin", targetMessages[0].ID, nil)
	require.NoError(t, err)
	assert.Positive(t, targetPinID)

	replicatedMessages, err := database.GetMessages(
		t.Context(), "replicated-pin", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, replicatedMessages, 1)
	replicatedMessageID := replicatedMessages[0].ID
	require.NoError(t, UpsertPinnedMessageRows(
		t.Context(), database.bunWriter, []bunmodel.PinnedMessage{{
			ID: targetPinID, SessionID: "replicated-pin",
			MessageID: &replicatedMessageID, Ordinal: replicatedMessages[0].Ordinal,
			CreatedAt: bunmodel.NewTimestamp(
				time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC),
			),
		}}, GeneratePinRowIDs,
	))
	replicatedPins, err := database.ListPinnedMessages(
		t.Context(), "replicated-pin", "",
	)
	require.NoError(t, err)
	require.Len(t, replicatedPins, 1)
	assert.NotEqual(t, targetPinID, replicatedPins[0].ID)

	generatedMessages, err := database.GetMessages(
		t.Context(), "generated-pin", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, generatedMessages, 1)
	generatedPinID, err := database.PinMessage(
		"generated-pin", generatedMessages[0].ID, nil,
	)
	require.NoError(t, err)
	assert.NotEqual(t, targetPinID, generatedPinID)
	assert.NotEqual(t, replicatedPins[0].ID, generatedPinID)
}

func TestBunCurationRowUpsertsRefreshCanonicalKeys(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	first := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	second := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC))
	archive := bunmodel.SourceArchive{
		SourceArchiveID: "curation-upsert-archive", SourceArchiveSalt: "salt",
	}
	_, err = store.NewInsert().Model(&archive).Exec(t.Context())
	require.NoError(t, err)
	const rootOldID = "curation-upsert-old"
	const rootNewID = "curation-upsert-new"
	for _, sessionID := range []string{rootOldID, rootNewID} {
		session := bunmodel.Session{
			ID: sessionID, Project: "curation", Machine: "host", Agent: "codex",
			CreatedAt: first, SourceArchiveID: archive.SourceArchiveID,
			SourceDatabaseGeneration: "curation-upsert-generation",
		}
		_, err = store.NewInsert().Model(&session).Exec(t.Context())
		require.NoError(t, err)
	}
	messageID := int64(802)
	message := bunmodel.Message{
		ID: &messageID, SessionID: rootNewID, Ordinal: 1,
		Role: "assistant", Content: "curation", Timestamp: &first,
	}
	_, err = store.NewInsert().Model(&message).Exec(t.Context())
	require.NoError(t, err)

	require.NoError(t, UpsertStarredSessionRows(t.Context(), store,
		[]bunmodel.StarredSession{
			{SessionID: rootOldID, CreatedAt: first},
			{SessionID: rootNewID, CreatedAt: first},
		},
	))
	require.NoError(t, UpsertStarredSessionRows(t.Context(), store,
		[]bunmodel.StarredSession{{SessionID: rootNewID, CreatedAt: second}},
	))
	var stars []bunmodel.StarredSession
	require.NoError(t, store.NewSelect().Model(&stars).
		OrderExpr("session_id ASC").Scan(t.Context()))
	require.Len(t, stars, 2)
	assert.Equal(t, rootNewID, stars[0].SessionID)
	assert.Equal(t, second.Time, stars[0].CreatedAt.Time)

	initialNote := "initial replicated pin"
	require.NoError(t, UpsertPinnedMessageRows(t.Context(), store,
		[]bunmodel.PinnedMessage{{
			ID: 3001, SessionID: rootNewID, MessageID: &messageID,
			Ordinal: 1, SourceUUID: "source-one", Note: &initialNote,
			CreatedAt: first,
		}}, GeneratePinRowIDs,
	))
	var initialPin bunmodel.PinnedMessage
	require.NoError(t, store.NewSelect().Model(&initialPin).
		Where("session_id = ?", rootNewID).
		Where("ordinal = 1").Scan(t.Context()))
	assert.Positive(t, initialPin.ID)
	assert.NotEqual(t, int64(3001), initialPin.ID,
		"the target generates new replicated pin identities")
	updatedNote := "updated replicated pin"
	require.NoError(t, UpsertPinnedMessageRows(t.Context(), store,
		[]bunmodel.PinnedMessage{{
			ID: 9999, SessionID: rootNewID, MessageID: &messageID,
			Ordinal: 1, SourceUUID: "source-two", Note: &updatedNote,
			CreatedAt: second,
		}}, GeneratePinRowIDs,
	))
	var pin bunmodel.PinnedMessage
	require.NoError(t, store.NewSelect().Model(&pin).
		Where("session_id = ?", rootNewID).
		Where("ordinal = 1").Scan(t.Context()))
	assert.Equal(t, initialPin.ID, pin.ID,
		"the target's generated pin identity stays stable on conflict")
	assert.Equal(t, "source-two", pin.SourceUUID)
	require.NotNil(t, pin.Note)
	assert.Equal(t, updatedNote, *pin.Note)
	assert.Equal(t, second.Time, pin.CreatedAt.Time)
}

func TestUpsertPinnedMessageRowsPreservesSourceIDForMirrorPolicy(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "mirror-archive", SourceArchiveSalt: "mirror-salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	_, err = store.NewInsert().Model(&bunmodel.Session{
		ID: "mirror-session", Project: "mirror", Machine: "host", Agent: "codex",
		CreatedAt: created, SourceArchiveID: "mirror-archive",
		SourceDatabaseGeneration: "mirror-generation",
	}).Exec(t.Context())
	require.NoError(t, err)
	messageID := int64(501)
	_, err = store.NewInsert().Model(&bunmodel.Message{
		ID: &messageID, SessionID: "mirror-session", Ordinal: 1,
		Role: "assistant", Content: "mirror pin",
	}).Exec(t.Context())
	require.NoError(t, err)

	require.NoError(t, UpsertPinnedMessageRows(
		t.Context(), store, []bunmodel.PinnedMessage{{
			ID: 7001, SessionID: "mirror-session", MessageID: &messageID,
			Ordinal: 1, CreatedAt: created,
		}}, PreservePinRowIDs,
	))
	require.NoError(t, UpsertPinnedMessageRows(
		t.Context(), store, []bunmodel.PinnedMessage{{
			ID: 7002, SessionID: "mirror-session", MessageID: &messageID,
			Ordinal: 1, CreatedAt: created,
		}}, PreservePinRowIDs,
	))
	var pin bunmodel.PinnedMessage
	require.NoError(t, store.NewSelect().Model(&pin).
		Where("session_id = ?", "mirror-session").Scan(t.Context()))
	assert.Equal(t, int64(7002), pin.ID,
		"a replacement mirror row adopts the current source identity")
}

func TestUpsertPinnedMessageRowsRollsBackGeneratedBatch(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "rollback-pin", "curation")
	insertMessages(t, database, asstMsg("rollback-pin", 0, "valid pin"))
	messages, err := database.GetMessages(
		t.Context(), "rollback-pin", 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	validMessageID := messages[0].ID
	missingMessageID := int64(999_999)
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC))

	err = UpsertPinnedMessageRows(
		t.Context(), database.bunWriter, []bunmodel.PinnedMessage{
			{
				ID: 8001, SessionID: "rollback-pin", MessageID: &validMessageID,
				Ordinal: 0, CreatedAt: created,
			},
			{
				ID: 8002, SessionID: "missing-session", MessageID: &missingMessageID,
				Ordinal: 0, CreatedAt: created,
			},
		}, GeneratePinRowIDs,
	)
	require.Error(t, err)
	pins, readErr := database.ListPinnedMessages(t.Context(), "rollback-pin", "")
	require.NoError(t, readErr)
	assert.Empty(t, pins)
}
