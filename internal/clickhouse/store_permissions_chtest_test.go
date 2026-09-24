//go:build chtest

package clickhouse

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

func TestNewStoreRequiresActivityPartsRead(t *testing.T) {
	dsn, database := chtest.FreshDatabase(t)
	admin := chtest.Open(t, dsn, database)
	ctx := t.Context()
	require.NoError(t, EnsureSchemaOn(ctx, admin))
	user := database + "_reader"
	_, err := admin.ExecContext(ctx, "CREATE USER "+user+" IDENTIFIED WITH no_password")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), "DROP USER "+user)
		require.NoError(t, err)
	})
	_, err = admin.ExecContext(ctx, "GRANT SELECT ON "+database+".* TO "+user)
	require.NoError(t, err)
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	parsed.User = url.User(user)
	target := Target{URL: parsed.String(), Database: database}

	store, err := NewStore(ctx, target)
	if store != nil {
		require.NoError(t, store.Close())
	}
	require.ErrorContains(t, err, "GRANT SELECT ON system.parts")

	_, err = admin.ExecContext(ctx, "GRANT SELECT ON system.parts TO "+user)
	require.NoError(t, err)
	store, err = NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.ActivityReportSourceProbe(ctx)
	require.NoError(t, err)
}
