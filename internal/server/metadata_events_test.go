package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type recordedMetadataEvent struct {
	Version    int                  `json:"v"`
	HLC        string               `json:"hlc"`
	Origin     string               `json:"origin"`
	SessionGID string               `json:"session_gid"`
	Op         string               `json:"op"`
	Value      map[string]any       `json:"value,omitempty"`
	Pin        *recordedMetadataPin `json:"pin,omitempty"`
}

type recordedMetadataPin struct {
	SourceUUID string  `json:"source_uuid,omitempty"`
	Ordinal    int     `json:"ordinal,omitempty"`
	Note       *string `json:"note,omitempty"`
}

func withArtifactOrigin(origin string) setupOption {
	return func(c *config.Config) { c.ArtifactOriginID = origin }
}

func TestMetadataEventsAppendForUserMutations(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	te.seedMessages(t, "s1", 2, func(i int, m *db.Message) {
		if i == 1 {
			m.SourceUUID = "uuid-answer"
		}
	})
	msgs, err := te.db.GetAllMessages(context.Background(), "s1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	messageID := msgs[1].ID

	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"Pinned investigation"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.post(t, fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID), `{"note":"remember"}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	w = te.del(t, fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID))
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 9)
	assert.Equal(t, []string{
		artifact.MetadataOpRename,
		artifact.MetadataOpStar,
		artifact.MetadataOpUnstar,
		artifact.MetadataOpPin,
		artifact.MetadataOpUnpin,
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpRestore,
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpPurge,
	}, metadataOps(events))
	for _, event := range events {
		assert.Equal(t, 1, event.Version)
		assert.NotEmpty(t, event.HLC)
		assert.Equal(t, "desk-a1b2c3", event.Origin)
		assert.Equal(t, "desk-a1b2c3~s1", event.SessionGID)
	}
	assert.Equal(t, "Pinned investigation", events[0].Value["display_name"])
	require.NotNil(t, events[3].Pin)
	assert.Equal(t, "uuid-answer", events[3].Pin.SourceUUID)
	assert.Equal(t, 1, events[3].Pin.Ordinal)
	require.NotNil(t, events[3].Pin.Note)
	assert.Equal(t, "remember", *events[3].Pin.Note)
	require.NotNil(t, events[4].Pin)
	assert.Equal(t, "uuid-answer", events[4].Pin.SourceUUID)
	assert.Equal(t, 1, events[4].Pin.Ordinal)
	assert.Nil(t, events[4].Pin.Note)
}

func TestMetadataEventsAppendWithoutSyncEngine(t *testing.T) {
	te := setupNoSyncMode(t)
	require.NoError(t, artifact.AdoptOrigin(te.db, "nosync-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"No sync title"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpRename, events[0].Op)
	assert.Equal(t, "nosync-a1b2c3", events[0].Origin)
	assert.Equal(t, "nosync-a1b2c3~s1", events[0].SessionGID)
}

func TestMetadataEventsNotRecordedWithoutOptIn(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"Local only"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	assert.Empty(t, readMetadataEvents(t, te),
		"curation must not write ledger events before the machine opts into artifact sync")
	origin, err := artifact.StoredOrigin(te.db)
	require.NoError(t, err)
	assert.Empty(t, origin,
		"curation must not mint an artifact origin before opt-in")
}

func TestMetadataEventsEmptyTrashStaysLocalOnly(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	for _, id := range []string{"s1", "s2"} {
		te.seedSession(t, id, "alpha", 2)
		w := te.del(t, "/api/v1/sessions/"+id)
		require.Equal(t, http.StatusNoContent, w.Code, "delete %s body: %s", id, w.Body.String())
	}

	w := te.del(t, "/api/v1/trash")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 2)
	assert.Equal(t, []string{
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpSoftDelete,
	}, metadataOps(events))
}

func TestMetadataEventsBatchDeleteRecordsNewAndUnrecordedTrashedSessions(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	for _, id := range []string{"s1", "s2", "s3"} {
		te.seedSession(t, id, "alpha", 2)
	}
	require.NoError(t, te.db.SoftDeleteSession("s3"))

	w := te.requestJSON(t, http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["s1","s2","s3","missing"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 3)
	assert.Equal(t, []string{
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpSoftDelete,
	}, metadataOps(events))
	assert.ElementsMatch(t, []string{
		"desk-a1b2c3~s1",
		"desk-a1b2c3~s2",
		"desk-a1b2c3~s3",
	}, []string{events[0].SessionGID, events[1].SessionGID, events[2].SessionGID})
}

func TestMetadataEventsBatchDeleteRetriesAlreadyDeletedSessions(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	for _, id := range []string{"s1", "s2"} {
		te.seedSession(t, id, "alpha", 2)
		require.NoError(t, te.db.SoftDeleteSession(id))
	}

	w := te.requestJSON(t, http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["s1","s2","missing"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 2)
	assert.Equal(t, []string{
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpSoftDelete,
	}, metadataOps(events))
	assert.ElementsMatch(t, []string{
		"desk-a1b2c3~s1",
		"desk-a1b2c3~s2",
	}, []string{events[0].SessionGID, events[1].SessionGID})
}

func TestMetadataEventsBatchDeleteRepairsPublishedFailureOnRetry(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	for _, id := range []string{"s1", "s2"} {
		te.seedSession(t, id, "alpha", 2)
	}
	execTestDDL(t, te, `
CREATE TRIGGER fail_s1_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
WHEN NEW.session_gid = 'desk-a1b2c3~s1'
BEGIN
	SELECT RAISE(FAIL, 'forced s1 metadata replay failure');
END;
`)

	w := te.requestJSON(t, http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["s1","s2"]}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	s1, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, s1)
	assert.NotNil(t, s1.DeletedAt,
		"published failure keeps the session whose artifact is durable in trash")
	s2, err := te.db.GetSessionFull(context.Background(), "s2")
	require.NoError(t, err)
	require.NotNil(t, s2)
	assert.Nil(t, s2.DeletedAt,
		"later unpublished sessions are restored after the failure")
	assert.Equal(t, 0, serverMetadataTableCount(
		t, te, "metadata_replay_state",
		"session_gid = 'desk-a1b2c3~s1' AND field = 'deleted_at'",
	))
	firstEvents := readMetadataEvents(t, te)
	require.Len(t, firstEvents, 1)
	assert.Equal(t, "desk-a1b2c3~s1", firstEvents[0].SessionGID)

	execTestDDL(t, te, `DROP TRIGGER fail_s1_metadata_replay_state_insert`)
	w = te.requestJSON(t, http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["s1","s2"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	for _, id := range []string{"s1", "s2"} {
		sess, getErr := te.db.GetSessionFull(context.Background(), id)
		require.NoError(t, getErr)
		require.NotNil(t, sess)
		assert.NotNil(t, sess.DeletedAt, "retry must trash %s", id)
		assert.Equal(t, artifact.MetadataOpSoftDelete,
			serverMetadataReplayOp(t, te, "desk-a1b2c3~"+id, "deleted_at"))
	}
	events := readMetadataEvents(t, te)
	require.Len(t, events, 2,
		"retry must repair the first artifact instead of publishing a duplicate")
	counts := map[string]int{}
	for _, event := range events {
		counts[event.SessionGID]++
	}
	assert.Equal(t, map[string]int{
		"desk-a1b2c3~s1": 1,
		"desk-a1b2c3~s2": 1,
	}, counts)
}

func TestMetadataEventsUnstarOnlyRecordsRemovedStars(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/missing/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, readMetadataEvents(t, te))

	ok, err := te.db.StarSession("s1")
	require.NoError(t, err)
	require.True(t, ok)
	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpUnstar, events[0].Op)
	assert.Equal(t, "desk-a1b2c3~s1", events[0].SessionGID)
}

func TestMetadataEventsUnstarRestoresStarWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	ok, err := te.db.StarSession("s1")
	require.NoError(t, err)
	require.True(t, ok)

	breakMetadataArtifactStore(t, te)

	w := te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids)
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_replay_state", "session_gid = 'desk-a1b2c3~s1'"))
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_applied_events", "origin = 'desk-a1b2c3'"))

	repairMetadataArtifactStore(t, te)
	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	ids, err = te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids)

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpUnstar, events[0].Op)
	assert.Equal(t, "desk-a1b2c3~s1", events[0].SessionGID)
}

func TestMetadataEventsUnstarDoesNotRestoreStarWhenArtifactPublished(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	ok, err := te.db.StarSession("s1")
	require.NoError(t, err)
	require.True(t, ok)

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w := te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids)
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_replay_state", "session_gid = 'desk-a1b2c3~s1'"))
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_applied_events", "origin = 'desk-a1b2c3'"))

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpUnstar, events[0].Op)
	assert.Equal(t, "desk-a1b2c3~s1", events[0].SessionGID)
}

func TestMetadataEventsNoopUnstarRepairsPublishedArtifactState(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpStar,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "starred"))

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpStar,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "starred"))
	unstarOrderKey := metadataEventOrderKey(t, te, artifact.MetadataOpUnstar)

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpUnstar,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "starred"))

	remoteHLC, remoteHash := splitMetadataOrderKey(t, unstarOrderKey)
	_, err := te.db.ApplyMetadataProjection(context.Background(), db.MetadataProjection{
		EventOrigin:    "peer-b2c3d4",
		OrderKey:       unstarOrderKey,
		HLC:            remoteHLC,
		ArtifactHash:   remoteHash,
		SessionGID:     "desk-a1b2c3~s1",
		LocalSessionID: "s1",
		Field:          "starred",
		Op:             artifact.MetadataOpStar,
		Value:          artifact.MetadataOpStar,
	})
	require.NoError(t, err)
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestMetadataEventsStarRollsBackWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	breakMetadataArtifactStore(t, te)

	w := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids, "failed metadata publish must roll back the star")

	repairMetadataArtifactStore(t, te)
	w = te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpStar, events[0].Op)

	// A pre-existing star survives a later failed re-star: the failed
	// append changed nothing this request needs to undo.
	// (breakMetadataArtifactStore also wipes recorded events.)
	breakMetadataArtifactStore(t, te)
	w = te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err = te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids,
		"failed re-star must not remove the pre-existing star")
}

func TestMetadataEventsStarKeepsStarWhenArtifactPublished(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids,
		"published metadata event must keep the local star")

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpStar, events[0].Op)
}

func TestMetadataEventsBulkStarRollsBackWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "alpha", 2)
	ok, err := te.db.StarSession("s1")
	require.NoError(t, err)
	require.True(t, ok)

	breakMetadataArtifactStore(t, te)
	w := te.requestJSON(t, http.MethodPost, "/api/v1/starred/bulk",
		`{"session_ids":["s1","s2"]}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids,
		"failed metadata publish must roll back only the stars this request created")
	assert.Empty(t, readMetadataEvents(t, te))
}

func TestMetadataEventsRenameRestoresNameWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"keep"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	breakMetadataArtifactStore(t, te)
	w = te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"replace"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	session, err := te.db.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "keep", *session.DisplayName,
		"failed metadata publish must restore the prior display name")
	// breakMetadataArtifactStore also wiped the first rename's event, so
	// no events remain after the rolled-back rename.
	assert.Empty(t, readMetadataEvents(t, te))
}

func TestMetadataEventsDeleteRestoresSessionWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)
	breakMetadataArtifactStore(t, te)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	session, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.DeletedAt,
		"failed metadata publish must restore the soft-deleted session")

	repairMetadataArtifactStore(t, te)
	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpSoftDelete, events[0].Op)
}

func seedPinTestMessage(t *testing.T, te *testEnv) int64 {
	t.Helper()
	te.seedSession(t, "s1", "alpha", 2)
	te.seedMessages(t, "s1", 2)
	msgs, err := te.db.GetAllMessages(context.Background(), "s1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	return msgs[1].ID
}

func breakMetadataArtifactStore(t *testing.T, te *testEnv) {
	t.Helper()
	require.NotNil(t, te.artifactFault)
	for _, entry := range listMetadataEntries(t, te) {
		require.NoError(t, te.artifactStore.Trash(t.Context(), entry.Ref))
	}
	te.artifactFault.setCreateError(
		errors.New("forced metadata artifact write failure"),
	)
}

func repairMetadataArtifactStore(t *testing.T, te *testEnv) {
	t.Helper()
	require.NotNil(t, te.artifactFault)
	te.artifactFault.setCreateError(nil)
}

func TestMetadataEventsPinRollsBackWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	messageID := seedPinTestMessage(t, te)
	breakMetadataArtifactStore(t, te)
	pinPath := fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID)

	w := te.post(t, pinPath, `{"note":"remember"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	pins, err := te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	assert.Empty(t, pins, "failed metadata publish must roll back the pin")
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_replay_state", "session_gid = 'desk-a1b2c3~s1'"))
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_applied_events", "origin = 'desk-a1b2c3'"))

	repairMetadataArtifactStore(t, te)
	w = te.post(t, pinPath, `{"note":"remember"}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	pins, err = te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpPin, events[0].Op)
}

