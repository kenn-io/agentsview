package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

func TestVectorHubRoutesUnavailableLocally(t *testing.T) {
	s := testServer(t, 0)
	w := serveGet(t, s, "/api/v1/vectors/generation")
	assertRecorderStatus(t, w, http.StatusNotImplemented)
}

func TestVectorHubLookupIsArchiveRead(t *testing.T) {
	assert.False(t, archiveWriteRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup", nil)))
	assert.True(t, archiveWriteRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup-extra", nil)))
}

type hubTestStore struct {
	db.Store
	t    *testing.T
	err  error
	hook func(context.Context)
}

func (s hubTestStore) HubVectorGeneration(context.Context) (vector.HubGeneration, error) {
	return vector.HubGeneration{Fingerprint: "fp", Model: "m", Dimension: 1, DataVersion: 1, Params: map[string]string{"max_input_chars": "8192"}}, nil
}

func (s hubTestStore) LookupHubVectors(ctx context.Context, r vector.HubLookupRequest) (vector.HubLookupReply, error) {
	if s.hook != nil {
		s.hook(ctx)
	}
	if s.err != nil {
		return vector.HubLookupReply{}, s.err
	}
	require.Equal(s.t, "fp", r.Fingerprint)
	require.Equal(s.t, "page-token", r.Cursor)
	require.Equal(s.t, []vector.HubIdentity{{SessionID: "A", UnitKind: "user", ContentHash: "hash"}}, r.Identities)
	return vector.HubLookupReply{Version: "vectors.v1", Fingerprint: "fp", Chunks: []vector.HubChunk{{Index: 0, ChunkIndex: 0, ChunkCount: 1, Vector: "AACAPw=="}}, Absent: []int{}}, nil
}

func TestVectorHubLookupWire(t *testing.T) {
	s := testServer(t, 0)
	s.db = hubTestStore{Store: s.db, t: t}
	s.cfg.RequireAuth = true
	s.cfg.AuthToken = "test-token"
	for _, tc := range []struct{ encoding, want string }{
		{"identity", ""},
		{"zstd", "zstd"},
		{"ZsTd;q=0, gzip;q=1", ""},
		{"ZsTd;q=0.001, gzip;q=1", "zstd"},
		{"zstd;q=0.5", "zstd"},
		{"zstd;q=1", "zstd"},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup", strings.NewReader(`{"cursor":"page-token","fingerprint":"fp","identities":[{"session_id":"A","unit_kind":"user","content_hash":"hash"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", tc.encoding)
		unauthorized := httptest.NewRecorder()
		s.Handler().ServeHTTP(unauthorized, req.Clone(t.Context()))
		assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
		req.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		raw := w.Body.Bytes()
		assert.Equal(t, tc.want, w.Header().Get("Content-Encoding"))
		if tc.want == "zstd" {
			decoder, err := zstd.NewReader(w.Body)
			require.NoError(t, err)
			raw, err = io.ReadAll(decoder)
			decoder.Close()
			require.NoError(t, err)
		}
		var reply vector.HubLookupReply
		require.NoError(t, json.Unmarshal(raw, &reply))
		assert.Equal(t, vector.HubLookupReply{Version: "vectors.v1", Fingerprint: "fp", Chunks: []vector.HubChunk{{Index: 0, ChunkIndex: 0, ChunkCount: 1, Vector: "AACAPw=="}}, Absent: []int{}}, reply)
	}
	fixture := newSyncRouteFixture(t, withStaleDB())
	require.NoError(t, fixture.db.CloseWriter())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup", strings.NewReader(`{"cursor":"page-token","fingerprint":"fp","identities":[{"session_id":"A","unit_kind":"user","content_hash":"hash"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:0"
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	fixture.srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code, "read lookup remains available during rebuild")
}

func TestVectorHubLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"generation changed", fmt.Errorf("lookup: %w", vector.ErrHubGenerationChanged), 409, "generation_changed"},
		{"bad cursor", fmt.Errorf("cursor: %w", vector.ErrHubLookupInvalid), 400, ""},
		{"storage failure", errors.New("storage failure"), 500, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, 0)
			s.db = hubTestStore{Store: s.db, t: t, err: tc.err}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup", strings.NewReader(`{"cursor":"page-token","fingerprint":"fp","identities":[{"session_id":"A","unit_kind":"user","content_hash":"hash"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "zstd")
			req.Host = "127.0.0.1:0"
			req.Header.Set("Origin", "http://127.0.0.1:0")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Empty(t, w.Header().Get("Content-Encoding"))
			if tc.code != "" {
				assert.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
			}
		})
	}
}

func TestVectorHubLookupDeadline(t *testing.T) {
	s := testServer(t, 0)
	s.cfg.RequireAuth = true
	s.cfg.AuthToken = "test-token"
	var deadline time.Time
	var bounded bool
	s.db = hubTestStore{Store: s.db, t: t, hook: func(ctx context.Context) { deadline, bounded = ctx.Deadline() }}
	for _, seconds := range []int{1, 30, 300} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			s.cfg.WriteTimeout = time.Duration(seconds) * time.Second
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/vectors/lookup", strings.NewReader(`{"cursor":"page-token","fingerprint":"fp","identities":[{"session_id":"A","unit_kind":"user","content_hash":"hash"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-token")
			before := time.Now()
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			require.True(t, bounded, "lookup must give PostgreSQL a deadline")
			require.False(t, deadline.Before(before.Add(s.cfg.WriteTimeout)))
			require.False(t, deadline.After(time.Now().Add(s.cfg.WriteTimeout)))
		})
	}
}
