package sync

import (
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Codex rollouts read from S3 record their rate-limit reports under the
// session ID namespaced by the source machine.
func TestProcessS3CodexRecordsRateLimits(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/codex/2026/11/01/rollout-2026-11-01T12-00-00-abc.jsonl"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc", "/repo", "codex_cli_rs", "2026-11-01T12:00:00Z"),
		testjsonl.CodexMsgJSON("user", "Hello", "2026-11-01T12:00:01Z"),
		`{"type":"event_msg","timestamp":"2026-11-01T12:00:02Z","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":7,"window_minutes":300,"resets_at":1793500000},"secondary":null,"plan_type":"plus"}}}`,
	)
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		if got != path {
			return nil, missingS3ObjectError()
		}
		return io.NopCloser(strings.NewReader(content)), nil
	}

	e := &Engine{db: database, machine: "central"}
	res := e.processFile(t.Context(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	require.NoError(t, res.err)
	t.Cleanup(func() {
		res.releaseStaged()
		res.retentionLease.Release()
	})

	latest, err := database.GetLatestRateLimits(t.Context())
	require.NoError(t, err)
	require.Len(t, latest, 1)
	assert.Equal(t, "laptop~codex:abc", latest[0].SessionID)
	assert.Equal(t, "plus", latest[0].PlanType)
}

// A failed report write after a staged S3 parse frees the staging sink and
// restores the GC setting instead of leaking them on every retry.
func TestProcessS3CodexRateLimitFailureReleasesStaging(t *testing.T) {
	database := openTestDB(t)
	path := "s3://bucket/laptop/raw/codex/2026/11/01/rollout-2026-11-01T12-00-00-def.jsonl"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("def", "/repo", "codex_cli_rs", "2026-11-01T12:00:00Z"),
		testjsonl.CodexMsgJSON("user", "Hello", "2026-11-01T12:00:01Z"),
		`{"type":"event_msg","timestamp":"2026-11-01T12:00:02Z","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":7,"window_minutes":300,"resets_at":1793500000},"secondary":null,"plan_type":"plus"}}}`,
	)
	oldFetch := fetchS3Object
	t.Cleanup(func() { fetchS3Object = oldFetch })
	fetchS3Object = func(got string) (io.ReadCloser, error) {
		if got != path {
			return nil, missingS3ObjectError()
		}
		return io.NopCloser(strings.NewReader(content)), nil
	}
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER fail_rate_limits BEFORE INSERT ON rate_limit_snapshots
			BEGIN SELECT RAISE(ABORT, 'database is busy'); END`)
		return err
	}))
	stagingDir := t.TempDir()
	stagedGCMu.Lock()
	refsBefore := stagedGCRefs
	stagedGCMu.Unlock()

	e := &Engine{db: database, machine: "central", stagedCodexDir: stagingDir}
	res := e.processFile(t.Context(), parser.DiscoveredFile{
		Agent:       parser.AgentCodex,
		Path:        path,
		Machine:     "laptop",
		SourceSize:  int64(len(content)),
		SourceMtime: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
	})
	require.Error(t, res.err)
	res.retentionLease.Release()

	assert.Nil(t, res.staged)
	stagedGCMu.Lock()
	refs := stagedGCRefs
	stagedGCMu.Unlock()
	assert.Equal(t, refsBefore, refs, "the lowered GC window is released")
	entries, err := os.ReadDir(stagingDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the scratch staging database is removed")
}
