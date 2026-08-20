package server_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// pinSessionMessage pins the first message of a session via the DB
// directly and returns the message ID used.
func pinSessionMessage(t *testing.T, te *testEnv, sessionID string) {
	t.Helper()
	msgs, err := te.db.GetMessages(context.Background(), sessionID, 0, 1, true)
	require.NoError(t, err, "pinSessionMessage: GetMessages for session %s", sessionID)
	require.NotEmpty(t, msgs, "pinSessionMessage: no messages in session %s", sessionID)
	id, err := te.db.PinMessage(sessionID, msgs[0].ID, nil)
	require.NoError(t, err, "pinSessionMessage: PinMessage for session %s", sessionID)
	require.NotZero(t, id, "pinSessionMessage: PinMessage returned 0 id for session %s", sessionID)
}

func TestHandleListPins_NoFilter(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "beta", 2)
	te.seedMessages(t, "s1", 2)
	te.seedMessages(t, "s2", 2)
	pinSessionMessage(t, te, "s1")
	pinSessionMessage(t, te, "s2")

	w := te.get(t, "/api/v1/pins")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Pins []db.PinnedMessage `json:"pins"`
	}
	require.NoError(t, json.UnmarshalRead(w.Body, &resp))
	assert.Len(t, resp.Pins, 2)
}

func TestHandleListPins_ProjectFilter(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "a1", "alpha", 2)
	te.seedSession(t, "a2", "alpha", 2)
	te.seedSession(t, "b1", "beta", 2)
	te.seedMessages(t, "a1", 2)
	te.seedMessages(t, "a2", 2)
	te.seedMessages(t, "b1", 2)
	pinSessionMessage(t, te, "a1")
	pinSessionMessage(t, te, "a2")
	pinSessionMessage(t, te, "b1")

	tests := []struct {
		query     string
		wantCount int
	}{
		{"?project=alpha", 2},
		{"?project=beta", 1},
		{"?project=unknown", 0},
		{"", 3},
	}
	for _, tc := range tests {
		t.Run("query="+tc.query, func(t *testing.T) {
			w := te.get(t, "/api/v1/pins"+tc.query)
			require.Equal(t, http.StatusOK, w.Code, "GET /api/v1/pins%s", tc.query)
			var resp struct {
				Pins []db.PinnedMessage `json:"pins"`
			}
			require.NoError(t, json.UnmarshalRead(w.Body, &resp))
			assert.Len(t, resp.Pins, tc.wantCount, "query %q", tc.query)
		})
	}
}
