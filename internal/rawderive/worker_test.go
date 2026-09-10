package rawderive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestWorkerRunsSnapshotPipelineAndCleansBeforeAtomicProjection(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	var materializedRoot string
	var projected bool
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			root, err := os.MkdirTemp(t.TempDir(), "worker-materialized-")
			if err != nil {
				return nil, err
			}
			materializedRoot = root
			return &Materialization{root: root, entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(
			_ context.Context,
			gotManifest rawsync.CanonicalManifest,
			materialized *Materialization,
		) (ParsedManifest, error) {
			assert.Equal(t, manifest, gotManifest)
			_, err := os.Stat(materialized.Root())
			if err != nil {
				return ParsedManifest{}, err
			}
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}, nil
		}),
		projectionSinkFunc(func(
			_ context.Context,
			gotLease JobLease,
			gotManifest rawsync.CanonicalManifest,
			gotParsed ParsedManifest,
		) error {
			assert.Equal(t, lease, gotLease)
			assert.Equal(t, manifest, gotManifest)
			assert.True(t, gotParsed.Outcome.ResultSetComplete)
			_, err := os.Stat(materializedRoot)
			assert.ErrorIs(t, err, os.ErrNotExist, "raw bytes must be gone before projection")
			projected = true
			return nil
		}),
	)

	result, err := worker.RunBatch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, BatchResult{Claimed: 1, Succeeded: 1}, result)
	assert.True(t, projected)
	assert.Empty(t, queue.retries)
}

func TestWorkerClassifiesParseFailureAndRetriesAfterCleanup(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	parseFailure := errors.New("malformed provider source")
	var materializedRoot string
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			root, err := os.MkdirTemp(t.TempDir(), "worker-materialized-")
			if err != nil {
				return nil, err
			}
			materializedRoot = root
			return &Materialization{root: root, entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, parseFailure
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after parse failure")
		}),
	)
	started := time.Now()

	result, err := worker.RunBatch(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse:internal",
		"the returned error carries the allowlisted diagnostic instead of raw provider text")
	assert.NotContains(t, err.Error(), parseFailure.Error())
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "parse", queue.retries[0].class)
	assert.Equal(t, "parse:internal", queue.retries[0].message)
	assert.WithinDuration(t, started.Add(time.Minute), queue.retries[0].availableAt, time.Second)
	_, statErr := os.Stat(materializedRoot)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWorkerMovesTerminalAttemptToDurableFailure(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	lease.Attempt = 3
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	parseFailure := errors.New("unsupported provider format")
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, parseFailure
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after parse failure")
		}),
	)

	result, err := worker.RunBatch(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse:internal")
	assert.NotContains(t, err.Error(), parseFailure.Error())
	assert.Equal(t, BatchResult{Claimed: 1, Failed: 1}, result)
	assert.Empty(t, queue.retries)
	require.Len(t, queue.failures, 1)
	assert.Equal(t, lease, queue.failures[0].lease)
	assert.Equal(t, "parse", queue.failures[0].class)
	assert.Equal(t, "parse:internal", queue.failures[0].message)
}

func TestWorkerCancelsWorkAndDoesNotRetryAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{
		leases:       []JobLease{lease},
		heartbeatErr: ErrLeaseLost,
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return ParsedManifest{}, ctx.Err()
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after lease loss")
		}),
	)
	worker.HeartbeatInterval = time.Millisecond

	result, err := worker.RunBatch(t.Context())
	require.ErrorIs(t, err, ErrLeaseLost)
	assert.Equal(t, BatchResult{Claimed: 1, LeaseLost: 1}, result)
	assert.Empty(t, queue.retries)
	assert.NotZero(t, queue.heartbeats.Load())
}

func TestWorkerClassifiesHeartbeatTransportCancellation(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	heartbeatFailure := errors.New("heartbeat transport unavailable")
	heartbeatObserved := make(chan struct{})
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	queue.heartbeat = func(context.Context, JobLease, time.Duration) error {
		close(heartbeatObserved)
		return heartbeatFailure
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			select {
			case <-heartbeatObserved:
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return ParsedManifest{}, ctx.Err()
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after heartbeat failure")
		}),
	)
	worker.HeartbeatInterval = time.Millisecond

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err)
	assert.NotErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), heartbeatFailure.Error())
	assert.Contains(t, err.Error(), "heartbeat:internal")
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "heartbeat", queue.retries[0].class)
	assert.Equal(t, "heartbeat:internal", queue.retries[0].message)
}

