//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// statementLog records the SQL a connection pool sends.
type statementLog struct {
	mu  sync.Mutex
	sql []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = append(l.sql, data.SQL)
	return ctx
}

func (*statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (l *statementLog) matching(pattern *regexp.Regexp) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, statement := range l.sql {
		if pattern.MatchString(statement) {
			out = append(out, statement)
		}
	}
	return out
}

// schemaStepStatement matches what only the full schema step sends: schema
// probes and DDL.
var schemaStepStatement = regexp.MustCompile(`(?is)^\s*(CREATE|ALTER|DROP)\b|information_schema`)

// A Sync for a target this process already prepared skips the schema step,
// as the daemon's per-request Syncs need, and runs it again when the schema
// changed or a full push asks for it.
func TestEnsureSchemaReusesSchemaThisProcessPrepared(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const schema = pgPushTestSchema
	pgURL := testPGSchemaURL(t, schema)
	local, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	newSync := func() (*Sync, *statementLog) {
		s, err := New(ctx, pgURL, schema, local, "machine-a", true, storage.PusherOptions{})
		require.NoError(t, err)
		require.NoError(t, s.pg.Close())
		cfg, err := pgx.ParseConfig(pgURL)
		require.NoError(t, err)
		cfg.RuntimeParams["search_path"] = schema
		cfg.RuntimeParams["TimeZone"] = "UTC"
		log := &statementLog{}
		cfg.Tracer = log
		s.pg = stdlib.OpenDB(*cfg)
		t.Cleanup(func() { require.NoError(t, s.pg.Close()) })
		return s, log
	}

	first, _ := newSync()
	require.NoError(t, first.EnsureSchema(ctx))

	second, log := newSync()
	require.NoError(t, second.EnsureSchema(ctx))
	assert.Empty(t, log.matching(schemaStepStatement),
		"a schema this process prepared is not checked again")

	_, err = first.pg.ExecContext(ctx, "DROP TABLE friction_findings")
	require.NoError(t, err)
	third, _ := newSync()
	require.NoError(t, third.EnsureSchema(ctx))
	assert.True(t, pgHasTable(ctx, third.pg, "friction_findings"), "a dropped table is created again")

	fourth, log := newSync()
	_, err = fourth.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, log.matching(regexp.MustCompile(`information_schema`)),
		"a full push checks the schema again")
}

// newCacheTestSync opens a Sync for schema on a database that test owns.
func newCacheTestSync(t *testing.T, pgURL, schema string, local *db.DB) *Sync {
	t.Helper()
	s, err := New(t.Context(), pgURL, schema, local, "machine-a", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.pg.Close()) })
	return s
}

// Another machine can push rows in a newer format without changing the
// schema, so a Sync that reuses this process's prepared schema still
// refuses them.
func TestEnsureSchemaReuseStillRejectsNewerData(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const schema = pgPushTestSchema
	pgURL := testPGSchemaURL(t, schema)
	local, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })

	first := newCacheTestSync(t, pgURL, schema, local)
	require.NoError(t, first.EnsureSchema(ctx))
	_, err = first.pg.ExecContext(ctx, `INSERT INTO sessions(id, machine, project, agent, data_version)
		VALUES ('newer-parser', 'machine-b', 'project', 'codex', $1)`, db.CurrentDataVersion()+1)
	require.NoError(t, err)

	err = newCacheTestSync(t, pgURL, schema, local).EnsureSchema(ctx)
	assert.True(t, db.IsDataVersionTooNew(err), "got %v", err)
}

// Optional setup that fails, as it does for a missing privilege, leaves the
// schema unrecorded, so the next Sync tries the setup again.
func TestEnsureSchemaFailedOptionalSetupIsNotRecorded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const schema = pgPushTestSchema
	pgURL := testPGSchemaURL(t, schema)
	local, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })

	owner := newCacheTestSync(t, pgURL, schema, local)
	// A type with the table's name makes CREATE TABLE IF NOT EXISTS fail.
	_, err = owner.pg.ExecContext(ctx, `DROP TABLE session_frustration_counts;
		CREATE TYPE session_frustration_counts AS ENUM ('a')`)
	require.NoError(t, err)

	s := newCacheTestSync(t, pgURL, schema, local)
	require.NoError(t, s.EnsureSchema(ctx))
	_, recorded := ensuredSchemas.Load(s.targetFingerprint)
	assert.False(t, recorded, "a schema with failed optional setup is checked again")
}
