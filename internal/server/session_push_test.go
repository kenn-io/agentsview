package server

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type sessionPushDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *sessionPushDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestSessionPushReadDeadlineScalesWithPayload(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "secret"
	body := strings.NewReader(strings.Repeat(" ", int(artifact.MaxSessionPushBytes)-1024))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", body)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	w := &sessionPushDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Greater(t, time.Until(w.deadline), 18*time.Minute)
}

func TestSessionPushReceiverInsertAndRetry(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "secret"
	d := s.db.(*db.DB)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "source", Agent: "claude", Machine: "source"}))
	payload, _, err := artifact.EncodeSessionPush(t.Context(), d, "source", "")
	require.NoError(t, err)
	// Move this selected synthetic session to a distinct receiving archive.
	receiver := testServer(t, 30*time.Second)
	receiver.cfg.AcceptSessionPush, receiver.cfg.RequireAuth, receiver.cfg.AuthToken = true, true, "secret"
	notified := 0
	receiver.sessionMutationNotify = func() { notified++ }
	receiver.broadcaster = NewBroadcaster(0)
	events, unsubscribe := receiver.broadcaster.Subscribe()
	defer unsubscribe()
	for _, status := range []string{"created", "unchanged"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/api/v1/sessions/ingest", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		receiver.Handler().ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.JSONEq(t, `{"session_id":"source","agent":"claude","status":"`+status+`"}`, w.Body.String())
	}
	assert.Equal(t, 1, notified)
	select {
	case event := <-events:
		assert.Equal(t, "sessions", event.Scope)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "accepted session did not broadcast")
	}
	assert.Empty(t, events)
	stored, err := receiver.db.(*db.DB).GetSession(t.Context(), "source")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "claude", stored.Agent)
}

func TestSessionPushCannotClaimReceiverOrigin(t *testing.T) {
	source := testServer(t, 30*time.Second).db.(*db.DB)
	require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: "selected", Agent: "claude", Machine: "source"}))
	packet, _, err := artifact.EncodeSessionPush(t.Context(), source, "selected", "")
	require.NoError(t, err)
	var envelope artifact.SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &manifest))
	manifest["origin"] = "receiver-box"
	manifest["session"].(map[string]any)["machine"] = "receiver-box"
	envelope.Manifest, err = json.Marshal(manifest)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	receiver := testServer(t, 30*time.Second)
	receiver.cfg.AcceptSessionPush, receiver.cfg.RequireAuth, receiver.cfg.AuthToken = true, true, "secret"
	database := receiver.db.(*db.DB)
	require.NoError(t, database.SetSyncState(t.Context(), "artifact_local_machine_name", "receiver-box"))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", bytes.NewReader(packet))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	owned, err := database.ListOwnedSessionIDsForExport(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, owned, "selected")
}

func TestSessionPushReceiverReportsErrors(t *testing.T) {
	source := testServer(t, 30*time.Second).db.(*db.DB)
	session := db.Session{ID: "source", Agent: "claude", Machine: "source", MessageCount: 1}
	message := db.Message{SessionID: session.ID, Role: "user", Content: "first"}
	_, err := source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, ReplaceMessages: true}})
	require.NoError(t, err)
	original, _, err := artifact.EncodeSessionPush(t.Context(), source, session.ID, "")
	require.NoError(t, err)
	receiver := testServer(t, 30*time.Second)
	receiver.cfg.AcceptSessionPush, receiver.cfg.RequireAuth, receiver.cfg.AuthToken = true, true, "secret"
	send := func(payload []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		receiver.Handler().ServeHTTP(w, r)
		return w
	}
	require.Equal(t, http.StatusOK, send(original).Code)
	message.Content = "changed"
	_, err = source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, ReplaceMessages: true}})
	require.NoError(t, err)
	changed, _, err := artifact.EncodeSessionPush(t.Context(), source, session.ID, "")
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, send(changed).Code)

	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- receiver.SerializeArchiveWrite(t.Context(), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	assert.Equal(t, http.StatusServiceUnavailable, send(original).Code)
	close(release)
	require.NoError(t, <-done)

	var envelope artifact.SessionPushEnvelope
	require.NoError(t, json.Unmarshal(original, &envelope))
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &manifest))
	manifest["data_version"] = db.CurrentDataVersion() + 1
	envelope.Manifest, err = json.Marshal(manifest)
	require.NoError(t, err)
	future, err := json.Marshal(envelope)
	require.NoError(t, err)
	w := send(future)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "future-data-version", w.Header().Get("X-AgentsView-Push-Error"))
	require.NoError(t, receiver.db.(*db.DB).Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `PRAGMA user_version = 175`)
		return err
	}))
	w = send(original)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "archive-rebuild-required", w.Header().Get("X-AgentsView-Push-Error"))
}

type sessionPushObservedBody struct{ reads int }

func (r *sessionPushObservedBody) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestSessionPushRejectsBeforeReadingUnauthorizedOrOversizeBody(t *testing.T) {
	for _, mode := range []string{"unauthorized", "disabled", "auth-off", "auth-off-token", "oversize", "encoding", "busy", "read-only", "usage-only"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t, 30*time.Second)
			s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
			body := &sessionPushObservedBody{}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/api/v1/sessions/ingest", body)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer fixture")
			want := http.StatusUnauthorized
			switch mode {
			case "unauthorized":
				r.Header.Del("Authorization")
			case "disabled":
				s.cfg.AcceptSessionPush = false
				want = http.StatusNotFound
			case "auth-off":
				s.cfg.RequireAuth = false
				r.Header.Del("Authorization")
			case "auth-off-token":
				s.cfg.RequireAuth = false
				want = http.StatusForbidden
			case "oversize":
				r.ContentLength = artifact.MaxSessionPushBytes + 1
				want = http.StatusRequestEntityTooLarge
			case "encoding":
				r.Header.Set("Content-Encoding", "gzip")
				want = http.StatusUnsupportedMediaType
			case "busy":
				s.sessionPushMu.Lock()
				defer s.sessionPushMu.Unlock()
				want = http.StatusServiceUnavailable
			case "read-only":
				readOnly, err := db.OpenReadOnly(t.Context(), s.db.(*db.DB).Path())
				require.NoError(t, err)
				defer readOnly.Close()
				s.db = readOnly
				want = http.StatusNotImplemented
			case "usage-only":
				s.db.(*db.DB).SetArchiveContent(config.ArchiveContentUsage)
				want = http.StatusForbidden
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			require.Equal(t, want, w.Code, w.Body.String())
			assert.Zero(t, body.reads)
		})
	}
}

func TestSessionPushOpenAPIRepresentsObjectAndSegmentArray(t *testing.T) {
	s := testServer(t, 30*time.Second)
	path := s.api.OpenAPI().Paths["/api/v1/sessions/ingest"]
	require.NotNil(t, path)
	op := path.Post
	require.NotNil(t, op)
	schema := op.RequestBody.Content["application/json"].Schema
	if schema.Ref != "" {
		schema = s.api.OpenAPI().Components.Schemas.SchemaFromRef(schema.Ref)
	}
	require.NotNil(t, schema)
	require.NotNil(t, schema.Properties["manifest"])
	require.NotNil(t, schema.Properties["segments"])
	assert.Equal(t, "object", schema.Properties["manifest"].Type)
	assert.Equal(t, "array", schema.Properties["segments"].Type)
	require.NotNil(t, schema.Properties["segments"].Items)
	assert.Equal(t, "object", schema.Properties["segments"].Items.Type)
}
