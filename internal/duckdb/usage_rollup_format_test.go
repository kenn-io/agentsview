//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/storage"
)

// A rollup in a mirror stays valid across releases until usageRollupFormat
// changes, so any change to what a build stores must bump the format. The
// test hashes every table a build writes for the fixture, except the
// session fingerprints, which belong to push, and requires one record per
// format in testdata/usage-rollup-format-digests.
func TestUsageRollupFormat(t *testing.T) {
	ctx := t.Context()
	syncer := newInMemoryTestSync(t, rollupFixture(t), storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	_, err = syncer.buildUsageRollup(ctx)
	require.NoError(t, err)

	scan := func(query string, each func(*sql.Rows)) {
		rows, err := syncer.DB().QueryContext(ctx, query)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			each(rows)
		}
		require.NoError(t, rows.Err())
	}
	var tables [][][]string
	for _, table := range []string{
		"usage_rollup", "usage_rollup_pricing", "usage_rollup_exceptions", "usage_rollup_identities",
	} {
		var values [][]string
		scan(`SELECT COLUMNS(*)::VARCHAR FROM `+table+` ORDER BY ALL`, func(rows *sql.Rows) {
			columns, err := rows.Columns()
			require.NoError(t, err)
			row := make([]*string, len(columns))
			dest := make([]any, len(columns))
			for i := range row {
				dest[i] = &row[i]
			}
			require.NoError(t, rows.Scan(dest...))
			text := make([]string, len(row))
			for i, v := range row {
				if v != nil {
					text[i] = "=" + *v
				}
			}
			values = append(values, text)
		})
		require.NotEmpty(t, values, table)
		tables = append(tables, values)
	}
	// Gob encodes the records' maps in random order, so hash what they decode
	// to.
	var records []any
	scan(`SELECT record_id, record FROM usage_rollup_records ORDER BY record_id`, func(rows *sql.Rows) {
		var id int64
		var data []byte
		require.NoError(t, rows.Scan(&id, &data))
		var rec usageRollupRecord
		require.NoError(t, gob.NewDecoder(bytes.NewReader(data)).Decode(&rec))
		// The fixture's price rows carry the time the test wrote them.
		rec.Lookup.Rates.UpdatedAt = nil
		records = append(records, []any{id, rec})
	})
	require.NotEmpty(t, records)
	encoded, err := json.Marshal([]any{tables, records})
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])

	dir := filepath.Join("testdata", "usage-rollup-format-digests")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var current []string
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry.Name(), "-")
		n, err := strconv.Atoi(prefix)
		require.NoError(t, err, entry.Name())
		require.LessOrEqual(t, n, usageRollupFormat,
			"usage rollup format %d has a newer record at format %d; bump to format %d", usageRollupFormat, n, n+1)
		if n == usageRollupFormat {
			current = append(current, entry.Name())
		}
	}
	want := fmt.Sprintf("%d-%s.txt", usageRollupFormat, digest)
	require.Equal(t, []string{want}, current,
		"usage rollup format %d output changed (digest %s); if this branch added the record, replace it; "+
			"otherwise bump usageRollupFormat and record the new digest", usageRollupFormat, digest)
}
