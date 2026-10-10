package config

import (
	"bytes"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTelemetryScreenClaimsMigrationRecovery(t *testing.T) {
	for _, tc := range []struct{ name, stored, want string }{
		{"two screens", "install-one 2026-10-08 sessions usage", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2026-10-08"],"[\"install-one\",\"screen_viewed\",\"usage\"]":["2026-10-08"]}}`},
		{"short fields", "install-one", ""},
		{"invalid date", "install-one broken sessions", ""},
		{"no screens", "install-one 2026-10-08", ""},
		{"empty file", "", ""},
		{"whitespace only", " \t\r\n", ""},
		{"future date", "install-one 2099-10-08 sessions", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2099-10-08"]}}`},
		{"newer version", `{"version":2,"claims":[]}`, ""},
		{"truncated JSON", `{"version":1,"days":{`, ""},
		{"damaged JSON without brace", `"version":1,"days":{}}`, ""},
		{"null days", `{"version":1,"days":null}`, ""},
		{"invalid stored date", `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"usage\"]":["10/08/2026"]}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			output := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(output) })
			c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
			path := c.TelemetryScreenClaimsPath()
			require.NoError(t, os.WriteFile(path, []byte(tc.stored), 0o600))
			if tc.want == "" {
				require.NoError(t, os.WriteFile(path+".unreadable", []byte("older claims"), 0o600))
			}
			require.NoError(t, c.MigrateTelemetryScreenClaims())
			if tc.want == "" {
				data, err := os.ReadFile(path + ".unreadable")
				require.NoError(t, err)
				assert.Equal(t, tc.stored, string(data))
				assert.Contains(t, logs.String(), "moved unreadable telemetry screen claims to "+path+".unreadable")
				_, err = os.Stat(path)
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(data))
			assert.Empty(t, logs.String())
		})
	}
}

func TestTelemetryScreenClaimsMigrationKeepsFileWhenMoveFails(t *testing.T) {
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	path := c.TelemetryScreenClaimsPath()
	const stored = `{"version":2,"claims":[]}`
	require.NoError(t, os.WriteFile(path, []byte(stored), 0o600))
	require.NoError(t, os.Mkdir(path+".unreadable", 0o700))
	require.Error(t, c.MigrateTelemetryScreenClaims())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, stored, string(data))
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
