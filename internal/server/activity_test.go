package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestGetSessionActivity(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	w := te.get(t, "/api/v1/sessions/s1/activity")
	require.Equal(t, http.StatusOK, w.Code)

	var body db.SessionActivityResponse
	require.NoError(t, json.UnmarshalRead(w.Body, &body))

	assert.NotZero(t, body.TotalMessages, "expected non-zero total_messages")
	assert.NotEmpty(t, body.Buckets, "expected non-empty buckets")
	assert.Positive(t, body.IntervalSeconds, "expected positive interval_seconds")
}

func TestGetSessionActivity_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/nonexistent/activity")
	require.Equal(t, http.StatusOK, w.Code)

	var body db.SessionActivityResponse
	require.NoError(t, json.UnmarshalRead(w.Body, &body))
	assert.Empty(t, body.Buckets, "expected empty buckets for nonexistent session")
}
