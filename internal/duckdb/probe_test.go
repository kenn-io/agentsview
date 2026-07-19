//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
		LastPushCutoff: "2026-07-18T00:00:00.000Z", LastPushMachine: "machine-a"}))
	require.NoError(t, conn.Close())

	p, err := ProbeMirror(context.Background(), path)
	require.NoError(t, err)
	assert.True(t, p.FileExists)
	assert.True(t, p.ShapeOK)
	assert.Equal(t, SchemaVersion, p.SchemaVersion)
	assert.Equal(t, 68, p.DataVersion)
	assert.Equal(t, "2026-07-18T00:00:00.000Z", p.LastPushCutoff)
	assert.Equal(t, "machine-a", p.LastPushMachine)

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

// TestProbeMirrorRecognitionRequiresSentinel pins RecognizedMirror to the
// agentsview sentinel (the agentsview_schema_version row in sync_metadata)
// rather than generic table names: a foreign DuckDB database that happens to
// carry a table named sessions or sync_metadata must never be recognized,
// because recognition authorizes a rebuild to atomically overwrite the file
// (see ensureReplaceableMirror). A real mirror keeps its sentinel — and its
// recognition — even when its shape is otherwise incompatible.
func TestProbeMirrorRecognitionRequiresSentinel(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name           string
		setup          func(t *testing.T, conn *sql.DB)
		wantRecognized bool
	}{
		{
			name: "generic sessions table only",
			setup: func(t *testing.T, conn *sql.DB) {
				_, err := conn.ExecContext(ctx, `CREATE TABLE sessions (id TEXT)`)
				require.NoError(t, err)
			},
			wantRecognized: false,
		},
		{
			name: "sync_metadata table without agentsview key",
			setup: func(t *testing.T, conn *sql.DB) {
				_, err := conn.ExecContext(ctx,
					`CREATE TABLE sync_metadata (key TEXT PRIMARY KEY, value TEXT)`)
				require.NoError(t, err)
				_, err = conn.ExecContext(ctx,
					`INSERT INTO sync_metadata (key, value) VALUES ('other_tool', '1')`)
				require.NoError(t, err)
			},
			wantRecognized: false,
		},
		{
			name: "sync_metadata without key/value columns",
			setup: func(t *testing.T, conn *sql.DB) {
				_, err := conn.ExecContext(ctx,
					`CREATE TABLE sync_metadata (id INTEGER)`)
				require.NoError(t, err)
			},
			wantRecognized: false,
		},
		{
			name: "sentinel present with incompatible shape",
			setup: func(t *testing.T, conn *sql.DB) {
				require.NoError(t, createSchema(ctx, conn))
				_, err := conn.ExecContext(ctx, `DROP TABLE messages`)
				require.NoError(t, err)
			},
			wantRecognized: true,
		},
		{
			name: "sentinel present with old schema version",
			setup: func(t *testing.T, conn *sql.DB) {
				require.NoError(t, createSchema(ctx, conn))
				_, err := conn.ExecContext(ctx, `
					INSERT INTO sync_metadata (key, value) VALUES (?, '1')
					ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
					schemaVersionMetadataKey)
				require.NoError(t, err)
			},
			wantRecognized: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recognition.duckdb")
			conn, err := Open(path)
			require.NoError(t, err)
			tt.setup(t, conn)
			require.NoError(t, conn.Close())

			p, err := ProbeMirror(ctx, path)

			require.NoError(t, err)
			assert.True(t, p.FileExists)
			assert.Equal(t, tt.wantRecognized, p.RecognizedMirror)
		})
	}
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

// TestIsMirrorLockConflictErrorClassifiesLockMessages tests the pure
// error-string classifier in isolation, using fabricated errors: an actual
// cross-process DuckDB lock conflict is hard to reproduce in-process (the
// duckdb-go driver shares an instance cache per path within one process, so
// a second in-process open of the same path does not race a lock the way a
// second OS process would), so the classifier itself is what gets tested
// directly against representative error strings instead.
func TestIsMirrorLockConflictErrorClassifiesLockMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			name: "could not set lock",
			err: errors.New(
				"IO Error: Could not set lock on file " +
					`"/tmp/agentsview.duckdb": Conflicting lock is held in ` +
					`process 12345`,
			),
			want: true,
		},
		{
			name: "conflicting lock only",
			err:  errors.New("Conflicting lock is held"),
			want: true,
		},
		{"unrelated io error", errors.New("no such file or directory"), false},
		{"malformed database", errors.New("file is not a valid DuckDB database"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isMirrorLockConflictError(tt.err))
		})
	}
}

func TestProbeMirrorReportsLockConflictDistinctFromOtherShapeIssues(t *testing.T) {
	probe := MirrorProbe{
		FileExists:   true,
		ShapeIssue:   "Could not set lock on file",
		LockConflict: true,
	}
	assert.True(t, probe.NeedsRebuild("", 1),
		"a lock conflict still forces a rebuild, same as any other shape issue")
}

func TestRebuildReasonReportsEachTrigger(t *testing.T) {
	baseProbe := func() MirrorProbe {
		return MirrorProbe{
			FileExists: true, ShapeOK: true,
			SchemaVersion: SchemaVersion, DataVersion: 1, Scope: "",
		}
	}
	tests := []struct {
		name    string
		probe   MirrorProbe
		scope   string
		dataVer int
		full    bool
		localDR int64
		machine string
		want    string
	}{
		{
			name: "missing file", probe: MirrorProbe{}, dataVer: 1,
			want: "missing file",
		},
		{
			name: "full requested", probe: baseProbe(), dataVer: 1, full: true,
			want: "--full requested",
		},
		{
			name: "schema version mismatch",
			probe: func() MirrorProbe {
				p := baseProbe()
				p.SchemaVersion = SchemaVersion - 1
				return p
			}(),
			dataVer: 1,
			want: fmt.Sprintf(
				"schema version %d vs %d", SchemaVersion-1, SchemaVersion,
			),
		},
		{
			name: "data version mismatch", probe: baseProbe(), dataVer: 2,
			want: "data version 1 vs 2",
		},
		{
			name: "scope changed", probe: baseProbe(), scope: "other", dataVer: 1,
			want: "scope changed",
		},
		{
			name: "lock conflict",
			probe: MirrorProbe{
				FileExists: true, ShapeIssue: "Could not set lock",
				LockConflict: true,
			},
			dataVer: 1,
			want:    "mirror locked by another process — likely a running serve; rebuilding from scratch because incremental update cannot proceed while it is served",
		},
		{
			name: "machine name changed",
			probe: func() MirrorProbe {
				p := baseProbe()
				p.LastPushMachine = "machine-a"
				return p
			}(),
			dataVer: 1, machine: "machine-b",
			want: "machine name changed from machine-a to machine-b",
		},
		{
			name: "no prior recorded machine does not force a rebuild",
			probe: func() MirrorProbe {
				p := baseProbe()
				p.LastPushMachine = ""
				return p
			}(),
			dataVer: 1, machine: "machine-b",
			want: "",
		},
		{
			name: "deletion cursor ahead of local archive",
			probe: func() MirrorProbe {
				p := baseProbe()
				p.DeletionRevision = 5
				return p
			}(),
			dataVer: 1, localDR: 2,
			want: "mirror deletion cursor ahead of archive; archive was rebuilt",
		},
		{
			name: "no rebuild needed", probe: baseProbe(), dataVer: 1,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rebuildReason(
				tt.probe, tt.scope, tt.dataVer, tt.full, tt.localDR, tt.machine,
			)
			assert.Equal(t, tt.want, got)
		})
	}
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

// TestClassifyProbeErrorClassifiesLockConflicts tests the pure
// shapeIssue/lockConflict classifier in isolation against fabricated
// errors, mirroring TestIsMirrorLockConflictErrorClassifiesLockMessages but
// asserting classifyProbeError's full (string, bool) return pair, which is
// what probeOpenMirror's mirrorShapeIssue/readMirrorMetadata error branches
// now route through (see TestProbeMirrorRoutesLazyOpenLockConflictThroughClassifier
// for the code-path routing itself).
func TestClassifyProbeErrorClassifiesLockConflicts(t *testing.T) {
	tests := []struct {
		name             string
		err              error
		wantShapeIssue   string
		wantLockConflict bool
	}{
		{"nil error", nil, "", false},
		{
			name: "could not set lock",
			err: errors.New(
				"IO Error: Could not set lock on file " +
					`"/tmp/agentsview.duckdb": Conflicting lock is held in ` +
					`process 12345`,
			),
			wantShapeIssue: "IO Error: Could not set lock on file " +
				`"/tmp/agentsview.duckdb": Conflicting lock is held in ` +
				`process 12345`,
			wantLockConflict: true,
		},
		{
			name:             "unrelated error preserves message but is not a lock conflict",
			err:              errors.New("no such file or directory"),
			wantShapeIssue:   "no such file or directory",
			wantLockConflict: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shapeIssue, lockConflict := classifyProbeError(tt.err)
			assert.Equal(t, tt.wantShapeIssue, shapeIssue)
			assert.Equal(t, tt.wantLockConflict, lockConflict)
		})
	}
}

// lockConflictProbeDriver and lockConflictProbeConn simulate the lazy-open
// lock conflict duckdb-go can surface: Open succeeds (as it does for a lazy
// driver), but the first real query fails with a DuckDB lock-conflict
// message. This is used to prove ProbeMirror routes that failure through
// classifyProbeError from inside mirrorShapeIssue's query (loadColumns),
// not just that the classifier is correct in isolation.
type lockConflictProbeDriver struct{}

type lockConflictProbeConn struct{}

var lockConflictProbeRegisterOnce sync.Once

func (lockConflictProbeDriver) Open(string) (driver.Conn, error) {
	return lockConflictProbeConn{}, nil
}

func (lockConflictProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (lockConflictProbeConn) Close() error { return nil }

func (lockConflictProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (lockConflictProbeConn) QueryContext(
	context.Context, string, []driver.NamedValue,
) (driver.Rows, error) {
	return nil, errors.New(
		`IO Error: Could not set lock on file "mirror.duckdb": ` +
			"Conflicting lock is held in process 999",
	)
}

// TestProbeMirrorRoutesLazyOpenLockConflictThroughClassifier is the FIX3
// regression: duckdb-go opens connections lazily, so a cross-process lock
// conflict often does not surface from openReadOnlyMirror's Open call at
// all but only once the first real query runs — inside mirrorShapeIssue's
// loadColumns call, in production. Before routing that error through
// classifyProbeError, probeOpenMirror only ever set LockConflict from the
// openReadOnlyMirror error path, so a lock conflict surfacing here would
// silently degrade to a generic, unclassified shape issue instead.
func TestProbeMirrorRoutesLazyOpenLockConflictThroughClassifier(t *testing.T) {
	lockConflictProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_lock_conflict_probe", lockConflictProbeDriver{})
	})
	conn, err := sql.Open("agentsview_lock_conflict_probe", t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	probe := probeOpenMirror(context.Background(), conn)

	assert.True(t, probe.FileExists)
	assert.False(t, probe.ShapeOK)
	assert.True(t, probe.LockConflict,
		"a lock conflict surfacing from the first lazy query must still be classified")
	assert.Contains(t, probe.ShapeIssue, "Could not set lock")
}
