package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMinimalHostedEmbeddingsLegacyPGAndProfileDefaults(t *testing.T) {
	f := newConfigFixture(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://environment-default")
	f.WriteConfigText(t, `
require_auth = true

[pg]
url = "postgres://runtime"
schema = "hosted"
raw_tenant = "tenant"
raw_derivation = true
raw_poll_seconds = 7
raw_attempt_seconds = 91
raw_max_attempts = 4
hosted_embeddings_enabled = true
hosted_embeddings_poll_seconds = 9
hosted_embeddings_attempt_seconds = 123
hosted_embeddings_max_attempts = 6
hosted_embeddings_concurrency = 2

[hosted_embeddings.profiles.current]
include_automated = true

[hosted_embeddings.profiles.current.embeddings]
model = "model-a"
dimension = 3
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = "https://embeddings.example.test/v1"

[hosted_embeddings.profiles.next.embeddings]
model = "model-b"
dimension = 1024
default_server = "primary"

[hosted_embeddings.profiles.next.embeddings.servers.primary]
endpoint = "https://embeddings.example.test/v1"
`)

	cfg := f.LoadMinimal(t)
	assert.Equal(t, "tenant", cfg.PG.RawTenant)
	assert.True(t, cfg.PG.RawDerivation)
	assert.Equal(t, 7, cfg.PG.RawPollSeconds)
	assert.Equal(t, 91, cfg.PG.RawAttemptSeconds)
	assert.Equal(t, 4, cfg.PG.RawMaxAttempts)
	assert.True(t, cfg.PG.HostedEmbeddingsEnabled)
	assert.Equal(t, 9, cfg.PG.HostedEmbeddingsPollSeconds)
	assert.Equal(t, 123, cfg.PG.HostedEmbeddingsAttemptSeconds)
	assert.Equal(t, 6, cfg.PG.HostedEmbeddingsMaxAttempts)
	assert.Equal(t, 2, cfg.PG.HostedEmbeddingsConcurrency)
	resolved, err := cfg.ResolvePG()
	require.NoError(t, err)
	assert.Equal(t, "postgres://environment-default", resolved.URL)

	profile, err := cfg.HostedEmbeddings.Profile("current")
	require.NoError(t, err)
	assert.True(t, profile.IncludeAutomated)
	assert.Equal(t, 8192, profile.Embeddings.MaxInputChars)
	server := profile.Embeddings.Servers["primary"]
	assert.Equal(t, 32, server.BatchSize)
	assert.Equal(t, 4, server.Concurrency)
	assert.Equal(t, "30s", server.Timeout)
	assert.Equal(t, 3, server.MaxRetries)
	next, err := cfg.HostedEmbeddings.Profile("next")
	require.NoError(t, err)
	assert.Equal(t, "model-b", next.Embeddings.Model)
	assert.Equal(t, 1024, next.Embeddings.Dimension)
}

func TestLoadMinimalHostedEmbeddingsNamedPGPreservesExplicitValues(t *testing.T) {
	f := newConfigFixture(t)
	f.WriteConfigText(t, `
default_pg = "runtime"

[pg.runtime]
url = "postgres://runtime"
schema = "hosted"
raw_tenant = "tenant"
raw_derivation = false
raw_poll_seconds = 8
raw_attempt_seconds = 92
raw_max_attempts = 3
hosted_embeddings_enabled = false
hosted_embeddings_poll_seconds = 10
hosted_embeddings_attempt_seconds = 124
hosted_embeddings_max_attempts = 7
hosted_embeddings_concurrency = 3

[hosted_embeddings.profiles.current.embeddings]
model = "model-a"
dimension = 3
max_input_chars = 100
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = "http://127.0.0.1:8081/v1"
batch_size = 1
concurrency = 1
timeout = "2s"
max_retries = 0
`)

	cfg := f.LoadMinimal(t)
	pg, err := cfg.ResolvePG()
	require.NoError(t, err)
	assert.False(t, pg.RawDerivation)
	assert.Equal(t, 8, pg.RawPollSeconds)
	assert.Equal(t, 92, pg.RawAttemptSeconds)
	assert.Equal(t, 3, pg.RawMaxAttempts)
	assert.False(t, pg.HostedEmbeddingsEnabled)
	assert.Equal(t, 10, pg.HostedEmbeddingsPollSeconds)
	assert.Equal(t, 124, pg.HostedEmbeddingsAttemptSeconds)
	assert.Equal(t, 7, pg.HostedEmbeddingsMaxAttempts)
	assert.Equal(t, 3, pg.HostedEmbeddingsConcurrency)

	profile, err := cfg.HostedEmbeddings.Profile("current")
	require.NoError(t, err)
	server := profile.Embeddings.Servers["primary"]
	assert.Equal(t, 1, server.BatchSize)
	assert.Equal(t, 1, server.Concurrency)
	assert.Equal(t, "2s", server.Timeout)
	assert.Zero(t, server.MaxRetries)
}

func TestHostedEmbeddingConfigValidation(t *testing.T) {
	validProfile := HostedEmbeddingProfile{Embeddings: VectorEmbeddingsConfig{
		Model: "model-a", Dimension: 3, MaxInputChars: 100,
		DefaultServer: "primary",
		Servers: map[string]VectorEmbeddingsServerConfig{
			"primary": {Endpoint: "http://127.0.0.1:8081/v1", BatchSize: 1, Concurrency: 1, Timeout: "2s"},
		},
	}}

	for _, tc := range []struct {
		name    string
		pg      PGConfig
		auth    bool
		wantErr string
	}{
		{name: "disabled", pg: PGConfig{}},
		{name: "enabled", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true}, auth: true},
		{name: "tenant", pg: PGConfig{Schema: "hosted", HostedEmbeddingsEnabled: true}, auth: true, wantErr: "raw_tenant"},
		{name: "auth", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true}, wantErr: "authentication"},
		{name: "poll", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true, HostedEmbeddingsPollSeconds: 61}, auth: true, wantErr: "bounds"},
		{name: "attempt", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true, HostedEmbeddingsAttemptSeconds: 301}, auth: true, wantErr: "bounds"},
		{name: "attempts", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true, HostedEmbeddingsMaxAttempts: 11}, auth: true, wantErr: "bounds"},
		{name: "concurrency", pg: PGConfig{RawTenant: "tenant", Schema: "hosted", HostedEmbeddingsEnabled: true, HostedEmbeddingsConcurrency: 5}, auth: true, wantErr: "bounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pg.ValidateHostedEmbeddings(tc.auth)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	cfg := Config{HostedEmbeddings: HostedEmbeddingsConfig{Profiles: map[string]HostedEmbeddingProfile{"current": validProfile}}}
	_, err := cfg.HostedEmbeddings.Profile("missing")
	require.ErrorContains(t, err, `no profile named "missing"`)

	bad := validProfile
	bad.Embeddings.Servers = map[string]VectorEmbeddingsServerConfig{
		"primary": {Endpoint: "/relative", BatchSize: 1, Concurrency: 1, Timeout: "2s"},
	}
	cfg.HostedEmbeddings.Profiles["bad"] = bad
	_, err = cfg.HostedEmbeddings.Profile("bad")
	require.ErrorContains(t, err, "absolute HTTP(S) endpoint")
}

func TestHostedEmbeddingWorkerBoundsDefaults(t *testing.T) {
	poll, attempt, attempts, concurrency := (PGConfig{}).HostedEmbeddingWorkerBounds()
	assert.Equal(t, 5, poll)
	assert.Equal(t, 120, attempt)
	assert.Equal(t, 5, attempts)
	assert.Equal(t, 1, concurrency)
}
