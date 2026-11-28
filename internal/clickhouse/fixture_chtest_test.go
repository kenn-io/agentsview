//go:build chtest

package clickhouse

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if readTemplate.drop != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := readTemplate.drop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "dropping ClickHouse read fixture:", err)
			code = 1
		}
		cancel()
	}
	chtest.Terminate()
	os.Exit(code)
}

var readTemplate struct {
	sync.Once
	name   string
	tables []string
	drop   func(context.Context) error
}

// newClonedReadStore gives a reader its own populated tables. Tests that
// exercise pushes or materialized views need the live fixture instead.
func newClonedReadStore(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	readTemplate.Do(func() {
		url, name, drop := chtest.CreateDatabaseOn(t, dsn)
		readTemplate.drop = drop
		syncer := newTestSync(t, seedFixtureArchive(t), Target{URL: url, Database: name}, storage.PusherOptions{})
		result, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
		require.Zero(t, result.Errors)
		hasView, err := preparedUsageViewExists(ctx, syncer.conn)
		require.NoError(t, err)
		if hasView {
			for _, cmd := range []string{"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage"} {
				_, err := syncer.conn.ExecContext(ctx, cmd)
				require.NoError(t, err)
			}
		}
		require.NoError(t, syncer.conn.QueryRowContext(ctx, `SELECT groupArray(name) FROM system.tables
			WHERE database = currentDatabase() AND endsWith(engine, 'MergeTree') AND NOT startsWith(name, '.')`).Scan(&readTemplate.tables))
		readTemplate.name = name
	})
	require.NotEmpty(t, readTemplate.name, "building shared ClickHouse fixture failed")
	conn := chtest.Open(t, dsn, database)
	for _, table := range readTemplate.tables {
		_, err := conn.ExecContext(ctx, "CREATE TABLE "+table+" CLONE AS "+readTemplate.name+"."+table)
		require.NoError(t, err)
	}
	store, err := NewStore(ctx, Target{URL: dsn, Database: database})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}
