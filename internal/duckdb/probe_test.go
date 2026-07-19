//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeMirrorMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "none.duckdb")

	p, err := ProbeMirror(context.Background(), path)

	require.NoError(t, err)
	assert.False(t, p.FileExists)
	// Probing must not create the file.
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

func TestProbeMirrorReadsMetadataAndFlagsShapeIssues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.duckdb")
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, createSchema(context.Background(), conn))
	require.NoError(t, writeMirrorMetadata(context.Background(), conn, mirrorMetadata{
		SchemaVersion: SchemaVersion, DataVersion: 68, Scope: "",
		LastPushCutoff: "2026-07-18T00:00:00.000Z"}))
	require.NoError(t, conn.Close())

	p, err := ProbeMirror(context.Background(), path)
	require.NoError(t, err)
	assert.True(t, p.FileExists)
	assert.True(t, p.ShapeOK)
	assert.Equal(t, SchemaVersion, p.SchemaVersion)
	assert.Equal(t, 68, p.DataVersion)
	assert.Equal(t, "2026-07-18T00:00:00.000Z", p.LastPushCutoff)

	// NeedsRebuild triggers: version drift either direction, scope drift.
	assert.False(t, p.NeedsRebuild("", 68))
	assert.True(t, p.NeedsRebuild("", 69))
	assert.True(t, p.NeedsRebuild(canonicalPushScope([]string{"p"}, nil), 68))
	older := p
	older.SchemaVersion = 2
	assert.True(t, older.NeedsRebuild("", 68))
	newer := p
	newer.SchemaVersion = 4
	assert.True(t, newer.NeedsRebuild("", 68))
}

// TestProbeMirrorFlagsDroppedMetadataTableAsShapeIssue verifies that a
// mirror file missing the sync_metadata table entirely (as opposed to
// merely holding a stale or absent key) is flagged as a shape issue by the
// table/column shape check, not silently probed as schema/data version 0.
func TestProbeMirrorFlagsDroppedMetadataTableAsShapeIssue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dropped-metadata.duckdb")
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, createSchema(context.Background(), conn))
	_, err = conn.ExecContext(context.Background(), `DROP TABLE sync_metadata`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	p, err := ProbeMirror(context.Background(), path)

	require.NoError(t, err)
	assert.True(t, p.FileExists)
	assert.False(t, p.ShapeOK)
	assert.NotEmpty(t, p.ShapeIssue)
	assert.True(t, p.NeedsRebuild("", 68))
}

// TestProbeMirrorFlagsMalformedMetadataIntAsShapeIssue verifies that a
// non-integer value in an integer metadata field (as opposed to a merely
// missing key, which readMirrorMetadata tolerates as a zero value) is
// reported as a shape issue rather than a hard error.
func TestProbeMirrorFlagsMalformedMetadataIntAsShapeIssue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-int.duckdb")
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, createSchema(context.Background(), conn))
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO sync_metadata (key, value) VALUES (?, 'not-an-int')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		dataVersionMetadataKey,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	p, err := ProbeMirror(context.Background(), path)

	require.NoError(t, err)
	assert.True(t, p.FileExists)
	assert.False(t, p.ShapeOK)
	assert.NotEmpty(t, p.ShapeIssue)
	assert.True(t, p.NeedsRebuild("", 68))
}

// TestProbeMirrorToleratesMissingMetadataKeysAsZeroValues verifies that a
// freshly created mirror with no push-metadata rows yet (schema created but
// never pushed into) probes as shape-OK with zero-value fields, per
// readMirrorMetadata's documented tolerance for missing (as opposed to
// malformed) keys.
func TestProbeMirrorToleratesMissingMetadataKeysAsZeroValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-push-yet.duckdb")
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, createSchema(context.Background(), conn))
	_, err = conn.ExecContext(context.Background(),
		`DELETE FROM sync_metadata WHERE key = ?`, dataVersionMetadataKey,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	p, err := ProbeMirror(context.Background(), path)

	require.NoError(t, err)
	assert.True(t, p.FileExists)
	assert.True(t, p.ShapeOK)
	assert.Equal(t, 0, p.DataVersion)
	assert.True(t, p.NeedsRebuild("", 68), "zero data version must not match a real source version")
}

func TestCanonicalPushScopeIsDeterministicAndSorted(t *testing.T) {
	assert.Equal(t, "", canonicalPushScope(nil, nil))
	assert.Equal(t, "", canonicalPushScope([]string{}, []string{}))

	forward := canonicalPushScope([]string{"b", "a"}, []string{"y", "x"})
	reordered := canonicalPushScope([]string{"a", "b"}, []string{"x", "y"})
	assert.Equal(t, forward, reordered)
	assert.NotEmpty(t, forward)

	assert.NotEqual(t,
		canonicalPushScope([]string{"a"}, nil),
		canonicalPushScope([]string{"a", "b"}, nil),
	)
	assert.NotEqual(t,
		canonicalPushScope([]string{"a"}, nil),
		canonicalPushScope(nil, []string{"a"}),
	)
}

func TestProbeMirrorOpensReadOnlyAndNeverMutates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly.duckdb")
	conn, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, createSchema(context.Background(), conn))
	require.NoError(t, conn.Close())

	before, err := os.Stat(path)
	require.NoError(t, err)

	_, err = ProbeMirror(context.Background(), path)
	require.NoError(t, err)

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size(),
		"probing a mirror must not write to it")
}
