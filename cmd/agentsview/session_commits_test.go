package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionCommitsReadsExactRetainedIDOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/sessions/codex:gone/commits", r.URL.Path)
		_, _ = w.Write([]byte(`{"commits":[]}`))
	}))
	defer server.Close()
	cmd := newSessionCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"commits", "codex:gone", "--server", server.URL, "--format", "json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"commits":[]}`, output.String())
}