func TestMetadataEventsRepinRestoresPriorNoteWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	messageID := seedPinTestMessage(t, te)
	pinPath := fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID)

	w := te.post(t, pinPath, `{"note":"keep"}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	breakMetadataArtifactStore(t, te)
	w = te.post(t, pinPath, `{"note":"replace"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	pins, err := te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "keep", *pins[0].Note,
		"failed re-pin must restore the prior note")
}

func TestMetadataEventsUnpinRestoresPinWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	messageID := seedPinTestMessage(t, te)
	pinPath := fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID)

	w := te.post(t, pinPath, `{"note":"remember"}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	breakMetadataArtifactStore(t, te)
	w = te.del(t, pinPath)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	pins, err := te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1, "failed metadata publish must restore the pin")
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "remember", *pins[0].Note)

	repairMetadataArtifactStore(t, te)
	w = te.del(t, pinPath)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	pins, err = te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	assert.Empty(t, pins)

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpUnpin, events[0].Op)
}

func TestMetadataEventsPinKeepsPinWhenArtifactPublished(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	messageID := seedPinTestMessage(t, te)
	pinPath := fmt.Sprintf("/api/v1/sessions/s1/messages/%d/pin", messageID)

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w := te.post(t, pinPath, `{"note":"remember"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())

	pins, err := te.db.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1,
		"published metadata event must keep the local pin")

	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpPin, events[0].Op)
}

