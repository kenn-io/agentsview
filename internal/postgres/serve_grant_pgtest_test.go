//go:build pgtest

package postgres

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PostgreSQL skips a search_path schema the role has no USAGE on, so the
// schema check sees missing tables and functions, not a permission error.
// Serve must still ask for the grant: the rebuild advice would discard the
// pushed data.
func TestServeWithoutSchemaUsageAsksForTheGrant(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_serve_usage_test"
	const role = "agentsview_serve_no_usage"
	const rolePassword = "agentsview_serve_no_usage_pw"

	admin, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(t.Context(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))

	_, _ = admin.ExecContext(t.Context(), `DROP OWNED BY `+role)
	_, _ = admin.ExecContext(t.Context(), `DROP ROLE IF EXISTS `+role)
	_, err = admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD '`+rolePassword+`'`)
	require.NoError(t, err, "create role without schema access")
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, _ = admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_, _ = admin.ExecContext(ctx, `DROP OWNED BY `+role)
		_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+role)
	})

	serveURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	serveURL.User = url.UserPassword(role, rolePassword)
	store, err := NewStore(serveURL.String(), schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	err = prepareServeStore(t.Context(), store, schema)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `GRANT USAGE ON SCHEMA "agentsview_serve_usage_test" TO <serve role>;`)
	assert.NotContains(t, err.Error(), "Drop and recreate")
}
