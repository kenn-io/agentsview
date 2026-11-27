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

// Each test that asks for a database gets its own database on the TEST_PG_URL
// server, so tests that use the same fixed schema names, or install
// database-wide extensions, can run in parallel. Subtests inherit an ancestor
// test's database when one already exists; sibling subtests whose parent did
// not create a database each get their own. A database is dropped when the
// last test referencing it finishes.
type pgTestDB struct {
	key    string
	ready  chan struct{}
	dsn    string
	name   string
	server *pgTestServer
	slot   *pgTestSlot
	refs   int
	failed bool
}

var pgTestDatabases = struct {
	sync.Mutex
	byTest map[string]*pgTestDB
}{byTest: map[string]*pgTestDB{}}

// testPGURL returns the URL of the calling test's database (or its ancestor's),
// creating it on first use. It skips the test when TEST_PG_URL is unset.
func testPGURL(tb testing.TB) string {
	tb.Helper()
	return pgTestDatabase(tb, pgTemplate{})
}

// testPGSchemaURL is testPGURL for tests that only need schema to exist
// before they start. On PostgreSQL a new database is a copy of a template in
// which EnsureSchema already created schema, so the test's own EnsureSchema
// finds it current: copying and checking take about 20 ms, and creating the
// schema takes about 90 ms. Tests of how EnsureSchema creates or upgrades a
// schema use testPGURL.
func testPGSchemaURL(tb testing.TB, schema string) string {
	tb.Helper()
	return pgTestDatabase(tb, pgTemplate{schema: schema})
}