func TestMetadataEventsPermanentDeleteRetriesExcludedSession(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	got, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.True(t, te.db.IsSessionExcluded("s1"))
	deleteMetadataEventsByOp(t, te, artifact.MetadataOpPurge)

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	assert.Equal(t, []string{
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpPurge,
	}, metadataOps(events))
	assert.Equal(t, artifact.MetadataOpPurge,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "purge"))
}

func TestMetadataEventsPermanentDeleteRetainsSessionWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	breakMetadataArtifactStore(t, te)

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	session, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session, "purge must not remove the only local copy before publication")
	assert.NotNil(t, session.DeletedAt)
	assert.False(t, te.db.IsSessionExcluded("s1"))
	assert.Empty(t, readMetadataEvents(t, te))

	repairMetadataArtifactStore(t, te)
	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	session, err = te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, session)
	assert.True(t, te.db.IsSessionExcluded("s1"))
	events := readMetadataEvents(t, te)
	require.Len(t, events, 1)
	assert.Equal(t, artifact.MetadataOpPurge, events[0].Op)
}

func TestMetadataEventsRestoreRepairsPublishedArtifactState(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	restored, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.DeletedAt)
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	restoreOrderKey := metadataEventOrderKey(t, te, artifact.MetadataOpRestore)

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpRestore,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))

	restoreHLC, _ := splitMetadataOrderKey(t, restoreOrderKey)
	olderHash := strings.Repeat("0", 64)
	_, err = te.db.ApplyMetadataProjection(context.Background(), db.MetadataProjection{
		EventOrigin:    "peer-b2c3d4",
		OrderKey:       restoreHLC + "-" + olderHash,
		HLC:            restoreHLC,
		ArtifactHash:   olderHash,
		SessionGID:     "desk-a1b2c3~s1",
		LocalSessionID: "s1",
		Field:          "deleted_at",
		Op:             artifact.MetadataOpSoftDelete,
		Value:          artifact.MetadataOpSoftDelete,
	})
	require.NoError(t, err)
	restored, err = te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.DeletedAt)
}

func TestMetadataEventsRestoreRetrashesSessionWhenArtifactWriteFails(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	breakMetadataArtifactStore(t, te)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	session, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.NotNil(t, session.DeletedAt,
		"failed pre-publication restore must return the session to trash")
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	assert.Empty(t, readMetadataEvents(t, te))
}

func TestMetadataEventsRestoreRetriesWithoutPublishedArtifact(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	restored, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.DeletedAt)
	deleteMetadataEventsByOp(t, te, artifact.MetadataOpRestore)

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	events := readMetadataEvents(t, te)
	assert.Equal(t, []string{
		artifact.MetadataOpSoftDelete,
		artifact.MetadataOpRestore,
	}, metadataOps(events))
	assert.Equal(t, artifact.MetadataOpRestore,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
}

