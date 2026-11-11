package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionPushServeExtendsBoundedUploadReadDeadline(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "secret"
	s.httpReadTimeout = time.Second
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadlines := make(chan time.Time, 16)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.Serve(readDeadlineListener{Listener: listener, deadlines: deadlines})
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		require.NoError(t, s.Shutdown(ctx))
		require.ErrorIs(t, <-serveErr, http.ErrServerClosed)
	})

	source, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	require.NoError(t, source.UpsertSession(t.Context(), db.Session{
		ID: "selected", Agent: "claude", Machine: "source",
	}))
	payload, _, err := artifact.EncodeSessionPush(t.Context(), source, "selected", "")
	require.NoError(t, err)

	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost,
		"http://"+listener.Addr().String()+"/api/v1/sessions/ingest", reader,
	)
	require.NoError(t, err)
	request.ContentLength = int64(len(payload))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := make(chan int, 1)
	requestErr := make(chan error, 1)
	go func() {
		result, err := http.DefaultClient.Do(request)
		if err != nil {
			requestErr <- err
			return
		}
		defer result.Body.Close()
		response <- result.StatusCode
	}()
	defer writer.Close()
	defer reader.Close()
	_, err = writer.Write(payload[:1])
	require.NoError(t, err)

	deadlineWait := time.NewTimer(3 * time.Second)
	defer deadlineWait.Stop()
waitForUploadDeadline:
	for {
		select {
		case deadline := <-deadlines:
			if time.Until(deadline) > time.Minute {
				break waitForUploadDeadline
			}
		case <-deadlineWait.C:
			require.FailNow(t, "session push did not extend the real connection read deadline")
		}
	}
	// Wait past the server's original deadline to prove the extension works.
	timer := time.NewTimer(1100 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	_, err = writer.Write(payload[1:])
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	select {
	case err := <-requestErr:
		require.FailNowf(t, "test failed", "slow valid session push failed: %v", err)
	case status := <-response:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(3 * time.Second):
		require.FailNow(t, "slow valid session push did not finish")
	}
}

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
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", strings.NewReader("{}"))
	r.ContentLength = artifact.MaxSessionPushBytes
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
	stored, err := receiver.db.(*db.DB).GetSession(t.Context(), "source")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "claude", stored.Agent)
}

func TestSessionPushReceiverRejectsChangedBoundary(t *testing.T) {
	source := testServer(t, 30*time.Second).db.(*db.DB)
	session := db.Session{ID: "source", Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}
	message := db.Message{SessionID: session.ID, Ordinal: 0, Role: "user", Content: "hello", HasToolUse: true, ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "call", ResultContent: "result", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, ToolUseID: "call", Source: "tool", Status: "done", Content: "result"}}}}}
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
	before, err := receiver.db.(*db.DB).GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func(*db.Message)
	}{
		{"compact boundary", func(m *db.Message) { m.IsCompactBoundary = true }},
		{"source subtype", func(m *db.Message) { m.SourceSubtype = "notice" }},
		{"timestamp", func(m *db.Message) { m.Timestamp = "2026-01-02T00:00:00Z" }},
		{"model", func(m *db.Message) { m.Model = "model-b" }},
		{"tool identity", func(m *db.Message) { m.ToolCalls[0].ToolUseID = "other-call" }},
		{"result time", func(m *db.Message) { m.ToolCalls[0].ResultEvents[0].Timestamp = "2026-01-02T00:00:00Z" }},
		{"result attribution", func(m *db.Message) { m.ToolCalls[0].ResultEvents[0].AgentID = "agent-b" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedMessage := message
			changedMessage.ToolCalls = append([]db.ToolCall(nil), message.ToolCalls...)
			changedMessage.ToolCalls[0].ResultEvents = append([]db.ToolResultEvent(nil), message.ToolCalls[0].ResultEvents...)
			tc.change(&changedMessage)
			_, err := source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{changedMessage}, ReplaceMessages: true}})
			require.NoError(t, err)
			changed, _, err := artifact.EncodeSessionPush(t.Context(), source, session.ID, "")
			require.NoError(t, err)
			assert.Equal(t, http.StatusConflict, send(changed).Code)
			stored, err := receiver.db.(*db.DB).GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			assert.Equal(t, before, stored)
			assert.JSONEq(t, `{"session_id":"source","agent":"claude","status":"unchanged"}`, send(original).Body.String())
		})
	}
}

func TestSessionPushReportsFutureParserVersion(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
	d := s.db.(*db.DB)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "selected", Agent: "claude", Machine: "source"}))
	packet, _, err := artifact.EncodeSessionPush(t.Context(), d, "selected", "")
	require.NoError(t, err)
	var envelope artifact.SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &manifest))
	manifest["data_version"] = db.CurrentDataVersion() + 1
	envelope.Manifest, err = json.Marshal(manifest)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", bytes.NewReader(packet))
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "future-data-version", w.Header().Get("X-AgentsView-Push-Error"))
}

type sessionPushObservedBody struct{ reads int }

func (r *sessionPushObservedBody) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestSessionPushRejectsBeforeReadingUnauthorizedOrOversizeBody(t *testing.T) {
	for _, mode := range []string{"unauthorized", "disabled", "auth-off", "wrong-token", "query-token", "oversize", "encoding", "busy", "read-only", "usage-only"} {
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
				want = http.StatusForbidden
			case "wrong-token":
				r.Header.Set("Authorization", "Bearer bad")
			case "query-token":
				r.Header.Del("Authorization")
				r.URL.RawQuery = "token=fixture"
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

func TestSessionPushBasePathAndMalformedBodyAreAtomic(t *testing.T) {
	s := testServer(t, 30*time.Second, WithBasePath("/view"))
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/view/api/v1/sessions/ingest", strings.NewReader(`{"v":99,"session_id":"selected"}`))
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	selected, err := s.db.(*db.DB).GetSession(t.Context(), "selected")
	require.NoError(t, err)
	assert.Nil(t, selected)
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
