package config

import (
	"bytes"
	"encoding/json/v2"
	"log"
	"os"
	"path/filepath"
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
		{"invalid UTF-8 screen", "install-one 2026-10-08 sessions \xff", ""},
		{"another installation", "install-two 2026-10-08 sessions", ""},
		{"existing backup", "malformed", ""},
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
			olderPath := path + ".unreadable-existing"
			if tc.name == "existing backup" {
				require.NoError(t, os.WriteFile(olderPath, []byte("older claims"), 0o600))
			}
			require.NoError(t, c.MigrateTelemetryScreenClaims())
			if tc.name == "invalid UTF-8 screen" {
				assert.Contains(t, logs.String(), "skipping legacy telemetry screen claims: encoding claim key:")
			}
			moved, err := filepath.Glob(path + ".unreadable-*")
			require.NoError(t, err)
			copies := 0
			for _, movedPath := range moved {
				data, err := os.ReadFile(movedPath)
				require.NoError(t, err)
				if movedPath == olderPath {
					assert.Equal(t, "older claims", string(data))
					continue
				}
				copies++
				assert.Equal(t, tc.stored, string(data))
				assert.Contains(t, filepath.ToSlash(logs.String()), "moved unreadable telemetry screen claims to "+filepath.ToSlash(movedPath))
			}
			assert.Equal(t, 1, copies)
			if tc.name == "existing backup" {
				assert.Contains(t, moved, olderPath)
			}
			data, err := os.ReadFile(path)
			if tc.want == "" {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(data))
			var claims screenClaims
			require.NoError(t, json.Unmarshal(data, &claims))
			assert.True(t, claims.readable())
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