func TestMetadataEventsRestoreRetryPublishesWhenOlderRestoreLoses(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	olderRestore := metadataEventOrderKey(t, te, artifact.MetadataOpRestore)

	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))

	execTestDDL(t, te, `
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	for _, key := range metadataEventOrderKeys(t, te, artifact.MetadataOpRestore) {
		if key != olderRestore {
			deleteMetadataEventOrderKey(t, te, key)
		}
	}

	execTestDDL(t, te, `DROP TRIGGER fail_metadata_replay_state_insert`)
	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	assert.Equal(t, artifact.MetadataOpRestore,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	assert.Len(t, metadataEventOrderKeys(t, te, artifact.MetadataOpRestore), 2)
}

func TestMetadataEventsSuppressedDuringReplay(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	ctx := artifact.WithMetadataEventSuppression(context.Background())
	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/sessions/s1/rename",
		strings.NewReader(`{"display_name":"Replay name"}`),
	).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	renamed, err := te.db.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, renamed)
	require.NotNil(t, renamed.DisplayName)
	assert.Equal(t, "Replay name", *renamed.DisplayName)
	assert.Empty(t, readMetadataEvents(t, te))
}

// execTestDDL runs schema DDL (failure-injection triggers) on the test
// database through a short-lived write connection. The server's Reader() pool
// opens with mode=ro, so tests cannot install triggers through it.
func execTestDDL(t *testing.T, te *testEnv, stmt string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", "file:"+te.db.Path()+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	_, err = raw.Exec(stmt)
	require.NoError(t, err)
}

func readMetadataEvents(t *testing.T, te *testEnv) []recordedMetadataEvent {
	t.Helper()
	entries := listMetadataEntries(t, te)
	events := make([]recordedMetadataEvent, 0, len(entries))
	for _, entry := range entries {
		data := readArtifactEntry(t, te, entry)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event), "artifact %s", entry.Ref.Name)
		events = append(events, event)
	}
	return events
}

func listMetadataEntries(t *testing.T, te *testEnv) []artifact.Entry {
	t.Helper()
	if te.artifactStore == nil {
		return nil
	}
	origins := collectArtifactOrigins(t, te.artifactStore, 64)

	var entries []artifact.Entry
	for _, origin := range origins {
		entries = append(entries,
			collectArtifactEntries(t, te.artifactStore, origin, artifact.KindMeta, 64)...)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Ref.Origin != entries[j].Ref.Origin {
			return entries[i].Ref.Origin < entries[j].Ref.Origin
		}
		return entries[i].Ref.Name < entries[j].Ref.Name
	})
	return entries
}

func readArtifactEntry(
	t *testing.T, te *testEnv, entry artifact.Entry,
) []byte {
	t.Helper()
	_, reader, err := te.artifactStore.Open(t.Context(), entry.Ref)
	require.NoError(t, err)
	data, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	return data
}

func metadataEventOrderKey(t *testing.T, te *testEnv, op string) string {
	t.Helper()
	keys := metadataEventOrderKeys(t, te, op)
	require.NotEmpty(t, keys, "metadata event op %s not found", op)
	return keys[0]
}

func metadataEventOrderKeys(t *testing.T, te *testEnv, op string) []string {
	t.Helper()
	keys := make([]string, 0)
	for _, entry := range listMetadataEntries(t, te) {
		data := readArtifactEntry(t, te, entry)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		if event.Op == op {
			keys = append(keys, strings.TrimSuffix(entry.Ref.Name, ".json"))
		}
	}
	return keys
}

func splitMetadataOrderKey(t *testing.T, orderKey string) (string, string) {
	t.Helper()
	idx := strings.LastIndex(orderKey, "-")
	require.NotEqual(t, -1, idx, "order key %q missing hash suffix", orderKey)
	return orderKey[:idx], orderKey[idx+1:]
}

func deleteMetadataEventOrderKey(t *testing.T, te *testEnv, orderKey string) {
	t.Helper()
	var matches []artifact.Entry
	for _, entry := range listMetadataEntries(t, te) {
		if entry.Ref.Name == orderKey+".json" {
			matches = append(matches, entry)
		}
	}
	require.Len(t, matches, 1)
	require.NoError(t, te.artifactStore.Trash(t.Context(), matches[0].Ref))
}

func deleteMetadataEventsByOp(t *testing.T, te *testEnv, op string) {
	t.Helper()
	for _, entry := range listMetadataEntries(t, te) {
		data := readArtifactEntry(t, te, entry)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		if event.Op == op {
			require.NoError(t, te.artifactStore.Trash(t.Context(), entry.Ref))
		}
	}
}

func metadataOps(events []recordedMetadataEvent) []string {
	ops := make([]string, len(events))
	for i, event := range events {
		ops[i] = event.Op
	}
	return ops
}

func serverMetadataReplayOp(t *testing.T, te *testEnv, sessionGID, field string) string {
	t.Helper()
	var op string
	err := te.db.Reader().QueryRow(
		`SELECT op FROM metadata_replay_state WHERE session_gid = ? AND field = ?`,
		sessionGID, field,
	).Scan(&op)
	require.NoError(t, err)
	return op
}

func serverMetadataTableCount(t *testing.T, te *testEnv, table, where string) int {
	t.Helper()
	var count int
	err := te.db.Reader().QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&count)
	require.NoError(t, err)
	return count
}
