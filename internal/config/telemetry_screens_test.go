package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryScreenClaimsMigrationRecovery(t *testing.T) {
	const empty = `{"version":1,"days":{}}`
	for _, tc := range []struct{ name, stored, want string }{
		{"two screens", "install-one 2026-10-08 sessions usage", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2026-10-08"],"[\"install-one\",\"screen_viewed\",\"usage\"]":["2026-10-08"]}}`},
		{"short fields", "install-one", empty},
		{"invalid date", "install-one broken sessions", empty},
		{"future date", "install-one 2099-10-08 sessions", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2099-10-08"]}}`},
		{"newer version", `{"version":2,"claims":[]}`, empty},
		{"truncated JSON", `{"version":1,"days":{`, empty},
		{"null days", `{"version":1,"days":null}`, empty},
		{"invalid stored date", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"usage\"]":["10/08/2026"]}}`, empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
			path := c.TelemetryScreenClaimsPath()
			require.NoError(t, os.WriteFile(path, []byte(tc.stored), 0o600))
			require.NoError(t, c.MigrateTelemetryScreenClaims())
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(data))
		})
	}
}

func TestTelemetryScreenClaimsMigrationKeepsReadableClaims(t *testing.T) {
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	path := c.TelemetryScreenClaimsPath()
	const stored = `{ "version": 1, "days": {"[\"install-one\",\"screen_viewed\",\"usage\"]": ["2026-10-08"]} }`
	require.NoError(t, os.WriteFile(path, []byte(stored), 0o600))
	require.NoError(t, c.MigrateTelemetryScreenClaims())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, stored, string(data))
}
