package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
)

// Preserve deployment recipe adoption at the PG service boundary, while a
// missing listener token file and invalid listener host remain irrelevant.
func TestScopedConfigPGVectorConsumersAdoptDeploymentRecipe(t *testing.T) {
	for _, args := range [][]string{
		{"session", "search", "task", "--pg", "--semantic"},
		{"session", "search", "task", "--pg", "--hybrid"},
		{"mcp", "--pg"},
		{"doctor", "memory", "--pg"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			seedScopedConfigArchive(t)
			t.Setenv("AGENTSVIEW_MODE", "pg-serve")
			t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/archive")
			t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", filepath.Join(os.Getenv("HOME"), "missing-token"))
			t.Setenv("AGENTSVIEW_HOST", "")
			t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", "http://127.0.0.1:30000/v1")
			store := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
			stubPGReadStore(t, store)
			orig := wireReplicaReadVectorSearchFn
			var adopted config.Config
			wireReplicaReadVectorSearchFn = func(cfg config.Config, backend storage.Replica, got db.Store) {
				assert.Same(t, db.Store(store), got)
				assert.Equal(t, "pg", backend.Name())
				require.NotNil(t, cfg.DeploymentEmbeddings, "PG vector reads need the deployment embeddings transport")
				var err error
				adopted, err = adoptReplicaVectorConfig(t.Context(), cfg,
					recipeProvider{gens: []storage.VectorGenerationInfo{publishedRecipe()}}, nil)
				require.NoError(t, err)
			}
			t.Cleanup(func() { wireReplicaReadVectorSearchFn = orig })
			cmd := parseCommand(t, args)
			resolve := resolveService
			if args[0] == "mcp" {
				resolve = resolveMCPService
			}
			var cleanup func()
			var err error
			if args[0] == "doctor" {
				_, cleanup, err = resolveDoctorMemoryService(cmd, doctorMemorySelection(cmd))
			} else {
				_, cleanup, err = resolve(cmd)
			}
			require.NoError(t, err)
			t.Cleanup(cleanup)
			assert.True(t, adopted.Vector.Enabled)
			assert.Equal(t, publishedRecipe().Fingerprint, vectorGeneration(adopted.Vector.Embeddings).Fingerprint())
			assert.Equal(t, "http://127.0.0.1:30000/v1", adopted.Vector.Embeddings.Servers["deployment"].Endpoint)
		})
	}
}

func TestScopedConfigPGDeploymentEmbeddingsValidation(t *testing.T) {
	for _, args := range [][]string{
		{"session", "list", "--pg"},
		{"session", "search", "task", "--pg", "--semantic"},
		{"mcp", "--pg"},
		{"doctor", "memory", "--pg"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			seedScopedConfigArchive(t)
			t.Setenv("AGENTSVIEW_MODE", "pg-serve")
			t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/archive")
			t.Setenv("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE", "invalid")
			stubPGReadStore(t, dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db")))
			resolve := resolveService
			if args[0] == "mcp" {
				resolve = resolveMCPService
			}
			cmd := parseCommand(t, args)
			var cleanup func()
			var err error
			if args[0] == "doctor" {
				_, cleanup, err = resolveDoctorMemoryService(cmd, doctorMemorySelection(cmd))
			} else {
				_, cleanup, err = resolve(cmd)
			}
			if args[1] == "list" {
				require.NoError(t, err, "plain PG reads do not use deployment embeddings")
				t.Cleanup(cleanup)
			} else {
				require.ErrorContains(t, err, "AGENTSVIEW_EMBEDDINGS_ENDPOINT is required")
			}
		})
	}
}