func TestWorkerTreatsPostProjectionLeaseReleaseAsSuccess(t *testing.T) {
	t.Parallel()
	testWorkerTreatsPostProjectionHeartbeatFailureAsSuccess(t, ErrLeaseLost)
}

func TestWorkerTreatsPostProjectionHeartbeatTransportFailureAsSuccess(t *testing.T) {
	t.Parallel()
	testWorkerTreatsPostProjectionHeartbeatFailureAsSuccess(
		t, errors.New("heartbeat transport unavailable"),
	)
}

func testWorkerTreatsPostProjectionHeartbeatFailureAsSuccess(
	t *testing.T,
	heartbeatErr error,
) {
	t.Helper()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestTombstone)
	lease.ManifestID = manifest.ManifestID
	projectionCommitted := make(chan struct{})
	heartbeatFinished := make(chan struct{})
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	queue.heartbeat = func(context.Context, JobLease, time.Duration) error {
		select {
		case <-projectionCommitted:
		case <-time.After(5 * time.Second):
		}
		close(heartbeatFinished)
		return heartbeatErr
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return nil, errors.New("unexpected tombstone materialization")
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Tombstone: true}, nil
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			close(projectionCommitted)
			select {
			case <-heartbeatFinished:
			case <-time.After(5 * time.Second):
			}
			return nil
		}),
	)
	worker.HeartbeatInterval = time.Millisecond

	result, err := worker.RunBatch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, BatchResult{Claimed: 1, Succeeded: 1}, result)
	assert.NotZero(t, queue.heartbeats.Load(),
		"the heartbeat must have run and failed before the outcome was classified")
	assert.Empty(t, queue.retries)
	assert.Empty(t, queue.failures)
}

func TestWorkerProjectsTombstoneWithoutMaterialization(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestTombstone)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return nil, errors.New("unexpected tombstone materialization")
		}),
		sourceParserFunc(func(
			_ context.Context,
			_ rawsync.CanonicalManifest,
			materialized *Materialization,
		) (ParsedManifest, error) {
			assert.Nil(t, materialized)
			return ParsedManifest{Tombstone: true}, nil
		}),
		projectionSinkFunc(func(
			_ context.Context,
			_ JobLease,
			_ rawsync.CanonicalManifest,
			parsed ParsedManifest,
		) error {
			assert.True(t, parsed.Tombstone)
			return nil
		}),
	)

	result, err := worker.RunBatch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, BatchResult{Claimed: 1, Succeeded: 1}, result)
}

func TestNewWorkerBoundsClaimBatchSize(t *testing.T) {
	t.Parallel()
	newConfig := func(batchSize int) WorkerConfig {
		return WorkerConfig{
			Queue: &workerQueueFixture{},
			Manifests: manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
				panic("must not run")
			}),
			Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
				panic("must not run")
			}),
			Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
				panic("must not run")
			}),
			Projection: projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
				panic("must not run")
			}),
			Owner:             "worker-a",
			BatchSize:         batchSize,
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 10 * time.Second,
			RetryBase:         time.Minute,
			RetryMax:          time.Hour,
			MaxAttempts:       3,
			AttemptTimeout:    5 * time.Minute,
		}
	}

	worker, err := NewWorker(newConfig(MaxClaimBatchSize))
	require.NoError(t, err)
	assert.Equal(t, MaxClaimBatchSize, worker.BatchSize)

	_, err = NewWorker(newConfig(MaxClaimBatchSize + 1))
	assert.ErrorIs(t, err, rawsync.ErrInvalid,
		"the worker must reject a claim batch the shared queue contract refuses")
}

