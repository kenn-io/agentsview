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

func metadataOps(events []recordedMetadataEvent) []string {
	ops := make([]string, len(events))
	for i, event := range events {
		ops[i] = event.Op
	}
	return ops
}
