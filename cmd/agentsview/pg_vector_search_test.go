package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

type pgReadCompatibilityTestStore struct {
	db.Store
	pg *sql.DB
}

func (s pgReadCompatibilityTestStore) DB() *sql.DB { return s.pg }

func TestResolvePGServeVectorState(t *testing.T) {
	tests := []struct {
		name         string
		enabled      bool
		found        bool
		wantFP       string
		foundFPs     string
		expectWire   bool
		expectReason string
	}{
		{
			name:         "vector disabled yields plain unavailable",
			enabled:      false,
			found:        false,
			wantFP:       "abc123",
			foundFPs:     "def456",
			expectWire:   false,
			expectReason: "",
		},
		{
			name:         "enabled and generation found wires searcher",
			enabled:      true,
			found:        true,
			wantFP:       "abc123",
			foundFPs:     "abc123",
			expectWire:   true,
			expectReason: "",
		},
		{
			name:       "enabled but no matching generation",
			enabled:    true,
			found:      false,
			wantFP:     "abc123",
			foundFPs:   "def456, ghi789",
			expectWire: false,
			expectReason: "semantic search: PG has no embedding generation matching " +
				"fingerprint abc123 (present: def456, ghi789); run 'agentsview pg push' " +
				"from a machine with a matching [vector.embeddings] config",
		},
		{
			name:       "enabled but PG has no generations at all",
			enabled:    true,
			found:      false,
			wantFP:     "abc123",
			foundFPs:   "",
			expectWire: false,
			expectReason: "semantic search: PG has no embedding generation matching " +
				"fingerprint abc123 (present: ); run 'agentsview pg push' " +
				"from a machine with a matching [vector.embeddings] config",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire, reason := resolvePGServeVectorState(
				tt.enabled, tt.found, tt.wantFP, tt.foundFPs)
			assert.Equal(t, tt.expectWire, wire)
			assert.Equal(t, tt.expectReason, reason)
		})
	}
}

// TestNewPGReadServiceRunsVectorWiring proves the CLI direct-read constructor
// (shared by `session search --pg` and `mcp --pg`) runs the PG vector gate on
// the store it opened, with the caller's config — the parity counterpart of
// the SQLite direct path's installDirectVectorSearcher call. Dropping the
// wiring call from newPGReadService, or passing it a different store or
// config, fails this test.
func TestNewPGReadServiceRunsVectorWiring(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	stubPGReadStore(t, fakeStore)

	var gotCfg config.Config
	var gotStore db.Store
	calls := 0
	orig := wirePGReadVectorSearchFn
	wirePGReadVectorSearchFn = func(cfg config.Config, store db.Store) {
		calls++
		gotCfg = cfg
		gotStore = store
	}
	t.Cleanup(func() { wirePGReadVectorSearchFn = orig })

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
	assert.True(t, gotCfg.Vector.Enabled,
		"wiring must see the caller's vector config")
}

// TestNewPGReadServiceUsesPersistentCompatibilityGate protects long-lived
// direct PostgreSQL readers such as `mcp --pg` and `session watch --pg` from
// accepting CockroachDB's one-time scan mode. The persistent gate must receive
// the database owned by the store that will serve subsequent reads.
func TestNewPGReadServiceUsesPersistentCompatibilityGate(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	pg := new(sql.DB)
	stubPGReadStore(t, pgReadCompatibilityTestStore{
		Store: fakeStore,
		pg:    pg,
	})

	calls := 0
	orig := checkPGPersistentReadCompatDBFn
	checkPGPersistentReadCompatDBFn = func(
		_ context.Context, got *sql.DB,
	) error {
		calls++
		assert.Same(t, pg, got)
		return nil
	}
	t.Cleanup(func() { checkPGPersistentReadCompatDBFn = orig })

	svc, cleanup, err := newPGReadService(config.Config{}, config.PGConfig{
		URL:    "postgres://example.test/agentsview",
		Schema: "agentsview",
	})
	require.NoError(t, err)
	require.NotNil(t, svc)
	t.Cleanup(cleanup)
	assert.Equal(t, 1, calls,
		"persistent compatibility must be checked exactly once at startup")
}

func TestNewPGReadServiceRejectsIncompatibleDataBeforeVectorWiring(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	stub := stubPGReadStore(t, fakeStore)

	wantErr := errors.New("encrypted payload repair required")
	origCheck := checkPGReadCompatFn
	checkPGReadCompatFn = func(_ context.Context, got db.Store) error {
		assert.Same(t, db.Store(fakeStore), got)
		return wantErr
	}
	t.Cleanup(func() { checkPGReadCompatFn = origCheck })

	wireCalls := 0
	origWire := wirePGReadVectorSearchFn
	wirePGReadVectorSearchFn = func(config.Config, db.Store) {
		wireCalls++
	}
	t.Cleanup(func() { wirePGReadVectorSearchFn = origWire })

	svc, cleanup, err := newPGReadService(config.Config{}, config.PGConfig{
		URL:    "postgres://example.test/agentsview",
		Schema: "agentsview",
	})

	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, svc)
	assert.Nil(t, cleanup)
	assert.True(t, stub.CleanupCalled,
		"an incompatible direct PG store must be closed")
	assert.Zero(t, wireCalls,
		"vector search must not be enabled before compatibility passes")
}

// TestWirePGReadVectorSearchIgnoresNonPGStore covers the CLI wiring guard for
// stores that are not *postgres.Store: tests stub openPGReadStore with a
// SQLite-backed fake, so the guard must leave such stores untouched instead
// of panicking on the type assertion.
func TestWirePGReadVectorSearchIgnoresNonPGStore(t *testing.T) {
	fakeStore := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "pg.db"))
	cfg := config.Config{}
	cfg.Vector.Enabled = true

	require.NotPanics(t, func() {
		wirePGReadVectorSearch(cfg, fakeStore)
	})
}