func TestNewWorkerSharesQueueOwnerContract(t *testing.T) {
	t.Parallel()
	newConfig := func(owner string) WorkerConfig {
		return WorkerConfig{
			Queue: &workerQueueFixture{},
			Manifests: manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
				panic("must not run")
			}),
			Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
				panic("must not run")
			}),
			Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
				panic("must not run")
			}),
			Projection: projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
				panic("must not run")
			}),
			Owner:             owner,
			BatchSize:         1,
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 10 * time.Second,
			RetryBase:         time.Minute,
			RetryMax:          time.Hour,
			MaxAttempts:       3,
			AttemptTimeout:    5 * time.Minute,
		}
	}

	_, err := NewWorker(newConfig(strings.Repeat("a", MaxLeaseOwnerBytes)))
	require.NoError(t, err)

	_, err = NewWorker(newConfig(strings.Repeat("a", MaxLeaseOwnerBytes+1)))
	assert.ErrorIs(t, err, rawsync.ErrInvalid)

	_, err = NewWorker(newConfig(string([]byte{0xff})))
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
}

func TestNewWorkerRequiresValidatedAttemptTimeout(t *testing.T) {
	t.Parallel()
	config := WorkerConfig{
		Queue: &workerQueueFixture{},
		Manifests: manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			panic("must not run")
		}),
		Materializer: sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			panic("must not run")
		}),
		Parser: sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			panic("must not run")
		}),
		Projection: projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			panic("must not run")
		}),
		Owner:             "worker-a",
		BatchSize:         2,
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 10 * time.Second,
		RetryBase:         time.Minute,
		RetryMax:          time.Hour,
		MaxAttempts:       3,
	}

	_, err := NewWorker(config)
	assert.ErrorIs(t, err, rawsync.ErrInvalid,
		"a worker without a per-attempt timeout must be rejected")

	config.AttemptTimeout = -time.Second
	_, err = NewWorker(config)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)

	config.AttemptTimeout = 90 * time.Second
	worker, err := NewWorker(config)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, worker.AttemptTimeout)
}

func TestWorkerAttemptTimeoutStopsRunawayPipelineAndRecordsDeadline(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	parseStopped := make(chan struct{})
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			defer close(parseStopped)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return ParsedManifest{}, ctx.Err()
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after attempt deadline")
		}),
	)
	worker.AttemptTimeout = 50 * time.Millisecond
	worker.HeartbeatInterval = 10 * time.Millisecond
	started := time.Now()

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err)
	assert.Less(t, time.Since(started), 5*time.Second,
		"a cooperative pipeline must observe the per-attempt deadline, not outlive the lease")
	select {
	case <-parseStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the parse pipeline never observed the attempt deadline")
	}
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "parse", queue.retries[0].class)
	assert.Contains(t, queue.retries[0].message, "deadline")
	assert.WithinDuration(t, started.Add(time.Minute), queue.retries[0].availableAt, 2*time.Second)
}

func TestWorkerStopsRenewingLeaseWhenStageBlocksPastAttemptDeadline(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing assertion must not strand the batch goroutine on the
	// non-cooperative stage's release channel.
	t.Cleanup(releaseBlocked)
	heartbeatStarted := make(chan struct{})
	deadlineObserved := make(chan struct{})
	var heartbeatOnce sync.Once
	queue.heartbeat = func(context.Context, JobLease, time.Duration) error {
		heartbeatOnce.Do(func() { close(heartbeatStarted) })
		return nil
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			// A non-cooperative stage: it never observes its context, so
			// neither the attempt deadline nor cancellation can end it and
			// only the test's release channel unblocks it.
			go func() {
				select {
				case <-ctx.Done():
					close(deadlineObserved)
				case <-time.After(5 * time.Second):
				}
			}()
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return ParsedManifest{}, nil
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return nil
		}),
	)
	worker.AttemptTimeout = 250 * time.Millisecond
	worker.HeartbeatInterval = 10 * time.Millisecond
	batchDone := make(chan BatchResult, 1)
	batchErr := make(chan error, 1)
	go func() {
		result, err := worker.RunBatch(t.Context())
		batchDone <- result
		batchErr <- err
	}()

	select {
	case <-heartbeatStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease never began renewing before the attempt deadline")
	}
	select {
	case <-deadlineObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked stage never observed its attempt deadline")
	}
	frozen := queue.heartbeats.Load()
	time.Sleep(8 * worker.HeartbeatInterval)
	still := queue.heartbeats.Load()
	require.Equal(t, frozen, still,
		"the attempt deadline must stop lease renewal; a stage that blocks past it cannot keep the lease alive")

	releaseBlocked()
	select {
	case result := <-batchDone:
		require.NoError(t, <-batchErr,
			"a stage that finishes after the deadline keeps its fenced outcome")
		assert.Equal(t, BatchResult{Claimed: 1, Succeeded: 1}, result)
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the blocked stage must let the batch finish")
	}
	assert.Equal(t, still, queue.heartbeats.Load(),
		"no renewal may happen after the deadline fires or after the stage finally returns")
}

