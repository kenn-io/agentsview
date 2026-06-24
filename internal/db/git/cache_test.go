package git

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheSchema matches the `git_cache` DDL in internal/db/schema.sql. We keep
// it inline so these tests don't depend on loading the full server schema.
const cacheSchema = `
CREATE TABLE IF NOT EXISTS git_cache (
    cache_key   TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    computed_at TEXT NOT NULL
);
`

const (
	testCacheKey  = "k1"
	testCacheKind = "log"
	testCacheTTL  = time.Hour
)

type testCache interface {
	GetOrCompute(
		context.Context,
		string,
		string,
		time.Duration,
		func() ([]byte, error),
	) ([]byte, error)
}

type computeSpy struct {
	calls          int
	err            error
	payloadForCall func(int) string
}

type cacheRow struct {
	Key        string
	Kind       string
	Payload    string
	ComputedAt time.Time
}

type cacheKeyFields struct {
	kind   string
	repo   string
	author string
	since  string
	until  string
}

func (f cacheKeyFields) Key() string {
	return CacheKey(f.kind, f.repo, f.author, f.since, f.until)
}

// newCacheDB returns a file-backed SQLite DB seeded with the git_cache
// table. A file (rather than `:memory:`) keeps the pool stable across
// multiple connection acquisitions by the *sql.DB.
func newCacheDB(t *testing.T) *sql.DB {
	t.Helper()
	return openCacheDB(t, true)
}

func newCacheDBWithoutSchema(t *testing.T) *sql.DB {
	t.Helper()
	return openCacheDB(t, false)
}

func openCacheDB(t *testing.T, withSchema bool) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "sql.Open")
	t.Cleanup(func() { _ = db.Close() })
	if withSchema {
		_, err = db.Exec(cacheSchema)
		require.NoError(t, err, "init git_cache schema")
	}
	return db
}

func callTestCache(
	t *testing.T,
	cache testCache,
	compute func() ([]byte, error),
) ([]byte, error) {
	t.Helper()
	return cache.GetOrCompute(
		context.Background(),
		testCacheKey,
		testCacheKind,
		testCacheTTL,
		compute,
	)
}

func newComputeSpy(payload string) *computeSpy {
	return newDynamicComputeSpy(func(int) string { return payload })
}

func newDynamicComputeSpy(fn func(int) string) *computeSpy {
	return &computeSpy{payloadForCall: fn}
}

func newErrorComputeSpy(err error) *computeSpy {
	return &computeSpy{err: err}
}

func (s *computeSpy) Compute() ([]byte, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.payloadForCall(s.calls)), nil
}

func seedCacheRow(t *testing.T, db *sql.DB, row cacheRow) {
	t.Helper()
	if row.Key == "" {
		row.Key = testCacheKey
	}
	if row.Kind == "" {
		row.Kind = testCacheKind
	}
	if row.ComputedAt.IsZero() {
		row.ComputedAt = time.Now().UTC()
	}
	_, err := db.Exec(
		`INSERT OR REPLACE INTO git_cache(cache_key, kind, payload, computed_at)
		 VALUES (?, ?, ?, ?)`,
		row.Key, row.Kind, row.Payload,
		row.ComputedAt.UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err, "seed cache row")
}

func requireCacheRow(t *testing.T, db *sql.DB, key string) cacheRow {
	t.Helper()
	var row cacheRow
	var computedAt string
	err := db.QueryRow(
		`SELECT cache_key, kind, payload, computed_at
		 FROM git_cache WHERE cache_key = ?`,
		key,
	).Scan(&row.Key, &row.Kind, &row.Payload, &computedAt)
	require.NoError(t, err, "row not persisted")
	row.ComputedAt, err = time.Parse(time.RFC3339Nano, computedAt)
	require.NoError(t, err, "parse computed_at")
	return row
}

