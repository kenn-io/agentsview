package server

import (
	"bytes"
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

type sessionPushObservedBody struct{ reads int }

func (r *sessionPushObservedBody) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestSessionPushRejectsBeforeReadingUnauthorizedOrOversizeBody(t *testing.T) {
	for _, mode := range []string{"unauthorized", "query-token", "oversize", "encoding", "busy", "read-only", "usage-only"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t, 30*time.Second)
			s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
			body := &sessionPushObservedBody{}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/ingest", body)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer fixture")
			want := http.StatusUnauthorized
			switch mode {
			case "unauthorized":
				r.Header.Del("Authorization")
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
				want = http.StatusConflict
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
	for _, payload := range []string{`{"v":99,"session_id":"selected"}`, `{"v":1,"v":1}`, `{} {}`} {
		r := httptest.NewRequest(http.MethodPost, "/view/api/v1/sessions/ingest", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer fixture")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		selected, err := s.db.(*db.DB).GetSession(t.Context(), "selected")
		require.NoError(t, err)
		assert.Nil(t, selected)
	}
	// A valid first push with that same ID must still be newly accepted.
	source := testServer(t, 30*time.Second)
	d := source.db.(*db.DB)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "selected", Agent: "claude", Machine: "source"}))
	payload, err := artifact.EncodeSessionPush(t.Context(), d, "selected", "")
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/view/api/v1/sessions/ingest", bytes.NewReader(payload))
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"session_id":"selected","agent":"claude","status":"created"}`, w.Body.String())
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