func TestWorkerJoinsCleanupFailureWithParseFailure(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	parseFailure := errors.New("malformed provider source")
	cleanupFailure := errors.New("scratch cleanup failed")
	materialized := &Materialization{
		root:    t.TempDir(),
		entries: map[string]string{},
		removeTree: func(string) error {
			return cleanupFailure
		},
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return materialized, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, parseFailure
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after parse failure")
		}),
	)

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cleanup_failed",
		"a cleanup failure must not be swallowed when parsing already failed")
	assert.NotContains(t, err.Error(), materialized.Root(),
		"the returned error must not expose the raw tree path")
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "parse", queue.retries[0].class,
		"the parse failure stays the durable stage")
}

func TestWorkerReportsPreProjectionCleanupFailureOnceWhenRetryFailsToo(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	cleanupFailure := errors.New("scratch cleanup failed")
	var attempts atomic.Int64
	materialized := &Materialization{
		root:    t.TempDir(),
		entries: map[string]string{},
		removeTree: func(string) error {
			attempts.Add(1)
			return cleanupFailure
		},
	}
	worker := newWorkerForTest(t, &workerQueueFixture{},
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return materialized, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}, nil
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection before materialization cleanup")
		}),
	)

	stage, pipelineErr := worker.runPipeline(t.Context(), lease)

	require.Error(t, pipelineErr)
	assert.Equal(t, "materialize", stage,
		"the pre-projection cleanup failure owns the durable stage")
	assert.ErrorIs(t, pipelineErr, errMaterializationCleanup)
	assert.Equal(t, 1, strings.Count(pipelineErr.Error(), cleanupFailure.Error()),
		"a failed cleanup retry must not re-report the already reported cleanup failure")
	assert.Equal(t, int64(2), attempts.Load(),
		"the deferred cleanup must retry the failed removal exactly once")
}

func TestWorkerRetriesJobWhenPreProjectionCleanupRetrySucceeds(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	cleanupFailure := errors.New("scratch cleanup failed")
	var attempts atomic.Int64
	materialized := &Materialization{
		root:    t.TempDir(),
		entries: map[string]string{},
		removeTree: func(path string) error {
			if attempts.Add(1) == 1 {
				return cleanupFailure
			}
			return os.RemoveAll(path)
		},
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return materialized, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}, nil
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after materialization cleanup failure")
		}),
	)

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err,
		"a first cleanup failure must still fail the attempt even when the retry succeeds")
	assert.Contains(t, err.Error(), "materialize:internal+cleanup_failed")
	assert.Equal(t, 1, strings.Count(err.Error(), "(materialization cleanup failed)"))
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "materialize", queue.retries[0].class)
	assert.Equal(t, "materialize:internal+cleanup_failed", queue.retries[0].message,
		"the durable diagnostic carries the cleanup failure exactly once")
	_, statErr := os.Stat(materialized.Root())
	assert.ErrorIs(t, statErr, os.ErrNotExist,
		"the successful retry must still remove the private tree")
}

func TestWorkerSanitizesPathAndContentBearingErrors(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	clientPath := "/srv/agent/projects/demo-project/session.jsonl"
	rawContent := "<raw transcript payload>"
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
		}),
		sourceParserFunc(func(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error) {
			return ParsedManifest{}, fmt.Errorf("reading %s: %s", clientPath, rawContent)
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after parse failure")
		}),
	)

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), clientPath,
		"returned errors must not leak client source paths")
	assert.NotContains(t, err.Error(), rawContent,
		"returned errors must not leak raw provider content")
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "parse", queue.retries[0].class)
	assert.NotContains(t, queue.retries[0].message, clientPath,
		"durable queue arguments must not carry client source paths")
	assert.NotContains(t, queue.retries[0].message, rawContent,
		"durable queue arguments must not carry raw provider content")
	assert.Equal(t, "parse:internal", queue.retries[0].message,
		"only allowlisted stage and error codes may be persisted")
}

