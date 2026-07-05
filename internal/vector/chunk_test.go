package vector

import (
	"context"
	"fmt"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forceIndexVarLimit pins ix's underlying vectors.db connection pool to a
// single connection and lowers its SQLITE_LIMIT_VARIABLE_NUMBER, mirroring
// internal/db's forceReaderVarLimit: some SQLite builds compile against the
// older documented 999-variable limit rather than the modern default
// (32766), and a test seeding a key count above maxSQLVars only genuinely
// regression-guards chunkKeys's chunking when the connection's real limit is
// low enough for that chunking to matter — otherwise a single unchunked IN
// (...) query would still succeed under the modern default and the test
// would pass even with chunking deleted.
func forceIndexVarLimit(t *testing.T, ix *Index, limit int) {
	t.Helper()
	ix.db.SetMaxOpenConns(1)
	ix.db.SetMaxIdleConns(1)
	conn, err := ix.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.Raw(func(dc any) error {
		sc, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("index conn is %T, want *sqlite3.SQLiteConn", dc)
		}
		sc.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, limit)
		return nil
	}))
}

// requireIndexVarLimitConstrained probes ix's connection with an
// over-the-limit IN (...) query, failing the test if it does not error --
// proof the lowered limit from forceIndexVarLimit is actually live, so a
// setup bug cannot silently mask the regression the caller checks next.
func requireIndexVarLimitConstrained(t *testing.T, ix *Index) {
	t.Helper()
	ctx := context.Background()
	overLimitPh, overLimitArgs := inPlaceholders(make([]string, 1001))
	_, probeErr := ix.db.QueryContext(ctx, "SELECT 1 WHERE '' IN "+overLimitPh, overLimitArgs...)
	require.Error(t, probeErr, "index variable limit was not constrained")
}

// TestChunkKeysSplitsAtMaxSQLVars asserts chunkKeys never hands fn more than
// maxSQLVars keys at a time, that every key is visited exactly once, and
// that a non-multiple-of-maxSQLVars input yields a shorter final chunk
// rather than an empty trailing one.
func TestChunkKeysSplitsAtMaxSQLVars(t *testing.T) {
	total := maxSQLVars*2 + 137
	keys := make([]string, total)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	var chunkSizes []int
	seen := make(map[string]int, total)
	err := chunkKeys(keys, func(chunk []string) error {
		chunkSizes = append(chunkSizes, len(chunk))
		for _, k := range chunk {
			seen[k]++
		}
		return nil
	})
	require.NoError(t, err)

	require.Len(t, chunkSizes, 3)
	assert.Equal(t, []int{maxSQLVars, maxSQLVars, 137}, chunkSizes)
	assert.Len(t, seen, total, "every key must be visited")
	for _, k := range keys {
		assert.Equal(t, 1, seen[k], "key %s must be visited exactly once", k)
	}
}

// TestChunkKeysEmptyInputInvokesNothing asserts an empty key slice never
// calls fn.
func TestChunkKeysEmptyInputInvokesNothing(t *testing.T) {
	calls := 0
	err := chunkKeys(nil, func([]string) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, calls)
}

// seedVectorMessages bulk-inserts n vector_messages rows with distinct
// doc_key/session_id/ordinal/content/content_hash values, inside one
// transaction so a large n (well past SQLite's 999-bind-variable limit)
// stays fast.
func seedVectorMessages(t *testing.T, ix *Index, n int) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := ix.db.BeginTx(ctx, nil)
	require.NoError(t, err)

	keys := make([]string, n)
	for i := range n {
		key := fmt.Sprintf("d%d", i)
		keys[i] = key
		_, err := tx.ExecContext(ctx, `
INSERT INTO vector_messages (doc_key, session_id, ordinal, content, content_hash)
VALUES (?, ?, ?, ?, ?)`,
			key, fmt.Sprintf("s%d", i), i, fmt.Sprintf("content %d", i), fmt.Sprintf("h%d", i))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	return keys
}

// TestLookupMirrorDocsOverMaxSQLVars asserts lookupMirrorDocs resolves every
// doc_key when the requested key count exceeds SQLite's 999-bind-variable
// limit (and this package's maxSQLVars chunk size), which a deep semantic
// overfetch (limit * over-fetch factor, in the low thousands) can trigger in
// a single Search call.
func TestLookupMirrorDocsOverMaxSQLVars(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	forceIndexVarLimit(t, ix, 999)
	requireIndexVarLimitConstrained(t, ix)

	n := maxSQLVars*3 + 42
	keys := seedVectorMessages(t, ix, n)

	docs, err := ix.lookupMirrorDocs(ctx, keys)
	require.NoError(t, err)

	require.Len(t, docs, n)
	for i, key := range keys {
		doc, ok := docs[key]
		require.True(t, ok, "doc_key %s missing from result", key)
		assert.Equal(t, fmt.Sprintf("s%d", i), doc.sessionID)
		assert.Equal(t, i, doc.ordinal)
		assert.Equal(t, fmt.Sprintf("content %d", i), doc.content)
	}
}

// TestLookupMirrorDocsMissingKeyOmittedNotZeroValued asserts a doc_key with
// no matching row is simply absent from the result map even when it is
// mixed into a chunk of thousands of keys that do resolve.
func TestLookupMirrorDocsMissingKeyOmittedNotZeroValued(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	keys := seedVectorMessages(t, ix, maxSQLVars+10)
	keys = append(keys, "does-not-exist")

	docs, err := ix.lookupMirrorDocs(ctx, keys)
	require.NoError(t, err)

	_, ok := docs["does-not-exist"]
	assert.False(t, ok)
	assert.Len(t, docs, maxSQLVars+10)
}

// TestCurrentOrdinalsOverMaxSQLVars asserts currentOrdinals resolves every
// key's ordinal when the key count exceeds SQLite's 999-bind-variable limit,
// which a pathological refresh with a large same-scan eviction batch could
// trigger.
func TestCurrentOrdinalsOverMaxSQLVars(t *testing.T) {
	ix := openTestIndex(t)
	ctx := context.Background()
	forceIndexVarLimit(t, ix, 999)
	requireIndexVarLimitConstrained(t, ix)

	n := maxSQLVars*3 + 42
	keys := seedVectorMessages(t, ix, n)

	ordinals, err := ix.currentOrdinals(ctx, keys)
	require.NoError(t, err)

	require.Len(t, ordinals, n)
	for i, key := range keys {
		assert.Equal(t, i, ordinals[key])
	}
}