func requireCacheRowCount(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM git_cache WHERE cache_key = ?`, key,
	).Scan(&n)
	require.NoError(t, err, "count")
	return n
}

func backdateCacheRow(t *testing.T, db *sql.DB, key string, age time.Duration) {
	t.Helper()
	_, err := db.Exec(
		`UPDATE git_cache SET computed_at = ? WHERE cache_key = ?`,
		time.Now().Add(-age).UTC().Format(time.RFC3339Nano), key,
	)
	require.NoError(t, err, "backdating row")
}

func TestCache_GetOrCompute_FirstCallInvokesCompute(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	compute := newComputeSpy(`{"commits":3}`)
	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "GetOrCompute")
	assert.Equal(t, 1, compute.calls, "compute call count")
	assert.Equal(t, `{"commits":3}`, string(got), "payload")

	// Verify the row landed in git_cache with the expected kind.
	row := requireCacheRow(t, db, testCacheKey)
	assert.Equal(t, testCacheKind, row.Kind, "row kind")
	assert.Equal(t, `{"commits":3}`, row.Payload, "row payload")
}

func TestCache_GetOrCompute_WithinTTLReturnsCached(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	compute := newComputeSpy(`{"n":1}`)
	_, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "first GetOrCompute")
	require.Equal(t, 1, compute.calls, "after first call")

	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "second GetOrCompute")
	assert.Equal(t, 1, compute.calls, "compute called again within TTL")
	assert.Equal(t, `{"n":1}`, string(got), "cached payload")
}

func TestCache_GetOrCompute_PastTTLRecomputes(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	compute := newDynamicComputeSpy(func(call int) string {
		return `{"call":` + strconv.Itoa(call) + `}`
	})

	// Seed the cache with a timestamp well in the past so the second call
	// sees an expired row.
	_, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "first GetOrCompute")
	backdateCacheRow(t, db, testCacheKey, 2*time.Hour)

	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "second GetOrCompute")
	assert.Equal(t, 2, compute.calls, "compute invocations (past TTL)")
	assert.Equal(t, `{"call":2}`, string(got), "recomputed payload")
}

func TestCache_GetOrCompute_ErrorDoesNotWriteRow(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	boom := errors.New("compute blew up")
	compute := newErrorComputeSpy(boom)
	_, err := callTestCache(t, cache, compute.Compute)
	require.ErrorIs(t, err, boom)
	assert.Zero(t, requireCacheRowCount(t, db, testCacheKey),
		"row count after error")
}

func TestReadOnlyCache_GetOrCompute_MissComputesWithoutWriting(t *testing.T) {
	db := newCacheDB(t)
	cache := NewReadOnlyCache(db)

	compute := newComputeSpy(`{"commits":4}`)
	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "GetOrCompute")
	assert.Equal(t, 1, compute.calls, "compute call count")
	assert.Equal(t, `{"commits":4}`, string(got), "payload")

	assert.Zero(t, requireCacheRowCount(t, db, testCacheKey),
		"read-only cache must not write rows")
}

func TestReadOnlyCache_GetOrCompute_HitReturnsCached(t *testing.T) {
	db := newCacheDB(t)
	seedCacheRow(t, db, cacheRow{Payload: `{"commits":5}`})

	cache := NewReadOnlyCache(db)
	compute := newComputeSpy(`{"commits":6}`)
	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "GetOrCompute")
	assert.Zero(t, compute.calls, "compute call count")
	assert.Equal(t, `{"commits":5}`, string(got), "cached payload")
}

func TestReadOnlyCache_GetOrCompute_MissingTableComputes(t *testing.T) {
	db := newCacheDBWithoutSchema(t)
	cache := NewReadOnlyCache(db)

	compute := newComputeSpy(`{"commits":7}`)
	got, err := callTestCache(t, cache, compute.Compute)
	require.NoError(t, err, "GetOrCompute")
	assert.Equal(t, 1, compute.calls, "compute call count")
	assert.Equal(t, `{"commits":7}`, string(got), "payload")
}

func TestCacheKey_DeterministicAndSensitiveToEachField(t *testing.T) {
	baseFields := cacheKeyFields{
		kind:   "log",
		repo:   "/r",
		author: "a@x",
		since:  "2026-01-01",
		until:  "2026-02-01",
	}
	base := baseFields.Key()
	require.NotEmpty(t, base, "CacheKey returned empty string")
	assert.Equal(t, base, baseFields.Key(),
		"CacheKey non-deterministic")

	cases := []struct {
		name   string
		fields cacheKeyFields
	}{
		{"kind", cacheKeyFields{kind: "pr", repo: "/r", author: "a@x", since: "2026-01-01", until: "2026-02-01"}},
		{"repo", cacheKeyFields{kind: "log", repo: "/r2", author: "a@x", since: "2026-01-01", until: "2026-02-01"}},
		{"author", cacheKeyFields{kind: "log", repo: "/r", author: "b@x", since: "2026-01-01", until: "2026-02-01"}},
		{"since", cacheKeyFields{kind: "log", repo: "/r", author: "a@x", since: "2026-01-02", until: "2026-02-01"}},
		{"until", cacheKeyFields{kind: "log", repo: "/r", author: "a@x", since: "2026-01-01", until: "2026-02-02"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.fields.Key()
			assert.NotEqual(t, base, got,
				"CacheKey did not change when %s differed", c.name)
		})
	}
}
