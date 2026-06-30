package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSessionStatsInvalidInputsReturnBadRequest(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name     string
		path     string
		wantBody string
	}{
		{
			name:     "timezone",
			path:     "/api/v1/session-stats?timezone=Fake/Zone",
			wantBody: "invalid timezone: Fake/Zone",
		},
		{
			name:     "since",
			path:     "/api/v1/session-stats?since=7x",
			wantBody: `parsing since "7x"`,
		},
		{
			name:     "until",
			path:     "/api/v1/session-stats?until=nope",
			wantBody: `parsing until "nope"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, tt.path)

			assertStatus(t, w, http.StatusBadRequest)
			resp := decode[map[string]string](t, w)
			assert.Contains(t, resp["error"], tt.wantBody)
		})
	}
}
