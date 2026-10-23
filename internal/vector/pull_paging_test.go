package vector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector/sqlitevec"
)

// Observe content rows crossing the SQL boundary, independently of the query's
// spelling. A batch must reach the hub before another page enters memory.
type pullContentConnector struct {
	drv   driver.Driver
	dsn   string
	reads atomic.Int64
}

func (c *pullContentConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &pullContentConn{Conn: conn, owner: c}, nil
}
func (c *pullContentConnector) Driver() driver.Driver { return c.drv }

type pullContentConn struct {
	driver.Conn
	owner *pullContentConnector
}

func (c *pullContentConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *pullContentConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &pullContentStmt{Stmt: stmt, owner: c.owner}, nil
}

type pullContentStmt struct {
	driver.Stmt
	owner *pullContentConnector
}

func (s *pullContentStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return &pullContentRows{Rows: rows, owner: s.owner, content: slices.Contains(rows.Columns(), "content")}, nil
}

type pullContentRows struct {
	driver.Rows
	owner   *pullContentConnector
	content bool
}

func (r *pullContentRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil && r.content {
		r.owner.reads.Add(1)
	}
	return err
}

func TestPullBoundsContentPagesAndAdvancesPastAbsentDocuments(t *testing.T) {
	for _, tc := range []struct{ count, pages, resumedPages int }{
		{255, 1, 1}, {256, 1, 1}, {257, 2, 1}, {512, 2, 2}, {513, 3, 2},
	} {
		t.Run(strconv.Itoa(tc.count), func(t *testing.T) { testPullBoundedPages(t, tc.count, tc.pages, tc.resumedPages) })
	}
}

func testPullBoundedPages(t *testing.T, count, pages, resumedPages int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(t.Context(), path, false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	src := &fakeUnitSource{}
	for i := range count {
		src.rows = append(src.rows, fakeUnit{unit: userDoc("s", fmt.Sprintf("u%04d", i), i, "document"), endedAt: "2024-01-01"})
	}
	_, err = ix.Refresh(t.Context(), src, true, false)
	require.NoError(t, err)
	oldDB := ix.db
	t.Cleanup(func() { require.NoError(t, oldDB.Close()) })
	observer := &pullContentConnector{drv: oldDB.Driver(), dsn: vectorDSN(path, false)}
	ix.db = sql.OpenDB(observer)
	ix.store, err = sqlitevec.New[string, string](t.Context(), ix.db, ix.spec.schema())
	require.NoError(t, err)
	gen := fakeGeneration("hub")
	calls := 0
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		calls++
		assert.LessOrEqual(t, observer.reads.Swap(0), int64(256), "pending content must be read one bounded page at a time")
		require.LessOrEqual(t, len(r.Identities), 256)
		reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint}
		for i, id := range r.Identities {
			if id.SourceUUID == fmt.Sprintf("u%04d", count-1) {
				reply.Chunks = append(reply.Chunks, HubChunk{Index: i, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"})
			} else {
				reply.Absent = append(reply.Absent, i)
			}
		}
		return reply, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	assert.False(t, result.Activated)
	assert.Equal(t, pages, calls, "absent pages must not starve the final available document")
	missing, err := ix.MissingEmbeddedDocs(t.Context(), gen.Fingerprint(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(count-1), missing)

	// A resumed pass skips the one completed document without trapping the cursor
	// on the still-absent first page.
	observer.reads.Store(0)
	calls = 0
	_, err = ix.Pull(t.Context(), src, hub, gen, PullOptions{})
	require.NoError(t, err)
	assert.Equal(t, resumedPages, calls)
}

func TestPullCompletesInterruptedInvalidRepair(t *testing.T) {
	ix := openTestIndex(t)
	src := &fakeUnitSource{rows: []fakeUnit{{unit: userDoc("s", "u", 0, "document"), endedAt: "2024-01-01"}}}
	gen := fakeGeneration("hub")
	_, err := ix.Build(t.Context(), src, fakeBuildEncoder(), gen, BuildOptions{})
	require.NoError(t, err)
	ordinal, err := ix.ordinalForFingerprint(t.Context(), gen.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(t.Context(), `UPDATE message_vectors_v`+fmtInt64(ordinal)+` SET embedding=?`, make([]byte, 12))
	require.NoError(t, err)
	stopped := errors.New("repair interrupted")
	_, err = ix.Build(t.Context(), src, func(context.Context, []string) ([][]float32, error) { return nil, stopped }, gen, BuildOptions{RepairInvalid: true})
	require.ErrorIs(t, err, stopped)
	_, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
	}), gen, PullOptions{})
	require.NoError(t, err)
	var encoded []string
	result, err := ix.Build(t.Context(), src, recordingEncoder(&encoded), gen, BuildOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Empty(t, encoded, "imported replacement vectors must complete durable repair work")
	assert.Zero(t, result.Repair.Documents)
}
