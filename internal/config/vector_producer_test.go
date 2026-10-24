package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVectorProducerConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, producer, url, host, endpoint, recipe, want string }{
		{"adopt", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "", ""},
		{"explicit recipe", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "model='hub'\ndimension=4", ""},
		{"explicit empty model", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "model=''", "model is required"},
		{"explicit zero dimension", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "dimension=0", "dimension must"},
		{"negative dimension", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "dimension=-1", "dimension must"},
		{"missing URL", "remote", "", "node", "http://127.0.0.1:8080/v1/embeddings", "", "[remote].url"},
		{"missing host", "remote", "https://hub.example", "", "http://127.0.0.1:8080/v1/embeddings", "", "[remote].spoke_host"},
		{"missing endpoint", "remote", "https://hub.example", "node", "", "", "endpoint"},
		{"invalid timeout", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "", "timeout"},
		{"unknown", "other", "", "", "", "", "producer must"},
		{"local recipe", "local", "", "", "http://127.0.0.1:8080/v1/embeddings", "", "model is required"},
		{"recall recipe", "remote", "https://hub.example", "node", "http://127.0.0.1:8080/v1/embeddings", "", "model is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			text := fmt.Sprintf("[remote]\nurl=%q\nspoke_host=%q\n[vector]\nenabled=true\nproducer=%q\n[vector.embeddings.servers.query]\nendpoint=%q\n", tc.url, tc.host, tc.producer, tc.endpoint)
			if tc.name == "invalid timeout" {
				text += "timeout='oops'\n"
			}
			if tc.recipe != "" {
				text += "[vector.embeddings]\n" + tc.recipe + "\n"
			}
			if tc.name == "recall recipe" {
				text += "[vector.embed]\nrecall=true\n"
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