func TestWorkerKeepsPipelineCauseWhenHeartbeatLosesLeaseToo(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, heartbeatErr error) {
		t.Helper()
		lease := workerTestLease()
		manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
		lease.ManifestID = manifest.ManifestID
		queue := &workerQueueFixture{leases: []JobLease{lease}, heartbeatErr: heartbeatErr}
		parseFailure := errors.New("malformed provider source")
		heartbeatObserved := make(chan struct{})
		var heartbeatOnce sync.Once
		queue.heartbeat = func(context.Context, JobLease, time.Duration) error {
			heartbeatOnce.Do(func() { close(heartbeatObserved) })
			return heartbeatErr
		}
		worker := newWorkerForTest(t, queue,
			manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
				return manifest, nil
			}),
			sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
				return &Materialization{root: t.TempDir(), entries: map[string]string{}}, nil
			}),
			sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
				// Return the real pipeline failure only after a heartbeat beat
				// has failed, so both failures deterministically coexist.
				select {
				case <-heartbeatObserved:
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
				return ParsedManifest{}, parseFailure
			}),
			projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
				return errors.New("unexpected projection after parse failure")
			}),
		)
		worker.HeartbeatInterval = time.Millisecond

		result, err := worker.RunBatch(t.Context())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse",
			"the real pipeline failure must remain the durable stage and cause")
		assert.Empty(t, queue.failures)
		if errors.Is(heartbeatErr, ErrLeaseLost) {
			assert.ErrorIs(t, err, ErrLeaseLost,
				"lease loss must still dominate the outcome classification")
			assert.Equal(t, BatchResult{Claimed: 1, LeaseLost: 1}, result)
			assert.Empty(t, queue.retries)
			return
		}
		assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result,
			"a heartbeat transport failure must not mask the parse failure")
		require.Len(t, queue.retries, 1)
		assert.Equal(t, "parse", queue.retries[0].class)
		assert.Equal(t, "parse:internal", queue.retries[0].message)
	}

	t.Run("lease lost", func(t *testing.T) {
		t.Parallel()
		run(t, ErrLeaseLost)
	})
	t.Run("transport failure", func(t *testing.T) {
		t.Parallel()
		run(t, errors.New("heartbeat transport unavailable"))
	})
}

