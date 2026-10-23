package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVectorProducerConfiguration(t *testing.T) {
	for _, tc := range []struct{ producer, url, host, endpoint, want string }{
		{"remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", ""},
		{"remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "model is required"},
		{"remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "dimension must"},
		{"remote", "", "node", "http://127.0.0.1:8080/v1/embeddings", "[remote].url"},
		{"remote", "https://hub.example", "", "http://127.0.0.1:8080/v1/embeddings", "[remote].spoke_host"},
		{"remote", "https://hub.example", "node", "", "endpoint"},
		{"other", "", "", "", "producer must"},
		{"local", "", "", "http://127.0.0.1:8080/v1/embeddings", "model is required"},
	} {
		t.Run(tc.producer+tc.want, func(t *testing.T) {
			dir := setupTestEnv(t)
			text := fmt.Sprintf("[remote]\nurl=%q\nspoke_host=%q\n[vector]\nenabled=true\nproducer=%q\n[vector.embeddings.servers.query]\nendpoint=%q\n", tc.url, tc.host, tc.producer, tc.endpoint)
			if tc.producer == "remote" && tc.want != "model is required" {
				text += "[vector.embeddings]\nmodel='hub'\n"
				if tc.want != "dimension must" {
					text += "dimension=4\n"
				}
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o600))
			_, err := LoadReadOnlyFor(ScopeVector)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestRemoteProducerDoesNotBlockLocalCommands(t *testing.T) {
	dir := setupTestEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nproducer='remote'\nenabled=true\n"), 0o600))
	_, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err)
}

func TestRecipeKeysExcludeDefaultsAndRetainExplicitZeroValues(t *testing.T) {
	dir := setupTestEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector.embeddings]\nmodel='hub'\nrequest_dimensions=false\nquery_prefix=''\n"), 0o600))
	cfg, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"model": true, "request_dimensions": true, "query_prefix": true}, cfg.Vector.Embeddings.RecipeKeys)
	require.Positive(t, cfg.Vector.Embeddings.MaxInputChars)
}
