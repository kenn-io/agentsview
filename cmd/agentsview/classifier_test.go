package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// unreachablePGURL points at a deliberately-closed port (1) so
// postgres.Open returns quickly without blocking the test.
const unreachablePGURL = "postgres://nobody:nobody@127.0.0.1:1/" +
	"nonexistent?sslmode=disable&connect_timeout=2"

// classifierFixture owns the per-test data dir and the config
// wired to it. Construct it with newClassifierFixture.
type classifierFixture struct {
	Dir string
	Cfg config.Config
}

// newClassifierFixture prepares a temp data dir with a minimal
// config.toml carrying the given user prefixes, loads a minimal
// config pointed at that dir, applies any option mutators, and
// installs the classifier patterns into the db singleton.
func newClassifierFixture(
	t *testing.T, prefixes []string, opts ...func(*config.Config),
) classifierFixture {
	t.Helper()
	dir := testDataDir(t)
	writeAutomatedPrefixesConfig(t, dir, prefixes)

	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	for _, opt := range opts {
		opt(&cfg)
	}
	applyClassifierConfig(cfg)

	t.Cleanup(func() {
		db.SetUserAutomationPrefixes(nil)
		db.SetUserAutomationSubstrings(nil)
		db.SetUserAutomationExactMatches(nil)
	})
	return classifierFixture{Dir: dir, Cfg: cfg}
}

// withUnreachablePG configures a PG URL that cannot be reached so
// tests can exercise the PG cleanup path without a live database.
func withUnreachablePG(cfg *config.Config) {
	cfg.PG.URL = unreachablePGURL
	cfg.PG.AllowInsecure = true
}

// withoutPG clears any configured PG URL.
func withoutPG(cfg *config.Config) {
	cfg.PG.URL = ""
}

// writeAutomatedPrefixesConfig writes a config.toml under dir with
// the given automated prefixes, quoting each via strconv.Quote so
// prefixes containing quotes or backslashes stay valid TOML.
func writeAutomatedPrefixesConfig(t *testing.T, dir string, prefixes []string) {
	t.Helper()
	quoted := make([]string, len(prefixes))
	for i, p := range prefixes {
		quoted[i] = strconv.Quote(p)
	}
	toml := "[automated]\nprefixes = [" + strings.Join(quoted, ", ") + "]\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "config.toml"),
		[]byte(toml), 0o600,
	), "write config")
}

// seedClassifierHash opens the DB at cfg.DBPath, which runs the
// backfill so a classifier hash gets stored, then closes.
func seedClassifierHash(t *testing.T, cfg config.Config) {
	t.Helper()
	d, err := db.Open(cfg.DBPath)
	require.NoError(t, err, "open db")
	require.NoError(t, d.Close(), "close db")
}

// classifierHashInSQLite returns the stored classifier hash from
// the stats table via a raw SQLite connection. Bypasses db.Open
// because db.Open runs the backfill, which would re-write the hash
// that this helper exists to observe (e.g. after runClassifierRebuild
// deletes it).
func classifierHashInSQLite(t *testing.T, dbPath string) string {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open raw sqlite")
	defer conn.Close()
	var v string
	err = conn.QueryRow(
		`SELECT value FROM stats WHERE key = ?`,
		db.ClassifierHashKey,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err, "query stats")
	return v
}

// runClassifierRebuildTest runs runClassifierRebuild against cfg,
// returning captured output and any error.
func runClassifierRebuildTest(
	t *testing.T, cfg config.Config, includePG bool,
) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	err := runClassifierRebuild(context.Background(), cfg, out, includePG)
	return out.String(), err
}

// requireClassifierRebuild runs the rebuild and asserts it succeeds,
// returning the captured output.
func requireClassifierRebuild(
	t *testing.T, cfg config.Config, includePG bool,
) string {
	t.Helper()
	out, err := runClassifierRebuildTest(t, cfg, includePG)
	require.NoError(t, err, "rebuild")
	return out
}

func TestClassifierRebuildClearsSQLiteHash(t *testing.T) {
	fx := newClassifierFixture(t, []string{"You are analyzing an essay"})
	seedClassifierHash(t, fx.Cfg)
	require.NotEmpty(t, classifierHashInSQLite(t, fx.Cfg.DBPath),
		"precondition: expected stored hash, got empty")

	requireClassifierRebuild(t, fx.Cfg, false)

	assert.Empty(t, classifierHashInSQLite(t, fx.Cfg.DBPath),
		"expected hash cleared")
}

