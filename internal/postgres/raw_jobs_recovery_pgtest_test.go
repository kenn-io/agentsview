//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"hegel.dev/go/hegel"
)

func TestRawJobsConcurrentReplayUsesDurableSelection(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "concurrent", "")
	old := f.lease(t, m)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), old, "parse", "parse:internal"))
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "worker", 1, time.Minute, 5)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), leases[0], "parse", "parse:internal"))
	result := replayFailedJobs(t, t.Context(), f.sink, 2)
	require.Equal(t, int64(1), result.Replayed)
	require.Equal(t, int64(1), result.Superseded)
	var state string
	var attempt int
	var epoch int64
	var failedAt *time.Time
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,attempt_count,lease_epoch,failed_at FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state, &attempt, &epoch, &failedAt))
	require.Equal(t, "ready", state)
	require.Zero(t, attempt)
	require.Equal(t, leases[0].LeaseEpoch+1, epoch)
	require.Nil(t, failedAt)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, old.ID).Scan(&state))
	require.Equal(t, "superseded", state)
}

func TestRawJobsTransitionModelProperty(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "model", "")
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		maximum := hegel.Draw(ht, hegel.Integers(1, 10)) // Hosted configuration permits 1–10 attempts.
		operations := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 8)))
		_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='ready',attempt_count=0,failed_at=NULL,available_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL`)
		require.NoError(ht, err)
		state, attempt, expired := "ready", 0, false
		var epoch int64
		require.NoError(ht, f.runtime.QueryRowContext(t.Context(), `SELECT lease_epoch FROM raw_ingest_jobs WHERE manifest_id=$1`, m.ManifestID).Scan(&epoch))
		var issued []rawderive.JobLease
		claim := func() {
			leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "shared-owner", 1, time.Minute, maximum)
			require.NoError(ht, err)
			eligible := state == "ready" || state == "retrying" || state == "leased" && expired
			if !eligible || attempt >= maximum {
				require.Empty(ht, leases)
				if eligible {
					state = "failed"
				}
			} else {
				state, expired = "leased", false
				attempt++
				epoch++
				require.Len(ht, leases, 1)
				require.Equal(ht, attempt, leases[0].Attempt)
				require.Equal(ht, epoch, leases[0].LeaseEpoch)
				issued = append(issued, leases[0])
			}
		}
		// Bound SQL work when materializing a generated history, not its domain.
		for _, op := range operations[:min(len(operations), 32)] {
			switch op {
			case 0:
				claim()
			case 1:
				if state == "leased" {
					_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE manifest_id=$1`, m.ManifestID)
					require.NoError(ht, err)
					expired = true
				}
			case 7, 8:
				count := 1
				if op == 8 {
					count = 2
				}
				replayed := replayFailedJobs(ht, t.Context(), f.sink, count).Replayed
				if state == "failed" {
					require.Equal(ht, int64(1), replayed)
					state = "ready"
					attempt = 0
					epoch++
				} else {
					require.Zero(ht, replayed)
				}
			default:
				if len(issued) == 0 {
					continue
				}
				lease := issued[hegel.Draw(ht, hegel.Integers(0, len(issued)-1))]
				err := recoveryOperation(t.Context(), f, m, lease, op-2)
				if state == "leased" && !expired && lease.LeaseEpoch == epoch {
					require.NoError(ht, err)
					switch op {
					case 3:
						state = "failed"
					case 4:
						state = "retrying"
					case 5, 6:
						state = "complete"
					}
				} else {
					require.ErrorIs(ht, err, rawderive.ErrLeaseLost)
				}
			}
			var got string
			var gotAttempt int
			var gotEpoch int64
			require.NoError(ht, f.runtime.QueryRowContext(t.Context(), `SELECT state,attempt_count,lease_epoch FROM raw_ingest_jobs WHERE manifest_id=$1`, m.ManifestID).Scan(&got, &gotAttempt, &gotEpoch))
			require.Equal(ht, state, got)
			require.Equal(ht, attempt, gotAttempt)
			require.Equal(ht, epoch, gotEpoch)
		}
		// Finite replay plus fair crash-only progress must reach a terminal state.
		for state != "complete" && state != "failed" {
			if state == "leased" {
				_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET lease_expires_at=clock_timestamp()-interval '1 second'`)
				require.NoError(ht, err)
				expired = true
			}
			claim()
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func recoveryOperation(ctx context.Context, f projectionFixture, m rawsync.CanonicalManifest, lease rawderive.JobLease, operation int) error {
	switch operation {
	case 0:
		return f.jobs.HeartbeatRawParseJob(ctx, lease, time.Minute)
	case 1:
		return f.jobs.FailRawParseJob(ctx, lease, "parse", "parse:internal")
	case 2:
		return f.jobs.RetryRawParseJob(ctx, lease, time.Now().Add(-time.Second), "parse", "parse:internal")
	case 3:
		return f.jobs.CompleteRawParseJob(ctx, lease)
	default:
		return f.sink.Project(ctx, lease, m, projectionOutcome("current"))
	}
}

// Synchronize starts so both replay tests exercise overlapping invocations.
func replayFailedJobs(t require.TestingT, ctx context.Context, sink *RawProjectionStore, calls int) RawJobReplayResult {
	type outcome struct {
		result RawJobReplayResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, calls)
	for range calls {
		go func() {
			<-start
			result, err := sink.RetryFailedRawJobs(ctx, RawJobRetryFilter{All: true})
			results <- outcome{result, err}
		}()
	}
	close(start)
	var total RawJobReplayResult
	for range calls {
		result := <-results
		require.NoError(t, result.err)
		total.Replayed += result.result.Replayed
		total.Superseded += result.result.Superseded
	}
	return total
}
