//go:build pgtest

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
func testPGURL(tb testing.TB) string {
	tb.Helper()
	dsn, _ := pgTestDatabase(tb, pgTemplate{})
	return dsn
}

// testPGSchemaURL is testPGURL for tests that only need schema to exist
// before they start. On PostgreSQL a new database is a copy of a template in
// which EnsureSchema already created schema, so the test's own EnsureSchema
// finds it current: copying and checking take about 20 ms, and creating the
// schema takes about 90 ms. Tests of how EnsureSchema creates or upgrades a
// schema use testPGURL.
func testPGSchemaURL(tb testing.TB, schema string) string {
	tb.Helper()
	return testPGTemplateURL(tb, pgTemplate{schema: schema})
}

// testPGHostedURL is testPGSchemaURL for a schema that EnsureHostedTenant
// provisions for tenant.
func testPGHostedURL(tb testing.TB, schema, tenant string) string {
	tb.Helper()
	return testPGTemplateURL(tb, pgTemplate{schema: schema, tenant: tenant})
}

func testPGTemplateURL(tb testing.TB, tmpl pgTemplate) string {
	tb.Helper()
	dsn, cloned := pgTestDatabase(tb, tmpl)
	if cloned {
		pgFreshSchemaCopies.Store(pgDatabaseName(tb, dsn), tmpl.schema)
	}
	return dsn
}

// pgTemplate names what a template database holds: schema as EnsureSchema
// creates it, or as EnsureHostedTenant provisions it when tenant is set. The
// zero value means no template.
type pgTemplate struct {
	schema string
	tenant string
}

// testPGStoreURL is testPGSchemaURL for tests that read through the
// testSchema store.
func testPGStoreURL(tb testing.TB) string {
	tb.Helper()
	return testPGSchemaURL(tb, testSchema)
}

// pgFreshSchemaCopies maps the names of template copies that no setup helper
// has used yet to the schema their template holds.
var pgFreshSchemaCopies sync.Map

// takeFreshSchemaCopy reports whether dsn is a template copy holding schema
// that no setup helper has used yet, and marks it used. A setup helper that
// drops and recreates schema skips both when this is true: the copy already
// holds a schema EnsureSchema just created.
func takeFreshSchemaCopy(tb testing.TB, dsn, schema string) bool {
	tb.Helper()
	return pgFreshSchemaCopies.CompareAndDelete(pgDatabaseName(tb, dsn), schema)
}

// pgTestDatabase returns the calling top-level test's database, creating
// it on first use, and reports whether it created a copy of the template
// tmpl. It copies a template only when tmpl is set and the server is
// PostgreSQL; CockroachDB has no database templates.
func pgTestDatabase(tb testing.TB, tmpl pgTemplate) (string, bool) {
	tb.Helper()
	test, _, _ := strings.Cut(tb.Name(), "/")
	pgTestDatabases.Lock()
	dsn, ok := pgTestDatabases.byTest[test]
	pgTestDatabases.Unlock()
	if ok {
		return dsn, false
	}
	server, _ := testPGServer(tb)
	if server.cockroach {
		tmpl = pgTemplate{}
	}
	dsn = newPGTestDatabase(tb, tmpl)
	pgTestDatabases.Lock()
	pgTestDatabases.byTest[test] = dsn
	pgTestDatabases.Unlock()
	// Registered after the drop, so it runs first: a later test with the
	// same name (benchmark rounds, -count) gets a new database.
	tb.Cleanup(func() {
		pgTestDatabases.Lock()
		delete(pgTestDatabases.byTest, test)
		pgTestDatabases.Unlock()
	})
	return dsn, tmpl != pgTemplate{}
}

// isolatedPGDatabase returns a database separate from testPGURL's, for tests
// that need several fresh databases.
func isolatedPGDatabase(t *testing.T) string {
	t.Helper()
	return newPGTestDatabase(t, pgTemplate{})
}

// testPGStandbyURL points TEST_PG_STANDBY_URL, a physical standby of the
// TEST_PG_URL server, at the calling test's database.
func testPGStandbyURL(tb testing.TB) string {
	tb.Helper()
	standbyURL := os.Getenv("TEST_PG_STANDBY_URL")
	if standbyURL == "" {
		tb.Skip("TEST_PG_STANDBY_URL must point to a standby of TEST_PG_URL")
	}
	return withPGDatabase(tb, standbyURL, pgDatabaseName(tb, testPGURL(tb)))
}

// pgTestServer is the process-wide administrative connection that creates
// and drops test databases. It is never closed; it lives as long as the
// test binary.
type pgTestServer struct {
	admin     *sql.DB
	cockroach bool
	// slots is how many top-level tests may hold databases on the server at
	// once, across every test process; zero means no limit.
	slots   int
	slotDir string
}

