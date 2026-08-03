package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

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
		}},
	))
	updatedNote := "updated replicated pin"
	require.NoError(t, UpsertPinnedMessageRows(t.Context(), store,
		[]bunmodel.PinnedMessage{{
			ID: 9999, SessionID: rootNewID, MessageID: &messageID,
			Ordinal: 1, SourceUUID: "source-two", Note: &updatedNote,
			CreatedAt: second,
		}},
	))
	var pin bunmodel.PinnedMessage
	require.NoError(t, store.NewSelect().Model(&pin).
		Where("session_id = ?", rootNewID).
		Where("ordinal = 1").Scan(t.Context()))
	assert.Equal(t, int64(3001), pin.ID,
		"the target's generated pin identity stays stable on conflict")
	assert.Equal(t, "source-two", pin.SourceUUID)
	require.NotNil(t, pin.Note)
	assert.Equal(t, updatedNote, *pin.Note)
	assert.Equal(t, second.Time, pin.CreatedAt.Time)
}
