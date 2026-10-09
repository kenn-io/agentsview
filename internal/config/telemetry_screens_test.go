package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryScreenClaimsMigrationRecovery(t *testing.T) {
	for _, tc := range []struct{ name, legacy, want string }{
		{"two screens", "install-one 2026-10-08 sessions usage", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2026-10-08"],"[\"install-one\",\"screen_viewed\",\"usage\"]":["2026-10-08"]}}`},
		{"short fields", "install-one", `{"version":1,"days":{}}`},
		{"invalid date", "install-one broken sessions", `{"version":1,"days":{}}`},
		{"future date", "install-one 2099-10-08 sessions", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2099-10-08"]}}`},
		{"unknown JSON", `{"version":2,"claims":[]}`, `{"version":2,"claims":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
			path := c.TelemetryScreenClaimsPath()
			require.NoError(t, os.WriteFile(path, []byte(tc.legacy), 0o600))
			require.NoError(t, c.MigrateTelemetryScreenClaims())
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(data))
		})
	}
}
