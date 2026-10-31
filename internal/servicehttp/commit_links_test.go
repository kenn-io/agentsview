package servicehttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestCommitLookupHTTPProxyPreservesKeysAndURLs(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/commits/sessions", r.URL.Path)
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		_, err := io.WriteString(w, `{"commits":{"abcdef0":[{"session_id":"codex:fixture","commit_sha":"abcdef0123456789012345678901234567890123","resolved":true,"source":"trailer","message_ordinal":null,"call_index":null,"repository_path":"","branch":"","subject":"","committed_at":"","ambiguous":false,"agent":"codex","project":"demo","session_started_at":"","web_url":"/sessions/codex/fixture","session":{"id":"codex:fixture"},"gone":false,"turn":null}],"0000000":[]}}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(s.Close)
	b := NewHTTPBackend(s.URL, "fixture-token", true, "https://example.test/ui")
	got, err := b.CommitSessions(t.Context(), []string{"abcdef0", "0000000"})
	require.NoError(t, err)
	require.Len(t, got["abcdef0"], 1)
	assert.Equal(t, "https://example.test/ui/sessions/codex/fixture", got["abcdef0"][0].WebURL)
	assert.NotNil(t, got["0000000"])
}

func TestCommitLookupHTTPProxyBoundsBeforeDecode(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", db.MaxCommitResponseBytes+1))
	}))
	t.Cleanup(s.Close)
	_, err := NewHTTPBackend(s.URL, "", true, "").CommitSessions(t.Context(), []string{"abcdef0"})
	assert.ErrorIs(t, err, service.ErrCommitLookupTooLarge)
}

func TestCommitLookupRejectsExpansionBeforeMalformedTail(t *testing.T) {
	// The malformed tail must not be reached after the occurrence cap is exceeded.
	input := []byte(`{"commits":{"abcdef0":[` + strings.Repeat(`{},`, db.MaxCommitLinks) + `{} BROKEN`)
	assert.ErrorIs(t, checkCommitJSONExpansion(input), service.ErrCommitLookupTooLarge)
}
