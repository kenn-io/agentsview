package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// classifierTestEnv prepares a temp data dir and writes a
// minimal config.toml with the given user prefixes.
func classifierTestEnv(t *testing.T, prefixes []string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)

	tomlBuf := &bytes.Buffer{}
	tomlBuf.WriteString("[automated]\nprefixes = [")
	for i, p := range prefixes {
		if i > 0 {
			tomlBuf.WriteString(", ")
		}
		tomlBuf.WriteString("\"" + p + "\"")
	}
	tomlBuf.WriteString("]\n")
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "config.toml"),
		tomlBuf.Bytes(), 0o600,
	), "write config")

	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	return dir
}

// seedHash opens the DB at cfg.DBPath, runs the backfill so
// a hash gets stored, then closes.
func seedHash(t *testing.T, cfg config.Config) {
	t.Helper()
	d, err := db.Open(cfg.DBPath)
	require.NoError(t, err, "open db")
	defer d.Close()
	// Opening already runs backfill; the hash is now stored.
	_ = d
}

// readStoredHash returns the stored classifier hash from the
// stats table via a raw SQLite connection. Bypasses db.Open
// because db.Open runs the backfill, which would re-write
// the hash that this helper exists to observe (e.g. after
// runClassifierRebuild deletes it).
func readStoredHash(t *testing.T, dbPath string) string {
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

func TestClassifierRebuildClearsSQLiteHash(t *testing.T) {
	dir := classifierTestEnv(t, []string{"You are analyzing an essay"})
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)
	require.NotEmpty(t, readStoredHash(t, cfg.DBPath),
		"precondition: expected stored hash, got empty")

	require.NoError(t, runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{}, false,
	), "rebuild")

	assert.Empty(t, readStoredHash(t, cfg.DBPath),
		"expected hash cleared")
}

func TestClassifierRebuildPrintsLoadedPrefixes(t *testing.T) {
	prefixes := []string{
		"You are analyzing an essay",
		"You are grading quotes",
	}
	dir := classifierTestEnv(t, prefixes)
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	out := &bytes.Buffer{}
	require.NoError(t, runClassifierRebuild(
		context.Background(), cfg, out, false,
	), "rebuild")
	got := out.String()
	for _, p := range prefixes {
		assert.Contains(t, got, p, "output missing %q", p)
	}
	assert.Contains(t, got, "loaded 2 user automation prefix",
		"output missing count line")
	assert.Contains(t, got, "restart",
		"output missing restart reminder")
}

func TestClassifierRebuildRefusesOnHTTPTransport(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")

	tr := transport{Mode: transportHTTP, URL: "http://127.0.0.1:8080"}
	err = guardClassifierRebuild(tr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon",
		"error should mention daemon")
}

func TestClassifierRebuildRefusesOnDirectReadOnly(t *testing.T) {
	tr := transport{Mode: transportDirect, DirectReadOnly: true}
	err := guardClassifierRebuild(tr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon",
		"error should mention daemon")
}

func TestClassifierRebuildAllowsDirectWritable(t *testing.T) {
	tr := transport{Mode: transportDirect}
	assert.NoError(t, guardClassifierRebuild(tr))
}

// TestClassifierRebuildSkipsConfiguredPGByDefault confirms that
// configured sync PG is not touched by the local recovery command
// unless the caller explicitly opts in.
func TestClassifierRebuildSkipsConfiguredPGByDefault(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	cfg.PG.URL = "postgres://nobody:nobody@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=2"
	cfg.PG.AllowInsecure = true
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	require.NoError(t, runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{}, false,
	), "configured PG must be ignored unless --pg is requested")
}

// TestClassifierRebuildPGFlagHardFailsOnPGUnreachable confirms
// that when PG cleanup is explicitly requested and the connection
// fails, runClassifierRebuild returns an error instead of silently
// skipping the PG delete.
func TestClassifierRebuildPGFlagHardFailsOnPGUnreachable(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	// Point at a deliberately-unreachable PG URL. Use port 1
	// (commonly closed) so Open returns quickly without
	// blocking the test.
	cfg.PG.URL = "postgres://nobody:nobody@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=2"
	cfg.PG.AllowInsecure = true
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	err = runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{}, true,
	)
	require.Error(t, err, "expected error for unreachable PG")
	assert.True(t,
		bytes.Contains([]byte(err.Error()), []byte("PG")) ||
			bytes.Contains([]byte(err.Error()), []byte("pg")),
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
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	require.NoError(t, err, "load")
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	cfg.PG.URL = ""
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	require.NoError(t, runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{}, false,
	), "unexpected error when PG unconfigured")
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
