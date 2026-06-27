package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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

func TestMetadataEventsBatchDeleteRecordsChangedSessions(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	for _, id := range []string{"s1", "s2", "s3"} {
		te.seedSession(t, id, "alpha", 2)
	}
	require.NoError(t, te.db.SoftDeleteSession("s3"))

	w := te.requestJSON(t, http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["s1","s2","s3","missing"]}`)
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

	metaDir := filepath.Join(te.dataDir, "artifacts", "desk-a1b2c3", "meta")
	require.NoError(t, os.MkdirAll(filepath.Dir(metaDir), 0o755))
	require.NoError(t, os.WriteFile(metaDir, []byte("not a directory"), 0o644))

	w := te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	ids, err := te.db.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, ids)
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_replay_state", "session_gid = 'desk-a1b2c3~s1'"))
	assert.Equal(t, 0, serverMetadataTableCount(t, te, "metadata_applied_events", "origin = 'desk-a1b2c3'"))

	require.NoError(t, os.Remove(metaDir))
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

	_, err = te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

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

	_, err := te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpStar,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "starred"))
	unstarOrderKey := metadataEventOrderKey(t, te, artifact.MetadataOpUnstar)

	_, err = te.db.Reader().Exec(`DROP TRIGGER fail_metadata_replay_state_insert`)
	require.NoError(t, err)
	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpUnstar,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "starred"))

	remoteHLC, remoteHash := splitMetadataOrderKey(t, unstarOrderKey)
	_, err = te.db.ApplyMetadataProjection(context.Background(), db.MetadataProjection{
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

func TestMetadataEventsPermanentDeleteRetriesExcludedSession(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	_, err := te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	got, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.True(t, te.db.IsSessionExcluded("s1"))
	deleteMetadataEventsByOp(t, te, artifact.MetadataOpPurge)

	_, err = te.db.Reader().Exec(`DROP TRIGGER fail_metadata_replay_state_insert`)
	require.NoError(t, err)
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

func TestMetadataEventsRestoreRepairsPublishedArtifactState(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))

	_, err := te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	restored, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.DeletedAt)
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	restoreOrderKey := metadataEventOrderKey(t, te, artifact.MetadataOpRestore)

	_, err = te.db.Reader().Exec(`DROP TRIGGER fail_metadata_replay_state_insert`)
	require.NoError(t, err)
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

func TestMetadataEventsRestoreRetriesWithoutPublishedArtifact(t *testing.T) {
	te := setup(t, withArtifactOrigin("desk-a1b2c3"))
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	_, err := te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	restored, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Nil(t, restored.DeletedAt)
	deleteMetadataEventsByOp(t, te, artifact.MetadataOpRestore)

	_, err = te.db.Reader().Exec(`DROP TRIGGER fail_metadata_replay_state_insert`)
	require.NoError(t, err)
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

	_, err := te.db.Reader().Exec(`
CREATE TRIGGER fail_metadata_replay_state_insert
BEFORE INSERT ON metadata_replay_state
BEGIN
	SELECT RAISE(FAIL, 'forced metadata replay failure');
END;
`)
	require.NoError(t, err)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, artifact.MetadataOpSoftDelete,
		serverMetadataReplayOp(t, te, "desk-a1b2c3~s1", "deleted_at"))
	for _, key := range metadataEventOrderKeys(t, te, artifact.MetadataOpRestore) {
		if key != olderRestore {
			deleteMetadataEventOrderKey(t, te, key)
		}
	}

	_, err = te.db.Reader().Exec(`DROP TRIGGER fail_metadata_replay_state_insert`)
	require.NoError(t, err)
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

func readMetadataEvents(t *testing.T, te *testEnv) []recordedMetadataEvent {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(te.dataDir, "artifacts", "*", "meta", "*.json"))
	require.NoError(t, err)
	sort.Strings(paths)
	events := make([]recordedMetadataEvent, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		events = append(events, event)
	}
	return events
}

func metadataEventOrderKey(t *testing.T, te *testEnv, op string) string {
	t.Helper()
	keys := metadataEventOrderKeys(t, te, op)
	require.NotEmpty(t, keys, "metadata event op %s not found", op)
	return keys[0]
}

func metadataEventOrderKeys(t *testing.T, te *testEnv, op string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(te.dataDir, "artifacts", "*", "meta", "*.json"))
	require.NoError(t, err)
	sort.Strings(paths)
	keys := make([]string, 0)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		if event.Op == op {
			keys = append(keys, strings.TrimSuffix(filepath.Base(path), ".json"))
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
	paths, err := filepath.Glob(filepath.Join(te.dataDir, "artifacts", "*", "meta", orderKey+".json"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	require.NoError(t, os.Remove(paths[0]))
}

func deleteMetadataEventsByOp(t *testing.T, te *testEnv, op string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(te.dataDir, "artifacts", "*", "meta", "*.json"))
	require.NoError(t, err)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var event recordedMetadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		if event.Op == op {
			require.NoError(t, os.Remove(path))
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