var (
	pgTestServerOnce sync.Once
	pgTestServerConn *pgTestServer
	errPGTestServer  error
)

func testPGServer(tb testing.TB) (*pgTestServer, string) {
	tb.Helper()
	baseURL := os.Getenv("TEST_PG_URL")
	if baseURL == "" {
		tb.Skip("TEST_PG_URL not set; skipping PG tests")
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
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute) //nolint:usetesting // shared by every test in the binary
		defer cancel()
		err = admin.QueryRowContext(ctx, "SELECT version()").Scan(&version)
		if err != nil {
			errPGTestServer = fmt.Errorf("reading server version: %w", err)
			_ = admin.Close()
			return
		}
		server := &pgTestServer{
			admin:     admin,
			cockroach: strings.HasPrefix(version, "CockroachDB"),
		}
		if !server.cockroach {
			server.slots, server.slotDir, errPGTestServer = pgTestSlotConfig(ctx, admin, baseURL)
			if errPGTestServer != nil {
				_ = admin.Close()
				return
			}
		}
		pgTestServerConn = server
	})
	require.NoError(tb, errPGTestServer, "connecting to TEST_PG_URL")
	return pgTestServerConn, baseURL
}

// pgTestSlotConfig sizes the server's test slots. Every PostgreSQL
// connection shares one lock table. Hosted schema creation and cleanup can
// exhaust it even with 12 concurrent tests on a default 100-connection server.
// CockroachDB has no shared lock table, so it gets no slots.
func pgTestSlotConfig(ctx context.Context, admin *sql.DB, baseURL string) (int, string, error) {
	var maxConnections, maxLocksPerTransaction int
	err := admin.QueryRowContext(ctx, `SELECT current_setting('max_connections')::int,
 current_setting('max_locks_per_transaction')::int`).Scan(&maxConnections, &maxLocksPerTransaction)
	if err != nil {
		return 0, "", fmt.Errorf("reading PostgreSQL test capacity: %w", err)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return 0, "", fmt.Errorf("parsing TEST_PG_URL: %w", err)
	}
	// Processes that test against the same server share the slot files, so
	// parallel shards stay inside one limit.
	sum := sha256.Sum256([]byte(parsed.Host))
	dir := filepath.Join(os.TempDir(), "agentsview-pgtest-"+hex.EncodeToString(sum[:])[:20])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", fmt.Errorf("creating test slot directory: %w", err)
	}
	// Reserve 2,048 shared lock entries per test for schema work, including
	// cleanup, while keeping the existing connection limit. The default
	// 100 connections and 64 locks per transaction permit three tests at once.
	lockSlots := maxConnections * maxLocksPerTransaction / 2048
	return max(1, min(maxConnections/8, lockSlots)), dir, nil
}

// A top-level test holds one server slot while any of its databases exist.
var pgTestSlots = struct {
	sync.Mutex
	byTest map[string]*pgTestSlot
}{byTest: map[string]*pgTestSlot{}}

type pgTestSlot struct {
	lock      *flock.Flock
	databases int
}

// holdPGTestSlot takes a server slot for t's top-level test, waiting for
// one to free up, and gives it back when the test's last database is
// dropped.
func holdPGTestSlot(tb testing.TB, server *pgTestServer) {
	tb.Helper()
	if server.slots == 0 {
		return
	}
	test, _, _ := strings.Cut(tb.Name(), "/")
	pgTestSlots.Lock()
	slot, ok := pgTestSlots.byTest[test]
	pgTestSlots.Unlock()
	if !ok {
		lock := waitPGTestSlot(tb, server)
		pgTestSlots.Lock()
		// A parallel subtest of the same test may have taken one meanwhile.
		if slot, ok = pgTestSlots.byTest[test]; ok {
			require.NoError(tb, lock.Unlock(), "releasing test slot")
		} else {
			slot = &pgTestSlot{lock: lock}
			pgTestSlots.byTest[test] = slot
		}
		pgTestSlots.Unlock()
	}
	pgTestSlots.Lock()
	slot.databases++
	pgTestSlots.Unlock()
	tb.Cleanup(func() {
		pgTestSlots.Lock()
		defer pgTestSlots.Unlock()
		slot.databases--
		if slot.databases == 0 {
			delete(pgTestSlots.byTest, test)
			assert.NoError(tb, slot.lock.Unlock(), "releasing test slot")
		}
	})
}

func waitPGTestSlot(tb testing.TB, server *pgTestServer) *flock.Flock {
	tb.Helper()
	for {
		for i := range server.slots {
			lock := flock.New(filepath.Join(server.slotDir, fmt.Sprintf("slot-%02d.lock", i)))
			locked, err := lock.TryLock()
			require.NoError(tb, err, "taking test slot")
			if locked {
				return lock
			}
		}
		select {
		case <-tb.Context().Done():
			require.FailNow(tb, "test ended while waiting for a PostgreSQL test slot")
		// Slots belong to other test processes too, so no in-process event
		// says when one frees up.
		case <-time.After(25 * time.Millisecond): //nolint:kennlint // deadlinetest: polls lock files that other processes release
		}
	}
}

