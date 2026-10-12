package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
)

func TestCodexCloudStatusRoute(t *testing.T) {
	t.Run("unconfigured answers an empty list", func(t *testing.T) {
		s := testServer(t, 5*time.Second)
		w := serveJSON(t, s.Handler(), http.MethodGet, "/api/v1/codex-cloud/status", nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t, `[]`, w.Body.String())
	})
	t.Run("rows in board order", func(t *testing.T) {
		board := codexcloud.NewStatusBoard()
		board.Set(codexcloud.HomeStatus{Key: codexcloud.SourceRowKey, State: codexcloud.StateOffDisabled, Message: "Off: enabled is false."})
		s := testServer(t, 5*time.Second, WithCodexCloudStatus(board))
		w := serveJSON(t, s.Handler(), http.MethodGet, "/api/v1/codex-cloud/status", nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.JSONEq(t,
			`[{"key":"codex-cloud","homes":[],"state":"off_disabled","message":"Off: enabled is false."}]`,
			w.Body.String())
	})
}