// testPGHostedURL is testPGSchemaURL for a schema that EnsureHostedTenant
// provisions for tenant.
func testPGHostedURL(tb testing.TB, schema, tenant string) string {
	tb.Helper()
	return pgTestDatabase(tb, pgTemplate{schema: schema, tenant: tenant})
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

// findPGTestDBLocked returns the database owned by name or its nearest
// ancestor test, if one exists. Callers must hold pgTestDatabases.Mutex.
func findPGTestDBLocked(name string) *pgTestDB {
	for cur := name; ; {
		if entry, ok := pgTestDatabases.byTest[cur]; ok {
			return entry
		}
		idx := strings.LastIndexByte(cur, '/')
		if idx < 0 {
			return nil
		}
		cur = cur[:idx]
	}
}

// pgTestDatabase returns the calling test's database (inheriting an ancestor
// test's database when one already exists), creating it on first use. It
// copies a template only when tmpl is set and the server is PostgreSQL;
// CockroachDB has no database templates.
func pgTestDatabase(tb testing.TB, tmpl pgTemplate) string {
	tb.Helper()
	// Validate server access before publishing an in-flight entry so
	// skipped tests leave no entry behind.
	server, baseURL := testPGServer(tb)
	name := tb.Name()
	for {
		pgTestDatabases.Lock()
		if entry := findPGTestDBLocked(name); entry != nil {
			entry.refs++
			pgTestDatabases.Unlock()
			<-entry.ready
			if entry.failed {
				continue
			}
			tb.Cleanup(func() { releasePGTestDatabase(tb, entry) })
			return entry.dsn
		}
		entry := &pgTestDB{
			key:   name,
			ready: make(chan struct{}),
			refs:  1,
		}
		pgTestDatabases.byTest[name] = entry
		pgTestDatabases.Unlock()

		if server.cockroach {
			tmpl = pgTemplate{}
		}
		return finishPGTestDatabase(tb, server, baseURL, tmpl, entry)
	}
}

func finishPGTestDatabase(
	tb testing.TB, server *pgTestServer, baseURL string, tmpl pgTemplate, entry *pgTestDB,
) string {
	tb.Helper()
	created := false
	defer func() {
		if created {
			return
		}
		pgTestDatabases.Lock()
		entry.failed = true
		if pgTestDatabases.byTest[entry.key] == entry {
			delete(pgTestDatabases.byTest, entry.key)
		}
		pgTestDatabases.Unlock()
		close(entry.ready)
	}()

	dsn, dbName, slot := createPGTestDatabase(tb, server, baseURL, tmpl)
	if tmpl != (pgTemplate{}) {
		pgFreshSchemaCopies.Store(dbName, tmpl.schema)
	}
	entry.dsn = dsn
	entry.name = dbName
	entry.server = server
	entry.slot = slot
	created = true
	tb.Cleanup(func() { releasePGTestDatabase(tb, entry) })
	close(entry.ready)
	return dsn
}

func releasePGTestDatabase(tb testing.TB, entry *pgTestDB) {
	tb.Helper()
	pgTestDatabases.Lock()
	entry.refs--
	if entry.refs > 0 {
		pgTestDatabases.Unlock()
		return
	}
	if pgTestDatabases.byTest[entry.key] == entry {
		delete(pgTestDatabases.byTest, entry.key)
	}
	pgTestDatabases.Unlock()

	pgFreshSchemaCopies.Delete(entry.name)
	dropPGTestDatabase(tb, entry.server, entry.name)
	releasePGTestSlot(tb, entry.slot)
}

// isolatedPGDatabase returns a database separate from testPGURL's, for tests
// that need several fresh databases.
func isolatedPGDatabase(t *testing.T) string {
	t.Helper()
	server, baseURL := testPGServer(t)
	dsn, dbName, slot := createPGTestDatabase(t, server, baseURL, pgTemplate{})
	t.Cleanup(func() {
		dropPGTestDatabase(t, server, dbName)
		releasePGTestSlot(t, slot)
	})
	return dsn
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
	// slots is how many database-owning tests or subtests may hold databases on
	// the server at once, across every test process; zero means no limit.
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

// Each test that owns databases holds one server slot while any of its
// databases exist. Subtests inherit an ancestor's slot when one is already
// held, avoiding self-deadlock on single-slot servers, while sibling subtests
// without a parent slot acquire separate slots so concurrent subtest databases
// stay within the server's lock-memory budget.
var pgTestSlots = struct {
	sync.Mutex
	byTest map[string]*pgTestSlot
}{byTest: map[string]*pgTestSlot{}}

type pgTestSlot struct {
	key       string
	lock      *flock.Flock
	databases int
}

func findPGTestSlotLocked(name string) *pgTestSlot {
	for cur := name; ; {
		if slot, ok := pgTestSlots.byTest[cur]; ok {
			return slot
		}
		idx := strings.LastIndexByte(cur, '/')
		if idx < 0 {
			return nil
		}
		cur = cur[:idx]
	}
}

// acquirePGTestSlot takes a server slot for tb (or joins an ancestor's held
// slot), waiting for one to free up, and returns the slot to pass to
// releasePGTestSlot after the database is dropped. It returns nil when
// server.slots is zero.
func acquirePGTestSlot(tb testing.TB, server *pgTestServer) *pgTestSlot {
	tb.Helper()
	if server.slots == 0 {
		return nil
	}
	name := tb.Name()
	for {
		pgTestSlots.Lock()
		if slot := findPGTestSlotLocked(name); slot != nil {
			slot.databases++
			pgTestSlots.Unlock()
			return slot
		}
		pgTestSlots.Unlock()
		for i := range server.slots {
			lock := flock.New(filepath.Join(server.slotDir, fmt.Sprintf("slot-%02d.lock", i)))
			locked, err := lock.TryLock()
			require.NoError(tb, err, "taking test slot")
			if locked {
				pgTestSlots.Lock()
				if slot := findPGTestSlotLocked(name); slot != nil {
					slot.databases++
					pgTestSlots.Unlock()
					assert.NoError(tb, lock.Unlock(), "releasing test slot")
					return slot
				}
				slot := &pgTestSlot{key: name, lock: lock, databases: 1}
				pgTestSlots.byTest[name] = slot
				pgTestSlots.Unlock()
				return slot
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

func releasePGTestSlot(tb testing.TB, slot *pgTestSlot) {
	tb.Helper()
	if slot == nil {
		return
	}
	pgTestSlots.Lock()
	slot.databases--
	var releaseLock *flock.Flock
	if slot.databases == 0 {
		if pgTestSlots.byTest[slot.key] == slot {
			delete(pgTestSlots.byTest, slot.key)
		}
		releaseLock = slot.lock
	}
	pgTestSlots.Unlock()
	if releaseLock != nil {
		assert.NoError(tb, releaseLock.Unlock(), "releasing test slot")
	}
}

// createPGTestDatabase acquires a server slot and creates a database (empty, or
// a copy of tmpl's template when tmpl is set). The caller owns dropping the
// database and releasing the returned slot afterward.
func createPGTestDatabase(
	tb testing.TB, server *pgTestServer, baseURL string, tmpl pgTemplate,
) (string, string, *pgTestSlot) {
	tb.Helper()
	name := fmt.Sprintf("agentsview_test_%016x", rand.Uint64())
	dsn := withPGDatabase(tb, baseURL, name)
	slot := acquirePGTestSlot(tb, server)
	created := false
	defer func() {
		if !created {
			releasePGTestSlot(tb, slot)
		}
	}()
	create := "CREATE DATABASE " + name
	if tmpl != (pgTemplate{}) {
		create += " TEMPLATE " + pgTemplateName(tb, server, baseURL, tmpl)
	}
	if !server.cockroach {
		// FILE_COPY copies files instead of logging every page to the WAL.
		// On a four-CPU run of the CI shards it cut PostgreSQL CPU by about
		// 10%. It needs PostgreSQL 15 or later.
		create += " STRATEGY FILE_COPY"
	}
	_, err := server.admin.ExecContext(tb.Context(), create)
	require.NoError(tb, err, "creating test database")
	created = true
	return dsn, name, slot
}

func dropPGTestDatabase(tb testing.TB, server *pgTestServer, name string) {
	tb.Helper()
	drop := "DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"
	if server.cockroach {
		drop = "DROP DATABASE IF EXISTS " + name + " CASCADE"
	}
	_, err := server.admin.ExecContext(context.WithoutCancel(tb.Context()), drop)
	assert.NoError(tb, err, "dropping test database")
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
	cacheDir, err := os.MkdirTemp("", "agentsview-test-cache-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CACHE_DIRECTORY", cacheDir); err != nil {
		panic(err)
	}
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
	_ = os.RemoveAll(cacheDir)
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
