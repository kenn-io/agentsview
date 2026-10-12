// Package chtest starts a throwaway ClickHouse server for integration tests
// or reuses the one named by TEST_CLICKHOUSE_URL. Each test gets its own
// database so tests can run in parallel against one server.
package chtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image pins the server version the integration suite runs against. The
// mirror relies on lightweight DELETE, recursive CTEs, and the final=1
// setting, all available from this release line.
const Image = "clickhouse/clickhouse-server:26.8"

// noRenameFlagsSeccomp fails every renameat2 call that carries a flag with
// EINVAL, as the Linux NFS client does, and allows plain renames. ClickHouse
// exchanges tables with RENAME_EXCHANGE, so a server under this profile
// behaves like one whose data directory is on NFS.
const noRenameFlagsSeccomp = `{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["renameat2"],` +
	`"action":"SCMP_ACT_ERRNO","errnoRet":22,"args":[{"index":4,"value":0,"op":"SCMP_CMP_NE"}]}]}`

// testServer is one shared test server, started on first use.
type testServer struct {
	mu        sync.Mutex
	url       string
	container testcontainers.Container
}

var (
	defaultServer    testServer
	noExchangeServer testServer
)

// ServerURL returns the DSN of the shared test server, starting a container
// on first use unless TEST_CLICKHOUSE_URL is set. It skips the test when
// Docker is unavailable and no URL is configured.
func ServerURL(tb testing.TB) string {
	tb.Helper()
	return defaultServer.start(tb, "TEST_CLICKHOUSE_URL", nil)
}

// NoExchangeServerURL returns the DSN of a shared test server that cannot
// exchange tables, like one whose data directory is on NFS. It starts a
// container under a seccomp profile on first use unless
// TEST_CLICKHOUSE_NO_EXCHANGE_URL is set, and skips the test when Docker is
// unavailable and no URL is configured.
func NoExchangeServerURL(tb testing.TB) string {
	tb.Helper()
	return noExchangeServer.start(tb, "TEST_CLICKHOUSE_NO_EXCHANGE_URL", func(hc *dockercontainer.HostConfig) {
		hc.SecurityOpt = append(hc.SecurityOpt, "seccomp="+noRenameFlagsSeccomp)
	})
}

func (s *testServer) start(tb testing.TB, urlEnv string, modify func(*dockercontainer.HostConfig)) string {
	tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != "" {
		return s.url
	}
	if env := os.Getenv(urlEnv); env != "" {
		s.url = env
		return s.url
	}
	if os.Getenv("TEST_CLICKHOUSE_SKIP_CONTAINER") != "" {
		tb.Skip(urlEnv + " not set and container start disabled")
	}
	ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image:              Image,
		ExposedPorts:       []string{"9000/tcp", "8123/tcp"},
		Env:                map[string]string{"CLICKHOUSE_SKIP_USER_SETUP": "1"},
		HostConfigModifier: modify,
		WaitingFor: wait.ForHTTP("/ping").WithPort("8123/tcp").
			WithStartupTimeout(2 * time.Minute),
		Started: true,
	})
	if err != nil {
		tb.Skipf("clickhouse container unavailable: %v", err)
	}
	host, err := c.Host(ctx)
	require.NoError(tb, err)
	port, err := c.MappedPort(ctx, "9000/tcp")
	require.NoError(tb, err)
	s.container = c
	s.url = fmt.Sprintf("clickhouse://default:@%s:%s/default", host, port.Port())
	return s.url
}

// Terminate stops the shared containers. Call it from TestMain after m.Run.
func Terminate() {
	defaultServer.terminate()
	noExchangeServer.terminate()
}

func (s *testServer) terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = s.container.Terminate(ctx)
	s.container = nil
}

// FreshDatabase creates an empty database on the test server and drops it
// when the test ends. It returns the DSN (pointing at the server default
// database) and the new database name, matching how operators configure a
// URL plus a database key.
func FreshDatabase(tb testing.TB) (dsn, database string) {
	tb.Helper()
	return FreshDatabaseOn(tb, ServerURL(tb))
}

// FreshDatabaseOn creates an empty database on the server dsn names, as
// FreshDatabase does on the shared test server.
func FreshDatabaseOn(tb testing.TB, dsn string) (string, string) {
	tb.Helper()
	var suffix [6]byte
	_, err := rand.Read(suffix[:])
	require.NoError(tb, err)
	database := "agentsview_test_" + hex.EncodeToString(suffix[:])
	admin := open(tb, dsn)
	ctx := tb.Context()
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+database)
	require.NoError(tb, err)
	tb.Cleanup(func() {
		_, _ = admin.ExecContext(context.WithoutCancel(tb.Context()), "DROP DATABASE IF EXISTS "+database+" SYNC")
		admin.Close()
	})
	return dsn, database
}

// Open connects to the given database on the test server for raw
// assertions.
func Open(tb testing.TB, dsn, database string) *sql.DB {
	tb.Helper()
	opt, err := clickhouse.ParseDSN(dsn)
	require.NoError(tb, err)
	opt.Auth.Database = database
	opt.Settings = clickhouse.Settings{"final": 1}
	conn := clickhouse.OpenDB(opt)
	tb.Cleanup(func() { conn.Close() })
	require.NoError(tb, conn.PingContext(tb.Context()))
	return conn
}

func open(tb testing.TB, dsn string) *sql.DB {
	tb.Helper()
	opt, err := clickhouse.ParseDSN(dsn)
	require.NoError(tb, err)
	conn := clickhouse.OpenDB(opt)
	require.NoError(tb, conn.PingContext(tb.Context()))
	return conn
}

// Count returns COUNT(*) for a table, optionally filtered.
func Count(tb testing.TB, conn *sql.DB, table string, where string, args ...any) int {
	tb.Helper()
	query := "SELECT COUNT(*) FROM " + table
	if strings.TrimSpace(where) != "" {
		query += " WHERE " + where
	}
	var n int
	require.NoError(tb, conn.QueryRowContext(tb.Context(), query, args...).Scan(&n))
	return n
}
