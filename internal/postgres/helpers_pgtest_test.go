//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each top-level test gets its own database on the TEST_PG_URL server, so
// tests that use the same fixed schema names, or install database-wide
// extensions, can run in parallel. Subtests share their top-level test's
// database. The database is dropped when the test that created it finishes.
var pgTestDatabases = struct {
	sync.Mutex
	byTest map[string]string
}{byTest: map[string]string{}}

// testPGURL returns the URL of the calling top-level test's database,
// creating it on first use. It skips the test when TEST_PG_URL is unset.
func testPGURL(t testing.TB) string {
	t.Helper()
	test, _, _ := strings.Cut(t.Name(), "/")
	pgTestDatabases.Lock()
	dsn, ok := pgTestDatabases.byTest[test]
	pgTestDatabases.Unlock()
	if ok {
		return dsn
	}
	dsn = newPGTestDatabase(t)
	pgTestDatabases.Lock()
	pgTestDatabases.byTest[test] = dsn
	pgTestDatabases.Unlock()
	// Registered after the drop, so it runs first: a later test with the
	// same name (benchmark rounds, -count) gets a new database.
	t.Cleanup(func() {
		pgTestDatabases.Lock()
		delete(pgTestDatabases.byTest, test)
		pgTestDatabases.Unlock()
	})
	return dsn
}

// isolatedPGDatabase returns a database separate from testPGURL's, for tests
// that need several fresh databases.
func isolatedPGDatabase(t *testing.T) string {
	t.Helper()
	return newPGTestDatabase(t)
}

// testPGStandbyURL points TEST_PG_STANDBY_URL, a physical standby of the
// TEST_PG_URL server, at the calling test's database.
func testPGStandbyURL(t testing.TB) string {
	t.Helper()
	standbyURL := os.Getenv("TEST_PG_STANDBY_URL")
	if standbyURL == "" {
		t.Skip("TEST_PG_STANDBY_URL must point to a standby of TEST_PG_URL")
	}
	return withPGDatabase(t, standbyURL, pgDatabaseName(t, testPGURL(t)))
}

// pgTestServer is the process-wide administrative connection that creates
// and drops test databases. It is never closed; it lives as long as the
// test binary.
type pgTestServer struct {
	admin     *sql.DB
	cockroach bool
}

var (
	pgTestServerOnce sync.Once
	pgTestServerConn *pgTestServer
	errPGTestServer  error
)

func testPGServer(t testing.TB) (*pgTestServer, string) {
	t.Helper()
	baseURL := os.Getenv("TEST_PG_URL")
	if baseURL == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	pgTestServerOnce.Do(func() {
		admin, err := sql.Open("pgx", baseURL)
		if err != nil {
			errPGTestServer = err
			return
		}
		admin.SetMaxOpenConns(4)
		var version string
		// The admin pool outlives every test, so no test's context bounds it.
		err = admin.QueryRowContext(context.Background(), "SELECT version()").Scan(&version)
		if err != nil {
			errPGTestServer = fmt.Errorf("reading server version: %w", err)
			_ = admin.Close()
			return
		}
		pgTestServerConn = &pgTestServer{
			admin:     admin,
			cockroach: strings.HasPrefix(version, "CockroachDB"),
		}
	})
	require.NoError(t, errPGTestServer, "connecting to TEST_PG_URL")
	return pgTestServerConn, baseURL
}

// newPGTestDatabase creates an empty database and drops it when t finishes.
func newPGTestDatabase(t testing.TB) string {
	t.Helper()
	server, baseURL := testPGServer(t)
	name := fmt.Sprintf("agentsview_test_%016x", rand.Uint64())
	_, err := server.admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err, "creating test database")
	t.Cleanup(func() {
		drop := "DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"
		if server.cockroach {
			drop = "DROP DATABASE IF EXISTS " + name + " CASCADE"
		}
		_, err := server.admin.ExecContext(context.WithoutCancel(t.Context()), drop)
		assert.NoError(t, err, "dropping test database")
	})
	return withPGDatabase(t, baseURL, name)
}

func withPGDatabase(t testing.TB, dsn, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err, "parsing PostgreSQL URL")
	parsed.Path = "/" + database
	return parsed.String()
}

func pgDatabaseName(t testing.TB, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err, "parsing PostgreSQL URL")
	return strings.TrimPrefix(parsed.Path, "/")
}

func pgTableCount(
	t *testing.T, ctx context.Context, conn *sql.DB, table string,
) int {
	t.Helper()
	var count int
	row := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return count
}
