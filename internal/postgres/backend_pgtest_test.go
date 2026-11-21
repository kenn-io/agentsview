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
	_, err = admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'test-password' CONNECTION LIMIT 5`)
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
	store, err := (Backend{}).OpenServeStore(t.Context(), storage.ReplicaTarget{URL: serveURL.String(), Schema: testSchema, AllowInsecure: true})
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
	open := func(serve bool) int {
		t.Helper()
		openStore := NewHostedStore
		if serve {
			openStore = NewHostedServeStore
		}
		store, err := openStore(t.Context(), f.dsn, f.schema, f.tenant, false)
		require.NoError(t, err)
		defer func() { require.NoError(t, store.Close()) }()
		return store.DB().Stats().MaxOpenConnections
	}

	require.Equal(t, servePoolConns, open(true))

	_, err := f.admin.ExecContext(t.Context(), `ALTER ROLE "`+f.role+`" CONNECTION LIMIT 3`)
	require.NoError(t, err)
	require.Equal(t, 3, open(true))
	require.Equal(t, 5, open(false))

	store, err := NewHostedServeStore(t.Context(), f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	acquireCtx, cancelAcquire := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelAcquire()
	for range 3 {
		conn, err := store.DB().Conn(acquireCtx)
		require.NoError(t, err)
		held = append(held, conn)
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := store.ListSessions(ctx, db.SessionFilter{})
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
	for store.DB().Stats().WaitCount == 0 {
		select {
		case err := <-result:
			t.Fatalf("hosted page query returned before a connection was released: %v", err)
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("hosted page query did not wait for a connection")
		}
	}
	require.NoError(t, held[0].Close())
	held = held[1:]
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-deadline.C:
		t.Fatal("hosted page query did not finish after a connection was released")
	}
}
