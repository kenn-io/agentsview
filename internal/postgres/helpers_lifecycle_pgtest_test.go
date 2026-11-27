//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopedTB emulates a subtest's testing.TB with an independent name, context,
// and LIFO cleanup queue so lifecycle ordering between sibling and nested
// subtests can be exercised deterministically regardless of -parallel.
type scopedTB struct {
	testing.TB
	name     string
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	cleanups []func()
	cleaned  bool
}

func newScopedTB(t *testing.T, name string) *scopedTB {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	s := &scopedTB{
		TB:     t,
		name:   name,
		ctx:    ctx,
		cancel: cancel,
	}
	t.Cleanup(s.finish)
	return s
}

func (s *scopedTB) Name() string {
	return s.name
}

func (s *scopedTB) Context() context.Context {
	return s.ctx
}

func (s *scopedTB) Cleanup(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		panic("Cleanup called after scopedTB finished")
	}
	s.cleanups = append(s.cleanups, fn)
}

func (s *scopedTB) finish() {
	s.mu.Lock()
	if s.cleaned {
		s.mu.Unlock()
		return
	}
	s.cleaned = true
	fns := slices.Clone(s.cleanups)
	s.cleanups = nil
	s.mu.Unlock()

	s.cancel()
	for _, fn := range slices.Backward(fns) {
		fn()
	}
}

func openTestPGConn(tb testing.TB, dsn string) *sql.Conn {
	tb.Helper()
	pool, err := sql.Open("pgx", dsn)
	require.NoError(tb, err, "opening test database")
	tb.Cleanup(func() { assert.NoError(tb, pool.Close()) })
	conn, err := pool.Conn(tb.Context())
	require.NoError(tb, err, "pinning test database connection")
	tb.Cleanup(func() { assert.NoError(tb, conn.Close()) })
	var one int
	require.NoError(tb, conn.QueryRowContext(tb.Context(), "SELECT 1").Scan(&one))
	require.Equal(tb, 1, one)
	return conn
}

func TestPGTestDatabaseParallelSubtestCleanupKeepsSiblingAlive(t *testing.T) {
	t.Parallel()
	// Ensure TEST_PG_URL is configured without creating a parent-owned DB.
	server, _ := testPGServer(t)

	const schema = pgPushTestSchema
	subA := newScopedTB(t, t.Name()+"/finishes_first")
	subB := newScopedTB(t, t.Name()+"/stays_open")

	dsnA := testPGSchemaURL(subA, schema)
	freshA := takeFreshSchemaCopy(subA, dsnA, schema)
	openTestPGConn(subA, dsnA)

	dsnB := testPGSchemaURL(subB, schema)
	freshB := takeFreshSchemaCopy(subB, dsnB, schema)
	connB := openTestPGConn(subB, dsnB)

	assert.NotEqual(t, dsnA, dsnB, "sibling subtests without a parent DB must get separate databases")
	if !server.cockroach {
		assert.True(t, freshA, "first sibling must get a fresh template copy")
		assert.True(t, freshB, "second sibling must also get a fresh template copy")
	}

	// First subtest finishes and runs its Cleanup (including DROP DATABASE WITH FORCE).
	subA.finish()

	// Sibling subtest's open connection must remain usable (no SQLSTATE 57P01).
	var one int
	require.NoError(t, connB.QueryRowContext(subB.Context(), "SELECT 1").Scan(&one),
		"sibling connection must survive peer subtest cleanup")
	assert.Equal(t, 1, one)

	subB.finish()
}

func TestPGTestDatabasePrefixNamedParallelSubtestsSurvivePeerCleanup(t *testing.T) {
	t.Parallel()
	testPGServer(t)

	// Go's testing.T.Run does not escape "/" in subtest names, so parallel
	// siblings named "a" and "a/b" share a slash-delimited prefix.
	subA := newScopedTB(t, t.Name()+"/a")
	subB := newScopedTB(t, t.Name()+"/a/b")

	dsnA := testPGURL(subA)
	openTestPGConn(subA, dsnA)

	dsnB := testPGURL(subB)
	connB := openTestPGConn(subB, dsnB)

	// Even if subA finishes first, subB's connection must remain alive.
	subA.finish()

	var one int
	require.NoError(t, connB.QueryRowContext(subB.Context(), "SELECT 1").Scan(&one),
		"prefix-named sibling must survive peer cleanup")
	assert.Equal(t, 1, one)

	subB.finish()
}