// newPGTestDatabase creates a database and drops it when t finishes. The
// database is empty, or a copy of the template for tmpl when tmpl is set.
func newPGTestDatabase(tb testing.TB, tmpl pgTemplate) string {
	tb.Helper()
	server, baseURL := testPGServer(tb)
	// Registered before the drop, so the slot is released after it.
	holdPGTestSlot(tb, server)
	name := fmt.Sprintf("agentsview_test_%016x", rand.Uint64())
	create := "CREATE DATABASE " + name
	if tmpl != (pgTemplate{}) {
		create += " TEMPLATE " + pgTemplateName(tb, server, baseURL, tmpl)
	}
	_, err := server.admin.ExecContext(tb.Context(), create)
	require.NoError(tb, err, "creating test database")
	tb.Cleanup(func() {
		drop := "DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"
		if server.cockroach {
			drop = "DROP DATABASE IF EXISTS " + name + " CASCADE"
		}
		_, err := server.admin.ExecContext(context.WithoutCancel(tb.Context()), drop)
		assert.NoError(tb, err, "dropping test database")
	})
	return withPGDatabase(tb, baseURL, name)
}

// pgTemplateDatabase is a template database. Each is built once per test
// binary, by the first test that asks for it, and dropped by TestMain.
type pgTemplateDatabase struct {
	once sync.Once
	name string
	err  error
}

var pgTemplates = struct {
	sync.Mutex
	byTemplate map[pgTemplate]*pgTemplateDatabase
}{byTemplate: map[pgTemplate]*pgTemplateDatabase{}}

func pgTemplateName(tb testing.TB, server *pgTestServer, baseURL string, tmpl pgTemplate) string {
	tb.Helper()
	pgTemplates.Lock()
	database, ok := pgTemplates.byTemplate[tmpl]
	if !ok {
		database = &pgTemplateDatabase{}
		pgTemplates.byTemplate[tmpl] = database
	}
	pgTemplates.Unlock()
	database.once.Do(func() {
		// Every test that copies the template shares this build, so no one
		// test's context bounds it. Building takes well under a second.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) //nolint:usetesting // shared by every test in the binary
		defer cancel()
		database.name, database.err = buildPGTemplate(ctx, server, baseURL, tmpl)
	})
	require.NoError(tb, database.err, "building template for schema %s", tmpl.schema)
	return database.name
}

// buildPGTemplate returns the template's name even when building it fails
// after the database exists, so TestMain still drops it.
func buildPGTemplate(ctx context.Context, server *pgTestServer, baseURL string, tmpl pgTemplate) (string, error) {
	name := fmt.Sprintf("agentsview_test_template_%016x", rand.Uint64())
	if _, err := server.admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		return "", fmt.Errorf("creating template: %w", err)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return name, fmt.Errorf("parsing TEST_PG_URL: %w", err)
	}
	parsed.Path = "/" + name
	pg, err := Open(ctx, parsed.String(), tmpl.schema, true)
	if err != nil {
		return name, fmt.Errorf("connecting to template: %w", err)
	}
	if tmpl.tenant == "" {
		err = EnsureSchema(ctx, pg, tmpl.schema)
	} else {
		err = EnsureHostedTenant(ctx, pg, tmpl.schema, tmpl.tenant)
	}
	// CREATE DATABASE ... TEMPLATE fails while anyone is connected to the
	// template, so no connection may outlive the build.
	if closeErr := pg.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("closing template: %w", closeErr)
	}
	return name, err
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, database := range pgTemplates.byTemplate {
		if database.name == "" {
			continue
		}
		// Runs after every test has ended, so no test's context remains.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, err := pgTestServerConn.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+database.name+" WITH (FORCE)")
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dropping template %s: %v\n", database.name, err)
			code = max(code, 1)
		}
	}
	os.Exit(code)
}

func withPGDatabase(tb testing.TB, dsn, database string) string {
	tb.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(tb, err, "parsing PostgreSQL URL")
	parsed.Path = "/" + database
	return parsed.String()
}

func pgDatabaseName(tb testing.TB, dsn string) string {
	tb.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(tb, err, "parsing PostgreSQL URL")
	return strings.TrimPrefix(parsed.Path, "/")
}

func pgTableCount(
	t *testing.T, ctx context.Context, conn *sql.DB, table string,
) int {
	t.Helper()
	var count int
	row := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table)
	require.NoError(t, row.Scan(&count), "counting %s", table)
	return count
}