func TestReplicaVectorReasons(t *testing.T) {
	backend := pgReplica{}
	assert.Equal(t,
		"semantic search: PostgreSQL has no embedding generation matching fingerprint "+
			"abc123 (present: def456, ghi789); run 'agentsview pg push' from a machine "+
			"with a matching [vector.embeddings] config",
		replicaVectorUnavailableReason(backend,
			"PostgreSQL has no embedding generation matching fingerprint abc123",
			[]storage.VectorGenerationInfo{{Fingerprint: "def456"}, {Fingerprint: "ghi789"}}))
	assert.Equal(t,
		"semantic search: no match (present: ); run 'agentsview pg push' from a "+
			"machine with a matching [vector.embeddings] config",
		replicaVectorUnavailableReason(backend, "no match", nil))
}

func TestWireReplicaVectorSearchRecordsVectorDisabledReason(t *testing.T) {
	store := &postgres.Store{}
	require.NoError(t, wireReplicaVectorSearch(
		t.Context(), config.Config{}, pgReplica{}, store, "pg serve"))

	_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "hello", Mode: "semantic",
	})
	require.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "PostgreSQL requires [vector] enabled")
	assert.Contains(t, err.Error(), "agentsview pg push")
	assert.NotContains(t, err.Error(), "agentsview embeddings build")
}

// nonVectorReplica is a replica that does not implement
// storage.VectorSearchProvider; only DisplayName is reachable from the gate.
type nonVectorReplica struct{ storage.Replica }

func (nonVectorReplica) DisplayName() string { return "Other" }

// TestWireReplicaVectorSearchRecordsUnsupportedBackend pins that a replica
// without a vector search provider leaves its store explaining the modes are
// unsupported, even when [vector] is enabled locally.
func TestWireReplicaVectorSearchRecordsUnsupportedBackend(t *testing.T) {
	store := &postgres.Store{}
	cfg := config.Config{}
	cfg.Vector.Enabled = true
	require.NoError(t, wireReplicaVectorSearch(
		t.Context(), cfg, nonVectorReplica{}, store, "other serve"))

	_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "hello", Mode: "hybrid",
	})
	require.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "not supported by the Other backend")
}

// TestNewPGReadServiceRunsVectorWiring proves the CLI direct-read constructor
// (shared by `session search --pg` and `mcp --pg`) runs the replica vector
// gate on the store it opened, with the caller's config and the PostgreSQL
// backend. Dropping the wiring call from newPGReadService, or passing it a
// different store, backend, or config, fails this test.
func TestNewPGReadServiceRunsVectorWiring(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	stubPGReadStore(t, fakeStore)

	var gotCfg config.Config
	var gotStore db.Store
	var gotBackend storage.Replica
	calls := 0
	orig := wireReplicaReadVectorSearchFn
	wireReplicaReadVectorSearchFn = func(cfg config.Config, backend storage.Replica, store db.Store) {
		calls++
		gotCfg = cfg
		gotBackend = backend
		gotStore = store
	}
	t.Cleanup(func() { wireReplicaReadVectorSearchFn = orig })

	cfg := config.Config{}
	cfg.Vector.Enabled = true
	svc, cleanup, err := newPGReadService(cfg, config.PGConfig{
		URL:    "postgres://example.test/agentsview",
		Schema: "agentsview",
	})
	require.NoError(t, err)
	require.NotNil(t, svc)
	t.Cleanup(cleanup)

	require.Equal(t, 1, calls, "vector wiring must run exactly once per service")
	assert.Same(t, db.Store(fakeStore), gotStore,
		"wiring must target the store the service serves reads from")
	assert.Equal(t, "pg", gotBackend.Name())
	assert.True(t, gotCfg.Vector.Enabled,
		"wiring must see the caller's vector config")
}

// TestWireReplicaReadVectorSearchIgnoresNonReplicaStore covers the CLI wiring
// guard for stores that are not replica stores: tests stub the store opener
// with a SQLite-backed fake, so the guard must leave such stores untouched.
func TestWireReplicaReadVectorSearchIgnoresNonReplicaStore(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	cfg := config.Config{}
	cfg.Vector.Enabled = true

	require.NotPanics(t, func() {
		wireReplicaReadVectorSearch(cfg, pgReplica{}, fakeStore)
	})
}