func TestWorkerPreservesCleanupFailureWhenHeartbeatCancelsCooperativeParse(t *testing.T) {
	t.Parallel()
	lease := workerTestLease()
	manifest := workerTestManifest(t, rawsync.ManifestSnapshot)
	lease.ManifestID = manifest.ManifestID
	heartbeatFailure := errors.New("heartbeat transport unavailable")
	heartbeatObserved := make(chan struct{})
	var heartbeatOnce sync.Once
	queue := &workerQueueFixture{leases: []JobLease{lease}}
	queue.heartbeat = func(context.Context, JobLease, time.Duration) error {
		heartbeatOnce.Do(func() { close(heartbeatObserved) })
		return heartbeatFailure
	}
	cleanupFailure := errors.New("scratch cleanup failed")
	materialized := &Materialization{
		root:    t.TempDir(),
		entries: map[string]string{},
		removeTree: func(string) error {
			return cleanupFailure
		},
	}
	worker := newWorkerForTest(t, queue,
		manifestSourceFunc(func(context.Context, JobLease) (rawsync.CanonicalManifest, error) {
			return manifest, nil
		}),
		sourceMaterializerFunc(func(context.Context, rawsync.CanonicalManifest) (*Materialization, error) {
			return materialized, nil
		}),
		sourceParserFunc(func(ctx context.Context, _ rawsync.CanonicalManifest, _ *Materialization) (ParsedManifest, error) {
			// A cooperative parser: it observes the heartbeat-induced
			// cancellation and reports the generic context error, while the
			// deferred materialization cleanup fails alongside it.
			select {
			case <-heartbeatObserved:
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return ParsedManifest{}, ctx.Err()
		}),
		projectionSinkFunc(func(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error {
			return errors.New("unexpected projection after heartbeat failure")
		}),
	)
	worker.HeartbeatInterval = time.Millisecond

	result, err := worker.RunBatch(t.Context())

	require.Error(t, err)
	assert.NotErrorIs(t, err, context.Canceled,
		"the heartbeat-induced cancellation must not become the classified outcome")
	assert.Contains(t, err.Error(), "parse:internal+cleanup_failed",
		"the durable diagnostic keeps the pipeline stage and the cleanup failure")
	assert.Contains(t, err.Error(), "(materialization cleanup failed)")
	assert.NotContains(t, err.Error(), heartbeatFailure.Error(),
		"heartbeat transport detail must not leak")
	assert.NotContains(t, err.Error(), cleanupFailure.Error(),
		"raw cleanup detail must not leak")
	assert.NotContains(t, err.Error(), materialized.Root(),
		"the raw tree path must not leak")
	assert.Equal(t, BatchResult{Claimed: 1, Retried: 1}, result)
	require.Len(t, queue.retries, 1)
	assert.Equal(t, "parse", queue.retries[0].class,
		"the parse stage stays the durable stage instead of degrading to heartbeat")
	assert.Equal(t, "parse:internal+cleanup_failed", queue.retries[0].message)
	assert.NotContains(t, queue.retries[0].message, heartbeatFailure.Error())
	assert.NotContains(t, queue.retries[0].message, cleanupFailure.Error())
	assert.Empty(t, queue.failures)
}

func TestStripCanceledLeavesPreservesSubstantiveCauses(t *testing.T) {
	t.Parallel()
	substantive := errors.New("malformed provider source")
	wrappedCleanup := fmt.Errorf("%w: cleaning raw materialization: %w",
		errMaterializationCleanup, errors.New("scratch removal failed"),
	)

	tests := []struct {
		name       string
		err        error
		wantMsg    string
		wantInside error
	}{
		{name: "nil stays nil", err: nil},
		{name: "bare cancellation is removed", err: context.Canceled},
		{
			name: "cancellation-only unwrap chain collapses",
			err:  fmt.Errorf("loading manifest: %w", context.Canceled),
		},
		{
			name:       "join keeps substantive branches",
			err:        errors.Join(context.Canceled, substantive),
			wantMsg:    substantive.Error(),
			wantInside: substantive,
		},
		{
			name:       "join keeps wrapped cleanup sentinels",
			err:        errors.Join(context.Canceled, wrappedCleanup),
			wantMsg:    wrappedCleanup.Error(),
			wantInside: errMaterializationCleanup,
		},
		{
			name: "ordinary wrapper around join keeps substantive branch",
			err: fmt.Errorf("parsing source: %w",
				errors.Join(context.Canceled, substantive)),
			wantMsg:    substantive.Error(),
			wantInside: substantive,
		},
		{
			name: "join of only cancellation collapses",
			err:  errors.Join(context.Canceled, fmt.Errorf("parse: %w", context.Canceled)),
		},
		{
			name:       "untouched join keeps its branches and formatting",
			err:        errors.Join(substantive, wrappedCleanup),
			wantMsg:    errors.Join(substantive, wrappedCleanup).Error(),
			wantInside: substantive,
		},
		{
			name:       "substantive error is unchanged",
			err:        substantive,
			wantMsg:    substantive.Error(),
			wantInside: substantive,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stripped, removed := stripCanceledLeaves(tc.err)
			if tc.wantInside == nil {
				assert.Nil(t, stripped)
				if tc.err != nil {
					assert.True(t, removed)
				} else {
					assert.False(t, removed)
				}
				return
			}
			require.NotNil(t, stripped)
			assert.Equal(t, tc.wantMsg, stripped.Error())
			assert.ErrorIs(t, stripped, tc.wantInside)
		})
	}
}

func newWorkerForTest(
	t *testing.T,
	queue JobQueue,
	manifests ManifestSource,
	materializer SourceMaterializer,
	parser SourceParser,
	projection ProjectionSink,
) *Worker {
	t.Helper()
	worker, err := NewWorker(WorkerConfig{
		Queue:             queue,
		Manifests:         manifests,
		Materializer:      materializer,
		Parser:            parser,
		Projection:        projection,
		Owner:             "worker-a",
		BatchSize:         2,
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 10 * time.Second,
		RetryBase:         time.Minute,
		RetryMax:          time.Hour,
		MaxAttempts:       3,
		AttemptTimeout:    5 * time.Minute,
	})
	require.NoError(t, err)
	return worker
}

type workerQueueFixture struct {
	leases       []JobLease
	heartbeatErr error
	heartbeat    func(context.Context, JobLease, time.Duration) error
	heartbeats   atomic.Int64
	retries      []workerRetry
	failures     []workerFailure
}

type workerRetry struct {
	lease       JobLease
	availableAt time.Time
	class       string
	message     string
}

type workerFailure struct {
	lease   JobLease
	class   string
	message string
}

func (q *workerQueueFixture) ClaimRawParseJobs(
	context.Context,
	string,
	int,
	time.Duration,
) ([]JobLease, error) {
	return append([]JobLease(nil), q.leases...), nil
}

func (q *workerQueueFixture) HeartbeatRawParseJob(
	ctx context.Context,
	lease JobLease,
	duration time.Duration,
) error {
	q.heartbeats.Add(1)
	if q.heartbeat != nil {
		return q.heartbeat(ctx, lease, duration)
	}
	return q.heartbeatErr
}

func (q *workerQueueFixture) RetryRawParseJob(
	_ context.Context,
	lease JobLease,
	availableAt time.Time,
	class string,
	message string,
) error {
	q.retries = append(q.retries, workerRetry{
		lease: lease, availableAt: availableAt, class: class, message: message,
	})
	return nil
}

func (q *workerQueueFixture) FailRawParseJob(
	_ context.Context,
	lease JobLease,
	class string,
	message string,
) error {
	q.failures = append(q.failures, workerFailure{
		lease: lease, class: class, message: message,
	})
	return nil
}

type manifestSourceFunc func(context.Context, JobLease) (rawsync.CanonicalManifest, error)

func (f manifestSourceFunc) Load(
	ctx context.Context,
	lease JobLease,
) (rawsync.CanonicalManifest, error) {
	return f(ctx, lease)
}

type sourceMaterializerFunc func(
	context.Context,
	rawsync.CanonicalManifest,
) (*Materialization, error)

func (f sourceMaterializerFunc) Materialize(
	ctx context.Context,
	manifest rawsync.CanonicalManifest,
) (*Materialization, error) {
	return f(ctx, manifest)
}

type sourceParserFunc func(
	context.Context,
	rawsync.CanonicalManifest,
	*Materialization,
) (ParsedManifest, error)

func (f sourceParserFunc) Parse(
	ctx context.Context,
	manifest rawsync.CanonicalManifest,
	materialized *Materialization,
) (ParsedManifest, error) {
	return f(ctx, manifest, materialized)
}

type projectionSinkFunc func(
	context.Context,
	JobLease,
	rawsync.CanonicalManifest,
	ParsedManifest,
) error

func (f projectionSinkFunc) Project(
	ctx context.Context,
	lease JobLease,
	manifest rawsync.CanonicalManifest,
	parsed ParsedManifest,
) error {
	return f(ctx, lease, manifest, parsed)
}

func workerTestLease() JobLease {
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	if err != nil {
		panic(err)
	}
	return JobLease{
		ID: 1, Identity: identity,
		ManifestID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProcessingVersion: "parser-data-17",
		Attempt:           1,
		Owner:             "worker-a",
		ExpiresAt:         time.Now().Add(time.Minute),
	}
}

func workerTestManifest(t *testing.T, kind rawsync.ManifestKind) rawsync.CanonicalManifest {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest := rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         parser.AgentClaude,
		ConfiguredRootID: "root-a",
		SourceKey:        "/canonical/claude/session.jsonl",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             kind,
	}
	if kind == rawsync.ManifestSnapshot {
		data := []byte("session")
		object := objectRefForBytes(t, data)
		manifest.Entries = []rawsync.Entry{{
			Path: "session.jsonl", Type: "file", Length: int64(len(data)),
			Objects: []rawsync.ObjectRef{object},
		}}
	}
	canonical, err := rawsync.ValidateAndCanonicalize(
		identity, manifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	return canonical
}