func TestClassifierRebuildPrintsLoadedPrefixes(t *testing.T) {
	prefixes := []string{
		"You are analyzing an essay",
		"You are grading quotes",
	}
	fx := newClassifierFixture(t, prefixes)
	seedClassifierHash(t, fx.Cfg)

	got := requireClassifierRebuild(t, fx.Cfg, false)
	for _, p := range prefixes {
		assert.Contains(t, got, p, "output missing %q", p)
	}
	assert.Contains(t, got, "loaded 2 user automation prefix",
		"output missing count line")
	assert.Contains(t, got, "restart",
		"output missing restart reminder")
}

func TestClassifierRebuildGuard(t *testing.T) {
	tests := []struct {
		name    string
		tr      transport
		wantErr bool
	}{
		{
			name:    "http transport refused",
			tr:      transport{Mode: transportHTTP, URL: "http://127.0.0.1:8080"},
			wantErr: true,
		},
		{
			name:    "direct read-only refused",
			tr:      transport{Mode: transportDirect, DirectReadOnly: true},
			wantErr: true,
		},
		{
			name:    "direct writable allowed",
			tr:      transport{Mode: transportDirect},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := guardClassifierRebuild(tt.tr)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "daemon",
				"error should mention daemon")
		})
	}
}

func TestClassifierRebuildRefusesBackgroundLaunchLock(t *testing.T) {
	fx := newClassifierFixture(t, nil)
	require.NoError(t, os.MkdirAll(fx.Dir, 0o700))
	launchLock, ok := acquireBackgroundLaunchLock(fx.Dir)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, launchLock.Unlock()) })

	_, err := runClassifierRebuildTest(t, fx.Cfg, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon launch is in progress")
}

// TestClassifierRebuildSkipsConfiguredPGByDefault confirms that
// configured sync PG is not touched by the local recovery command
// unless the caller explicitly opts in.
func TestClassifierRebuildSkipsConfiguredPGByDefault(t *testing.T) {
	fx := newClassifierFixture(t, nil, withUnreachablePG)
	seedClassifierHash(t, fx.Cfg)

	requireClassifierRebuild(t, fx.Cfg, false)
}

// TestClassifierRebuildPGFlagHardFailsOnPGUnreachable confirms
// that when PG cleanup is explicitly requested and the connection
// fails, runClassifierRebuild returns an error instead of silently
// skipping the PG delete.
func TestClassifierRebuildPGFlagHardFailsOnPGUnreachable(t *testing.T) {
	fx := newClassifierFixture(t, nil, withUnreachablePG)
	seedClassifierHash(t, fx.Cfg)

	_, err := runClassifierRebuildTest(t, fx.Cfg, true)
	require.Error(t, err, "expected error for unreachable PG")
	lower := strings.ToLower(err.Error())
	assert.Contains(t, lower, "pg",
		"error should mention PG, got: %v", err)
	// Lock the spec contract: the error must surface the
	// 'pg push --full' remediation hint so a future refactor
	// can't silently drop it.
	assert.Contains(t, err.Error(), "pg push --full",
		"error should mention 'pg push --full' remediation")
}

// TestClassifierRebuildSkipsPGWhenNotConfigured verifies the
// silent-skip path: when pg.url is empty, the command does
// NOT attempt PG cleanup and returns nil even if PG would
// otherwise be unreachable.
func TestClassifierRebuildSkipsPGWhenNotConfigured(t *testing.T) {
	fx := newClassifierFixture(t, nil, withoutPG)
	seedClassifierHash(t, fx.Cfg)

	requireClassifierRebuild(t, fx.Cfg, false)
}

// TestClassifierCommandIsHidden pins the UX decision that the
// classifier group does not appear in `agentsview --help`.
// Routine config edits are auto-detected on daemon restart;
// this group is a recovery hatch.
func TestClassifierCommandIsHidden(t *testing.T) {
	cmd := newClassifierCommand()
	assert.True(t, cmd.Hidden,
		"classifier command should be Hidden=true; got false")
}
