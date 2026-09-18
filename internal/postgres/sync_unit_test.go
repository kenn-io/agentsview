package postgres

import (
	"errors"
	"fmt"

	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

func TestIsUndefinedTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"unrelated error",
			errors.New("connection refused"),
			false,
		},
		{
			"generic does not exist",
			errors.New(
				`column "foo" does not exist`,
			),
			false,
		},
		{
			"SQLSTATE 42P01",
			fmt.Errorf("query sessions: %w", &pgconn.PgError{Code: "42P01", Message: `relation "sessions" does not exist`}),
			true,
		},
		{
			"bare SQLSTATE",
			errors.New("42P01"),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUndefinedTable(tt.err))
		})
	}
}

func TestScopedSyncStateStoreMigratesLegacyState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	local := testDB(t)

	require.NoError(local.SetSyncState(
		"last_push_at",
		"2026-03-11T12:34:56.123Z",
	))
	require.NoError(local.SetSyncState(
		lastPushBoundaryStateKey,
		`{"cutoff":"2026-03-11T12:34:56.123Z"}`,
	))
	require.NoError(local.SetSyncState(
		lastPushTargetFingerprintKey,
		"fingerprint-a",
	))
	require.NoError(local.SetSyncState(
		pushMarkerIDStateKey,
		"marker-a",
	))

	store := newScopedSyncStateStore(local, "work", true)

	lastPush, err := store.GetSyncState("last_push_at")
	require.NoError(err)
	assert.Equal("2026-03-11T12:34:56.123Z", lastPush)

	for _, key := range []string{
		"last_push_at",
		lastPushBoundaryStateKey,
		lastPushTargetFingerprintKey,
	} {
		legacyValue, err := local.GetSyncState(key)
		require.NoError(err)
		assert.Empty(legacyValue)

		scopedValue, err := local.GetSyncState(key + ":work")
		require.NoError(err)
		assert.NotEmpty(scopedValue)
	}

	legacyMarker, err := local.GetSyncState(pushMarkerIDStateKey)
	require.NoError(err)
	assert.Equal("marker-a", legacyMarker)

	scopedMarker, err := local.GetSyncState(
		pushMarkerIDStateKey + ":work",
	)
	require.NoError(err)
	assert.Empty(scopedMarker)
}

func TestScopedSyncStateStoreNonDefaultTargetDoesNotMigrateLegacyState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	local := testDB(t)

	require.NoError(local.SetSyncState(
		"last_push_at",
		"2026-03-11T12:34:56.123Z",
	))

	store := newScopedSyncStateStore(local, "archive", false)

	got, err := store.GetSyncState("last_push_at")
	require.NoError(err)
	assert.Empty(got)

	legacyValue, err := local.GetSyncState("last_push_at")
	require.NoError(err)
	assert.Equal("2026-03-11T12:34:56.123Z", legacyValue)
}

func TestScopedSyncStateStoreLegacyModeUsesUnscopedKeys(t *testing.T) {
	local := testDB(t)
	store := newScopedSyncStateStore(local, "", false)

	require.NoError(t, store.SetSyncState(
		"last_push_at",
		"2026-03-11T12:34:56.123Z",
	))

	got, err := local.GetSyncState("last_push_at")
	require.NoError(t, err)
	assert.Equal(t, "2026-03-11T12:34:56.123Z", got)
}

func TestPushSyncStateScopeIncludesProjectFilters(t *testing.T) {
	assert := assert.New(t)

	assert.Empty(pushSyncStateScope("", nil, nil))
	assert.Equal("work", pushSyncStateScope("work", nil, nil))

	includeAB := pushSyncStateScope(
		"work",
		[]string{"beta", "alpha"},
		nil,
	)
	includeBA := pushSyncStateScope(
		"work",
		[]string{"alpha", "beta"},
		nil,
	)
	excludeAB := pushSyncStateScope(
		"work",
		nil,
		[]string{"alpha", "beta"},
	)
	includeAExcludeB := pushSyncStateScope(
		"work",
		[]string{"alpha"},
		[]string{"beta"},
	)
	defaultIncludeAB := pushSyncStateScope(
		"",
		[]string{"alpha", "beta"},
		nil,
	)

	assert.Equal(includeAB, includeBA)
	assert.NotEmpty(includeAB)
	assert.NotEqual("work", includeAB)
	assert.NotEqual(includeAB, excludeAB)
	assert.NotEqual(pushSyncStateScope("work", []string{"alpha"}, nil),
		includeAExcludeB)
	assert.NotEqual(includeAB, defaultIncludeAB)
}

func TestNewRejectsIncludeAndExcludeProjects(t *testing.T) {
	local := testDB(t)

	_, err := New(
		"postgres://user:pass@127.0.0.1:1/db?sslmode=disable",
		"agentsview",
		local,
		"test-machine",
		true,
		SyncOptions{
			Projects:        []string{"alpha"},
			ExcludeProjects: []string{"beta"},
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"projects and exclude_projects are mutually exclusive")
}

func TestReadLastPushAtUsesProjectFilterScope(t *testing.T) {
	require := require.New(t)

	local := testDB(t)

	require.NoError(local.SetSyncState(
		"last_push_at:work",
		"2026-03-11T12:00:00.000Z",
	))
	filterScope := pushSyncStateScope(
		"work",
		[]string{"alpha", "beta"},
		nil,
	)
	require.NoError(local.SetSyncState(
		"last_push_at:"+filterScope,
		"2026-03-11T13:00:00.000Z",
	))

	got, err := ReadLastPushAt(
		local,
		"work",
		[]string{"beta", "alpha"},
		nil,
		true,
	)
	require.NoError(err)
	assert.Equal(t, "2026-03-11T13:00:00.000Z", got)
}

func TestReadLastPushAtDoesNotMigrateLegacyStateForProjectFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	local := testDB(t)

	require.NoError(local.SetSyncState(
		"last_push_at",
		"2026-03-11T12:00:00.000Z",
	))

	got, err := ReadLastPushAt(
		local,
		"",
		[]string{"alpha"},
		nil,
		true,
	)
	require.NoError(err)
	assert.Empty(got)

	legacyValue, err := local.GetSyncState("last_push_at")
	require.NoError(err)
	assert.Equal("2026-03-11T12:00:00.000Z", legacyValue)
}
