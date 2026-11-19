package db

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveReaderReusesSessionSQL(t *testing.T) {
	d := testDB(t)
	for _, session := range []Session{
		{ID: "reader-a", Project: "project-a", Machine: "local", Agent: "codex"},
		{ID: "reader-b", Project: "project-b", Machine: "local", Agent: "codex"},
	} {
		require.NoError(t, d.UpsertSession(t.Context(), session))
	}
	var preparations atomic.Int64
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		// SQLite calls the authorizer during SQL compilation.
		raw.(interface {
			RegisterAuthorizer(func(int, string, string, string) int)
		}).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_SELECT {
				preparations.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())

	ids := []string{"reader-a", "reader-b", "missing"}
	projects := map[string]string{"reader-a": "project-a", "reader-b": "project-b"}
	for i := range 22 {
		id := ids[i%len(ids)]
		for _, read := range []func(context.Context, string) (*Session, error){
			d.GetSession,
			d.GetSessionFull,
		} {
			session, err := read(t.Context(), id)
			require.NoError(t, err)
			if id == "missing" {
				require.Nil(t, session)
				continue
			}
			require.NotNil(t, session)
			assert.Equal(t, id, session.ID)
			assert.Equal(t, projects[id], session.Project)
		}
		if i == 1 {
			preparations.Store(0)
		}
	}
	assert.Zero(t, preparations.Load(), "later session reads must reuse their compiled SQL")
}

func TestArchiveReaderCacheSeesWritesAndReleasesSnapshot(t *testing.T) {
	d := testDB(t)
	session := Session{ID: "reader", Project: "before", Machine: "local", Agent: "codex"}
	require.NoError(t, d.UpsertSession(t.Context(), session))
	for range 3 {
		got, err := d.GetSession(t.Context(), session.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, "before", got.Project)
	}

	session.Project = "after"
	require.NoError(t, d.UpsertSession(t.Context(), session))
	got, err := d.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "after", got.Project)
	require.NoError(t, d.CheckpointWALTruncate(t.Context()), "idle compiled statements must release their read snapshot")
}

func TestArchiveReaderFirstStepAllowsReopen(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "reader", "project")
	entered := make(chan struct{})
	unblock := make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		workers.Wait()
	})
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(raw any) error {
		return raw.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("pause_first_step", func() int {
			close(entered)
			<-unblock
			return 1
		}, false)
	}))
	require.NoError(t, conn.Close())

	queryDone := make(chan error, 1)
	workers.Go(func() {
		var value int
		err := d.Reader().QueryRow(t.Context(), "SELECT pause_first_step()").Scan(&value)
		assert.Equal(t, 1, value)
		queryDone <- err
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "query did not start its first step")
	}

	reopenDone := make(chan error, 1)
	workers.Go(func() { reopenDone <- d.Reopen() })
	select {
	case err := <-reopenDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "query's first step blocked reader pool replacement")
	}
	session, err := d.GetSession(t.Context(), "reader")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "project", session.Project)
	release()
	require.NoError(t, <-queryDone)
}
