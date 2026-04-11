package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

// pinSessionMessage pins the first message of a session via the DB
// directly and returns the message ID used.
func pinSessionMessage(t *testing.T, te *testEnv, sessionID string) {
	t.Helper()
	msgs, err := te.db.GetMessages(context.Background(), sessionID, 0, 1, true)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("pinSessionMessage: no messages in session %s (err=%v)", sessionID, err)
	}
	id, err := te.db.PinMessage(sessionID, msgs[0].ID, nil)
	if err != nil || id == 0 {
		t.Fatalf("pinSessionMessage: PinMessage failed for session %s (id=%d, err=%v)", sessionID, id, err)
	}
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
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/pins: status %d, want 200", w.Code)
	}
	var resp struct {
		Pins []db.PinnedMessage `json:"pins"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Pins) != 2 {
		t.Errorf("got %d pins, want 2", len(resp.Pins))
	}
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
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/pins%s: status %d, want 200", tc.query, w.Code)
			}
			var resp struct {
				Pins []db.PinnedMessage `json:"pins"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if len(resp.Pins) != tc.wantCount {
				t.Errorf("query %q: got %d pins, want %d",
					tc.query, len(resp.Pins), tc.wantCount)
			}
		})
	}
}
