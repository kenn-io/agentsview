package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

// The token file selected by AGENTSVIEW_AUTH_TOKEN_FILE replaces the
// config.toml token for the HTTP API, not just in the loaded config.
func TestDeploymentTokenFileGatesAPI(t *testing.T) {
	for _, name := range []string{"AGENTSVIEW_AUTH_TOKEN", "AGENTSVIEW_HOST", "AGENTSVIEW_EMBEDDINGS_ENDPOINT", "AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", "AGENTSVIEW_EMBEDDINGS_BATCH_SIZE"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte("auth_token = \"old-config-token\"\n"), 0o600))
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("file-token\n"), 0o600))
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "true")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", tokenFile)

	loaded, err := config.LoadMinimal()
	require.NoError(t, err)
	require.NoError(t, loaded.EnsureAuthToken())

	te := setup(t, func(c *config.Config) {
		c.RequireAuth = loaded.RequireAuth
		c.AuthToken = loaded.AuthToken
	})
	w := te.rawRequest(http.MethodGet, "/api/ping", withBearer("old-config-token"))
	assertStatus(t, w, http.StatusUnauthorized)
	w = te.rawRequest(http.MethodGet, "/api/ping", withBearer("file-token"))
	assertStatus(t, w, http.StatusOK)
}
