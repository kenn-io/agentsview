//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestServePageWaitsAtRoleConnectionLimit(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("PostgreSQL role connection limits are not supported by CockroachDB")
	}
	pgURL := testPGStoreURL(t)
	admin, err := Open(t.Context(), pgURL, testSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	role := fmt.Sprintf("page_limit_%x", rand.Uint64())
	_, err = admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'test-password' CONNECTION LIMIT 10`)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := admin.ExecContext(ctx, `DROP OWNED BY `+role+`; DROP ROLE `+role)
		require.NoError(t, err)
	})
	_, err = admin.ExecContext(t.Context(), `GRANT USAGE ON SCHEMA `+testSchema+` TO `+role+`; GRANT SELECT ON ALL TABLES IN SCHEMA `+testSchema+` TO `+role)
	require.NoError(t, err)

	serveURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	serveURL.User = url.UserPassword(role, "test-password")
	target := storage.ReplicaTarget{URL: serveURL.String(), Schema: testSchema, AllowInsecure: true}
	initial, err := (Backend{}).OpenServeStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() {
		if initial != nil {
			_ = initial.Close()
		}
	})
	require.Equal(t, 10, initial.(*Store).DB().Stats().MaxOpenConnections)
	require.NoError(t, initial.Close())
	initial = nil
	_, err = admin.ExecContext(t.Context(), `ALTER ROLE `+role+` CONNECTION LIMIT 5`)
	require.NoError(t, err)
	store, err := (Backend{}).OpenServeStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	pgStore := store.(*Store)

	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	acquireCtx, cancelAcquire := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelAcquire()
	for range 5 {
		conn, err := pgStore.DB().Conn(acquireCtx)
		require.NoError(t, err)
		held = append(held, conn)
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := pgStore.ListSessions(ctx, db.SessionFilter{})
		result <- err
	})
	defer func() {
		cancel()
		wg.Wait()
	}()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for pgStore.DB().Stats().WaitCount == 0 {
		select {
		case err := <-result:
			t.Fatalf("page query returned before a connection was released: %v", err)
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("page query did not wait for a connection")
		}
	}
	require.NoError(t, held[0].Close())
	held = held[1:]
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-deadline.C:
		t.Fatal("page query did not finish after a connection was released")
	}
}

func TestHostedServeStoreHonorsRoleConnectionLimit(t *testing.T) {
	t.Parallel()
	server, _ := testPGServer(t)
	if server.cockroach {
		t.Skip("PostgreSQL role connection limits are not supported by CockroachDB")
	}
	f := newHostedFixture(t, "tenant-serve-limit")
	// The fixture's own pool holds role connections the limit would count.
	require.NoError(t, f.runtime.Close())
	open := func() int {
		t.Helper()
		store, err := NewHostedServeStore(t.Context(), f.dsn, f.schema, f.tenant, false)
		require.NoError(t, err)
		defer func() { require.NoError(t, store.Close()) }()
		return store.DB().Stats().MaxOpenConnections
	}

	require.Equal(t, servePoolConns, open())

	_, err := f.admin.ExecContext(t.Context(), `ALTER ROLE "`+f.role+`" CONNECTION LIMIT 3`)
	require.NoError(t, err)
	require.Equal(t, 3, open())
}
