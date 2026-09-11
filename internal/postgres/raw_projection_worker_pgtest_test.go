//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type projectionQueueObserver struct {
	rawderive.JobQueue
	retries, failures atomic.Int32
}

func (q *projectionQueueObserver) RetryRawParseJob(context.Context, rawderive.JobLease, time.Time, string, string) error {
	q.retries.Add(1)
	return fmt.Errorf("unexpected second retry write")
}
func (q *projectionQueueObserver) FailRawParseJob(context.Context, rawderive.JobLease, string, string) error {
	q.failures.Add(1)
	return fmt.Errorf("unexpected second failure write")
}

type projectionPartialParser struct {
	body string
	root string
}

func (p *projectionPartialParser) Parse(_ context.Context, m rawsync.CanonicalManifest, tree *rawderive.Materialization) (rawderive.ParsedManifest, error) {
	p.root = tree.Root()
	body, err := os.ReadFile(filepath.Join(tree.Root(), m.Manifest.Entries[0].Path))
	if err != nil {
		return rawderive.ParsedManifest{}, err
	}
	if string(body) != p.body {
		return rawderive.ParsedManifest{}, fmt.Errorf("wrong custody bytes")
	}
	out := projectionOutcome("proven member")
	out.Outcome.ResultSetComplete = false
	return out, nil
}
func TestRawProjectionWorkerReportsCommittedPartialOutcomes(t *testing.T) {
	for _, maximum := range []int{1, 3} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			f := newProjectionFixture(t)
			m, _ := f.accept(t, "device-a", "partial-worker", "")
			policy := rawderive.RetryPolicy{Base: 2 * time.Second, Maximum: time.Minute, MaxAttempts: maximum}
			sink, err := NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant, RetryPolicy: policy})
			require.NoError(t, err)
			_, err = sink.SelectSourceGeneration(t.Context(), m, "parser-1")
			require.NoError(t, err)
			queue := &projectionQueueObserver{JobQueue: f.jobs}
			parser := &projectionPartialParser{body: "synthetic custody partial-worker"}
			worker, err := rawderive.NewWorker(rawderive.WorkerConfig{Queue: queue, Manifests: rawderive.ManifestLoader{Store: f.objects, Limits: rawsync.DefaultManifestLimits()}, Materializer: &rawderive.Materializer{Store: f.objects, BaseDir: t.TempDir(), MaxTotalBytes: 4096}, Parser: parser, Projection: sink, Owner: "worker", BatchSize: 1, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, AttemptTimeout: 30 * time.Second, RetryBase: policy.Base, RetryMax: policy.Maximum, MaxAttempts: policy.MaxAttempts})
			require.NoError(t, err)
			result, err := worker.RunBatch(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, result.Claimed)
			assert.Zero(t, result.Succeeded)
			assert.Zero(t, result.LeaseLost)
			assert.Zero(t, queue.retries.Load())
			assert.Zero(t, queue.failures.Load())
			var state string
			var delay float64
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,extract(epoch FROM available_at-clock_timestamp()) FROM raw_ingest_jobs WHERE manifest_id=$1`, m.ManifestID).Scan(&state, &delay))
			if maximum == 1 {
				assert.Equal(t, 1, result.Failed)
				assert.Zero(t, result.Retried)
				assert.Equal(t, "failed", state)
			} else {
				assert.Equal(t, 1, result.Retried)
				assert.Zero(t, result.Failed)
				assert.Equal(t, "retrying", state)
				assert.Greater(t, delay, 1.0)
				assert.LessOrEqual(t, delay, 2.0)
			}
			r, err := sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, RawIdentityUnique, r.State)
			_, err = os.Stat(parser.root)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