func TestPGTestDatabaseParentOwnedSharedWithSubtests(t *testing.T) {
	t.Parallel()
	testPGServer(t)

	parent := newScopedTB(t, t.Name()+"/parent")
	childA := newScopedTB(t, parent.Name()+"/child_a")
	childB := newScopedTB(t, parent.Name()+"/child_b")
	grandchild := newScopedTB(t, childB.Name()+"/grandchild")

	dsnParent := testPGURL(parent)
	connParent := openTestPGConn(parent, dsnParent)

	dsnA := testPGURL(childA)
	dsnB := testPGURL(childB)
	dsnGrandchild := testPGURL(grandchild)

	assert.Equal(t, dsnParent, dsnA)
	assert.Equal(t, dsnParent, dsnB)
	assert.Equal(t, dsnParent, dsnGrandchild)

	connB := openTestPGConn(childB, dsnB)

	// Subtest cleanups must not drop the parent's database.
	childA.finish()
	grandchild.finish()

	var one int
	require.NoError(t, connB.QueryRowContext(childB.Context(), "SELECT 1").Scan(&one))
	childB.finish()

	require.NoError(t, connParent.QueryRowContext(parent.Context(), "SELECT 1").Scan(&one))
	parent.finish()

	// After the owning parent finishes, the database must be dropped.
	server, _ := testPGServer(t)
	if !server.cockroach {
		var exists bool
		require.NoError(t, server.admin.QueryRowContext(
			t.Context(),
			`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`,
			pgDatabaseName(t, dsnParent),
		).Scan(&exists))
		assert.False(t, exists, "parent database must be dropped once parent finishes")
	}
}

func TestPGTestDatabaseParallelHostedFixturesSurvivePeerCleanup(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)

	// Exercise the hosted template path (testPGHostedURL + takeFreshSchemaCopy)
	// across sibling parallel subtests whose parent holds no database.
	const schema = "hosted_test_tenant_projection"
	const tenant = "tenant-projection"
	subA := newScopedTB(t, t.Name()+"/exclude_merged_short_variant")
	subB := newScopedTB(t, t.Name()+"/empty_trash")

	dsnA := testPGHostedURL(subA, schema, tenant)
	freshA := takeFreshSchemaCopy(subA, dsnA, schema)
	openTestPGConn(subA, dsnA)

	dsnB := testPGHostedURL(subB, schema, tenant)
	freshB := takeFreshSchemaCopy(subB, dsnB, schema)
	connB := openTestPGConn(subB, dsnB)

	assert.NotEqual(t, dsnA, dsnB)
	if !server.cockroach {
		assert.True(t, freshA)
		assert.True(t, freshB)
	}

	subA.finish()

	var one int
	require.NoError(t, connB.QueryRowContext(subB.Context(), "SELECT 1").Scan(&one),
		"sibling hosted connection must survive peer subtest cleanup")
	assert.Equal(t, 1, one)

	subB.finish()
}

func TestPGTestSlotConcurrentAcquireReleaseAndSingleSlotReentry(t *testing.T) {
	t.Parallel()
	server := &pgTestServer{slots: 1, slotDir: t.TempDir()}

	// Parent holds the sole slot; a descendant must inherit it without deadlocking.
	parent := newScopedTB(t, t.Name()+"/parent")
	child := newScopedTB(t, parent.Name()+"/child")
	releaseParent := acquirePGTestSlot(parent, server)
	releaseChild := acquirePGTestSlot(child, server)
	releaseChild(child)
	releaseParent(parent)

	// Concurrent acquire/release for the same test must keep the slot refcount
	// balanced and release the underlying flock once every holder finishes.
	owner := newScopedTB(t, t.Name()+"/shared")
	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			release := acquirePGTestSlot(owner, server)
			release(owner)
		})
	}
	close(start)
	wg.Wait()

	// The single slot must be free again for an unrelated test.
	other := newScopedTB(t, t.Name()+"/other")
	releaseOther := acquirePGTestSlot(other, server)
	releaseOther(other)
}

func TestPGTestDatabaseConcurrentSameTestSharesSingleDatabase(t *testing.T) {
	t.Parallel()
	testPGServer(t)

	owner := newScopedTB(t, t.Name()+"/concurrent_owner")
	const workers = 6
	start := make(chan struct{})
	dsns := make([]string, workers)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			<-start
			dsns[i] = testPGURL(owner)
		})
	}
	close(start)
	wg.Wait()

	for i := 1; i < workers; i++ {
		assert.Equal(t, dsns[0], dsns[i], "concurrent callers for the same test must share one database")
	}

	openTestPGConn(owner, dsns[0])
	owner.finish()
}

func TestPGTestDatabaseRealParallelSubtests(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pgURL := testPGSchemaURL(t, pgPushTestSchema)
			conn := openTestPGConn(t, pgURL)
			var one int
			require.NoError(t, conn.QueryRowContext(t.Context(), "SELECT 1").Scan(&one))
			assert.Equal(t, 1, one)
		})
	}
}
