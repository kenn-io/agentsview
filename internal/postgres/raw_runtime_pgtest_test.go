//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestHostedRuntimeAcceptanceSelectsAtomically(t *testing.T) {
	f := newProjectionFixture(t)
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-1")
	require.NoError(t, err)
	f.jobs = jobs
	f.custody, err = rawsync.NewService(f.objects, jobs, rawsync.DefaultManifestLimits(), "parser-1")
	require.NoError(t, err)
	m, receipt := f.accept(t, "device-a", "capture-a", "")
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.EqualValues(t, 1, leases[0].ProjectionGeneration)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, projectionOutcome("hello")))
	next, _ := f.accept(t, "device-a", "capture-b", receipt.Receipt)
	_, err = f.custody.CommitManifest(t.Context(), m.Identity, m.Manifest)
	require.NoError(t, err)
	leases, err = jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, next.ManifestID, leases[0].ManifestID)
	assert.EqualValues(t, 2, leases[0].ProjectionGeneration)
}

func TestHostedRuntimeClaimsOnlyConfiguredSelection(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-2")
	require.NoError(t, err)
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, leases)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	leases, err = jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, leases)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	leases, err = jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, "parser-2", leases[0].ProcessingVersion)
	assert.EqualValues(t, 2, leases[0].ProjectionGeneration)
}

func TestHostedRuntimeSettlesWithoutUpload(t *testing.T) {
	f := newProjectionFixture(t)
	ended := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	now := ended.Add(time.Minute)
	f.sink.options.Now = func() time.Time { return now }
	out := projectionOutcome("Implement the change")
	out.Outcome.Results[0].Result.Messages[0].DialogueText = new("")
	out.Outcome.Results[0].Result.Messages[1].DialogueText = new("assistant dialogue")
	out.Outcome.Results[0].Result.Session.EndedAt = ended
	out.Outcome.Results[0].Result.Messages = append(out.Outcome.Results[0].Result.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Continue implementing this change"})
	m, _ := f.accept(t, "device-a", "recent", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "starred", true))
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var outbox int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_embedding_outbox`).Scan(&outbox))
	n, err := f.sink.SettlePendingSignals(t.Context(), 1)
	require.NoError(t, err)
	assert.Zero(t, n)
	now = ended.Add(11 * time.Minute)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	n, err = f.sink.SettlePendingSignals(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	assert.Equal(t, before.IdentityRevision, after.IdentityRevision)
	assert.Equal(t, before.CorpusRevision+1, after.CorpusRevision)
	var pending *string
	var starred bool
	var count int
	require.NoError(t, f.runtime.QueryRow(`SELECT signals_pending_since,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=$1) FROM sessions WHERE id=$1`, after.SessionID).Scan(&pending, &starred))
	assert.Nil(t, pending)
	assert.True(t, starred)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_embedding_outbox`).Scan(&count))
	assert.Equal(t, outbox, count)
	for ordinal, want := range []sql.NullString{{Valid: true}, {String: "assistant dialogue", Valid: true}, {}} {
		var got sql.NullString
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT dialogue_text FROM messages WHERE session_id=$1 AND ordinal=$2`, after.SessionID, ordinal).Scan(&got))
		assert.Equal(t, want, got, "settling preserves dialogue at ordinal %d", ordinal)
	}
	n, err = f.sink.SettlePendingSignals(t.Context(), 1)
	require.NoError(t, err)
	assert.Zero(t, n)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	payload, err := loadRawPayload(t.Context(), tx, after.SessionID)
	require.NoError(t, err)
	assert.Equal(t, new(""), payload.Messages[0].DialogueText, "signal settling preserves classified empty dialogue")
	require.NoError(t, tx.Commit())
}

func TestHostedRuntimeRolloutResumesAndDoesNotResurrect(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("retained")))
	// Only the execution runtime changes; provider and friction versions stay
	// fixed. A failed replacement must leave the published transcript intact.
	const previous = "parser-data-153+friction-v1/runtime-0"
	next := fmt.Sprintf("parser-data-153+friction-v1/runtime-%d", rawderive.ParserRuntimeVersion)
	_, err := f.sink.SelectSourceGeneration(t.Context(), a, previous)
	require.NoError(t, err)
	oldLeases, err := f.jobs.ClaimRawParseJobs(t.Context(), "old-runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, oldLeases, 1)
	failed := oldLeases[0]
	assert.Equal(t, previous, failed.ProcessingVersion)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), failed, "parse", "parse:size_limit"))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	retained, err := (&Store{pg: f.runtime}).GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, retained, 2)
	assert.Equal(t, "retained", retained[0].Content)
	f.accept(t, "device-b", "b", "")
	first, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", next, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Selected)
	second, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", next, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Selected)
	end, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", next, 1)
	require.NoError(t, err)
	assert.True(t, end.Done)
	assert.Zero(t, end.Selected)
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, next)
	require.NoError(t, err)
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, a.ManifestID, leases[0].ManifestID)
	assert.Equal(t, 1, leases[0].Attempt)
	assert.Greater(t, leases[0].ProjectionGeneration, failed.ProjectionGeneration)
	assert.Equal(t, next, leases[0].ProcessingVersion)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], a, projectionOutcome("hello")))
	_, err = f.sink.ScheduleCurrentHeads(t.Context(), "repeat", next, 10)
	require.NoError(t, err)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state))
	assert.Equal(t, "complete", state)
	var attempts int
	require.NoError(t, f.runtime.QueryRow(`SELECT attempt_count FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&attempts))
	assert.Equal(t, 1, attempts, "same-version replay does not reset a completed job")
}

func TestHostedRuntimeRepairWaitsForHeadLock(t *testing.T) {
	for _, tc := range []struct {
		name           string
		selectionMoved bool
		wantCount      int
	}{
		{name: "failed_job_re_failed", wantCount: 2},
		{name: "selection_moved", selectionMoved: true, wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			f.sink.options.RetryPolicy = rawderive.RetryPolicy{Base: time.Second, Maximum: time.Minute, MaxAttempts: 1}
			if !tc.selectionMoved {
				// Job IDs run opposite to rollout's head order.
				b, _ := f.accept(t, "device-b", "failed-b", "")
				bLease := f.lease(t, b)
				require.NoError(t, f.jobs.FailRawParseJob(t.Context(), bLease, "parse", "failed"))
			}
			m, _ := f.accept(t, "device-a", "failed", "")
			lease := f.lease(t, m)
			partial := projectionOutcome("partial")
			partial.Outcome.ResultSetComplete = false
			require.ErrorIs(t, f.sink.Project(t.Context(), lease, m, partial), rawderive.ErrProjectionFailed)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gate, err := f.runtime.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer gate.Rollback()
			// Acceptance and rollouts take this same current-head lock.
			_, err = gate.ExecContext(ctx, `SELECT manifest_id FROM raw_source_heads WHERE manifest_id=$1 FOR UPDATE`, m.ManifestID)
			require.NoError(t, err)
			var count int
			var repairErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				count, repairErr = f.sink.RepairFailedJobs(ctx, "parser-1", 64)
			}()
			defer func() {
				cancel()
				_ = gate.Rollback()
				<-done
			}()
			waitHostedLocks(t, f, ctx, 1)
			if tc.selectionMoved {
				// A rollout selects a new version; the old job stays failed but unselected.
				_, err = selectRawSourceGenerationTx(ctx, gate, m, "parser-2")
				require.NoError(t, err)
			} else {
				// Commit the state left by another repair followed by a worker failure
				// while the waiting repair still has its original statement snapshot.
				_, err = resetRawSourceSelectionTx(ctx, gate, m, "parser-1", 2, lease.ID)
				require.NoError(t, err)
				_, err = gate.ExecContext(ctx, `UPDATE raw_ingest_jobs SET state='failed',attempt_count=1,last_error_class='parse',last_error='failed',updated_at=now() WHERE id=$1`, lease.ID)
				require.NoError(t, err)
			}
			require.NoError(t, gate.Commit())
			<-done
			require.NoError(t, repairErr)
			assert.Equal(t, tc.wantCount, count)
			var state string
			if tc.selectionMoved {
				var version string
				require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT processing_version FROM raw_source_projections WHERE selected_manifest_id=$1`, m.ManifestID).Scan(&version))
				assert.Equal(t, "parser-2", version, "repair overwrote the rollout's selection")
				var selected bool
				require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT state,projection_selected FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state, &selected))
				assert.Equal(t, "failed", state)
				assert.False(t, selected)
			} else {
				require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
				assert.Equal(t, "ready", state)
				var generation int64
				require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT projection_generation FROM raw_source_projections WHERE selected_job_id=$1`, lease.ID).Scan(&generation))
				assert.EqualValues(t, 3, generation)
			}
		})
	}
}

func TestHostedRuntimeRepairReachesNextFailureAfterUploadAdvancesHead(t *testing.T) {
	f := newProjectionFixture(t)
	a, receipt := f.accept(t, "device-a", "failed-a", "")
	aLease := f.lease(t, a)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), aLease, "parse", "failed"))
	b, _ := f.accept(t, "device-b", "failed-b", "")
	bLease := f.lease(t, b)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), bLease, "parse", "failed"))

	// Hold an upload to the oldest failure's source after it locks the head.
	f.parkOn(t, "INSERT", "raw_manifests", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
	require.NoError(t, err)
	next := a.Manifest
	next.CaptureID = "next-a"
	next.ExpectedParentReceipt = receipt.Receipt
	uploaded := make(chan error, 1)
	go func() {
		_, err := f.custody.CommitManifest(ctx, a.Identity, next)
		uploaded <- err
	}()
	defer func() {
		cancel()
		_ = gate.Rollback()
		<-uploaded
	}()
	var count int
	var repairErr error
	done := make(chan struct{})
	waitHostedLocks(t, f, ctx, 1)
	go func() {
		defer close(done)
		count, repairErr = f.sink.RepairFailedJobs(ctx, "parser-1", 1)
	}()
	defer func() {
		cancel()
		_ = gate.Rollback()
		<-done
	}()
	waitHostedLocks(t, f, ctx, 2)
	require.NoError(t, gate.Commit())
	<-done
	require.NoError(t, repairErr)
	assert.Equal(t, 1, count, "repair reported zero while a failure remained")
	var state string
	require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT state FROM raw_ingest_jobs WHERE id=$1`, bLease.ID).Scan(&state))
	assert.Equal(t, "ready", state)
}

func TestHostedRuntimeRepairFailedJobs(t *testing.T) {
	t.Run("field_reset_and_healthy_job", func(t *testing.T) {
		f := newProjectionFixture(t)
		f.sink.options.RetryPolicy = rawderive.RetryPolicy{Base: time.Second, Maximum: time.Minute, MaxAttempts: 1}
		a, _ := f.accept(t, "device-a", "a", "")
		stale := f.lease(t, a)
		partial := projectionOutcome("partial")
		partial.Outcome.ResultSetComplete = false
		require.ErrorIs(t, f.sink.Project(t.Context(), stale, a, partial), rawderive.ErrProjectionFailed)
		b, _ := f.accept(t, "device-b", "b", "")
		healthy := f.lease(t, b)
		require.NoError(t, f.sink.Project(t.Context(), healthy, b, projectionOutcome("complete")))
		var state string
		n, err := f.sink.RepairFailedJobs(t.Context(), "parser-1", 64)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		var attempt int
		var generation int64
		var errorClass, diagnostic string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,attempt_count,projection_generation,last_error_class,last_error FROM raw_ingest_jobs WHERE id=$1`, stale.ID).Scan(&state, &attempt, &generation, &errorClass, &diagnostic))
		assert.Equal(t, "ready", state)
		assert.Zero(t, attempt)
		assert.Empty(t, errorClass)
		assert.Empty(t, diagnostic)
		assert.Equal(t, stale.ProjectionGeneration+1, generation)
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,attempt_count,projection_generation FROM raw_ingest_jobs WHERE id=$1`, healthy.ID).Scan(&state, &attempt, &generation))
		assert.Equal(t, "complete", state)
		assert.Equal(t, healthy.Attempt, attempt)
		assert.Equal(t, healthy.ProjectionGeneration, generation)
		leases, err := f.jobs.ClaimRawParseJobs(t.Context(), stale.Owner, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, leases, 1)
		assert.Equal(t, stale.ID, leases[0].ID)
		assert.Equal(t, stale.Attempt, leases[0].Attempt)
		assert.Greater(t, leases[0].ProjectionGeneration, stale.ProjectionGeneration)
	})
	t.Run("limit_and_other_version", func(t *testing.T) {
		f := newProjectionFixture(t)
		old, _ := f.accept(t, "device-old", "old", "")
		oldLease := f.lease(t, old)
		require.NoError(t, f.jobs.FailRawParseJob(t.Context(), oldLease, "parse", "failed"))
		jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-2")
		require.NoError(t, err)
		for _, device := range []string{"device-a", "device-b"} {
			m, _ := f.accept(t, device, device, "")
			_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
			require.NoError(t, err)
			leases, err := jobs.ClaimRawParseJobs(t.Context(), "worker", 1, time.Minute)
			require.NoError(t, err)
			require.Len(t, leases, 1)
			require.Equal(t, m.ManifestID, leases[0].ManifestID)
			require.Equal(t, "parser-2", leases[0].ProcessingVersion)
			require.NoError(t, jobs.FailRawParseJob(t.Context(), leases[0], "parse", "failed"))
		}
		for _, want := range []int{1, 1, 0} {
			n, err := f.sink.RepairFailedJobs(t.Context(), "parser-2", 1)
			require.NoError(t, err)
			assert.Equal(t, want, n)
		}
		var state, version string
		var generation int64
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,processing_version,projection_generation FROM raw_ingest_jobs WHERE id=$1`, oldLease.ID).Scan(&state, &version, &generation))
		assert.Equal(t, "failed", state)
		assert.Equal(t, "parser-1", version)
		assert.Equal(t, oldLease.ProjectionGeneration, generation)
	})
}

func TestHostedRuntimeSelectionFailureRollsBackAcceptance(t *testing.T) {
	f := newProjectionFixture(t)
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-1")
	require.NoError(t, err)
	f.custody, err = rawsync.NewService(f.objects, jobs, rawsync.DefaultManifestLimits(), "parser-1")
	require.NoError(t, err)
	m, r := f.accept(t, "device-a", "first", "")
	_, err = f.admin.Exec(`CREATE FUNCTION reject_selection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic selection dependency unavailable'; END $$; CREATE TRIGGER reject_selection BEFORE INSERT ON raw_projection_generations FOR EACH ROW EXECUTE FUNCTION reject_selection()`)
	require.NoError(t, err)
	next := m.Manifest
	next.CaptureID = "second"
	next.ExpectedParentReceipt = r.Receipt
	_, err = f.custody.CommitManifest(t.Context(), m.Identity, next)
	require.Error(t, err)
	for _, table := range []string{"raw_manifests", "raw_ingest_jobs", "raw_projection_generations"} {
		var count int
		require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
	var head string
	var generation int
	require.NoError(t, f.runtime.QueryRow(`SELECT manifest_id,generation FROM raw_source_heads`).Scan(&head, &generation))
	assert.Equal(t, m.ManifestID, head)
	assert.Equal(t, 1, generation)
}

func TestHostedRuntimeRefusesLegacyReaderAndPush(t *testing.T) {
	f := newProjectionFixture(t)
	require.ErrorContains(t, RejectHostedPush(t.Context(), f.runtime), "hosted-owned")
	_, err := NewStore(f.dsn, f.schema, false)
	require.ErrorContains(t, err, "raw_tenant")
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	_, err = f.admin.Exec(`REVOKE UPDATE ON raw_content_revisions FROM "` + f.role + `"`)
	require.NoError(t, err)
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "privileges")
}

func TestHostedRuntimeRequiresFrictionProjectionPrivileges(t *testing.T) {
	f := newProjectionFixture(t)
	for _, table := range []string{"friction_findings"} {
		_, err := f.admin.Exec(`REVOKE DELETE ON "` + table + `" FROM "` + f.role + `"`)
		require.NoError(t, err)
		require.ErrorContains(t,
			CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull),
			table,
		)
		_, err = f.admin.Exec(`GRANT DELETE ON "` + table + `" TO "` + f.role + `"`)
		require.NoError(t, err)

		_, err = f.admin.Exec(`REVOKE SELECT,INSERT ON "` + table + `" FROM "` + f.role + `"`)
		require.NoError(t, err)
		require.ErrorContains(t,
			CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull),
			table,
		)
		_, err = f.admin.Exec(`GRANT SELECT,INSERT ON "` + table + `" TO "` + f.role + `"`)
		require.NoError(t, err)
	}
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
}

func TestHostedRuntimeSettlingRechecksConcurrentVisibility(t *testing.T) {
	for _, mode := range []string{"removal", "exclusion", "replacement", "curation"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			ended := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			now := ended.Add(time.Minute)
			f.sink.options.Now = func() time.Time { return now }
			out := projectionOutcome("Implement the change")
			out.Outcome.Results[0].Result.Session.EndedAt = ended
			out.Outcome.Results[0].Result.Messages = append(out.Outcome.Results[0].Result.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Continue"})
			m, receipt := f.accept(t, "device-a", "initial", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
			old, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			now = ended.Add(11 * time.Minute)
			var action func() error
			switch mode {
			case "removal", "replacement":
				next, _ := f.accept(t, "device-a", "next", receipt.Receipt)
				lease := f.lease(t, next)
				changed := out
				if mode == "removal" {
					changed.Outcome.Results = nil
				} else {
					changed = projectionOutcome("new content")
				}
				action = func() error { return f.sink.Project(t.Context(), lease, next, changed) }
			case "exclusion":
				require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
				action = func() error { _, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable"); return err }
			case "curation":
				action = func() error { return f.sink.SetCuration(t.Context(), "codex:portable", "starred", true) }
			}
			gate, err := f.runtime.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer gate.Rollback()
			_, err = gate.Exec(`SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, old.GroupID)
			require.NoError(t, err)
			waitForLocks := func(n int) {
				require.Eventually(t, func() bool {
					var count int
					err := f.admin.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'`, f.role).Scan(&count)
					return err == nil && count >= n
				}, 3*time.Second, 10*time.Millisecond)
			}
			changed := make(chan error, 1)
			go func() { changed <- action() }()
			waitForLocks(1)
			type result struct {
				n   int
				err error
			}
			settled := make(chan result, 1)
			go func() { n, err := f.sink.SettlePendingSignals(t.Context(), 1); settled <- result{n, err} }()
			waitForLocks(2)
			require.NoError(t, gate.Commit())
			require.NoError(t, <-changed)
			got := <-settled
			require.NoError(t, got.err)
			if mode == "curation" {
				assert.Equal(t, 1, got.n)
				stars, err := (&Store{pg: f.runtime}).ListStarredSessionIDs(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []string{old.SessionID}, stars)
			} else {
				assert.Zero(t, got.n)
				var count int
				require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE id=$1`, old.SessionID).Scan(&count))
				assert.Zero(t, count)
			}
		})
	}
}

func TestHostedRuntimeSubprocessTombstoneRetainsPublicSession(t *testing.T) {
	f := newProjectionFixture(t)
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-1")
	require.NoError(t, err)
	f.jobs = jobs
	f.custody, err = rawsync.NewService(f.objects, jobs, rawsync.DefaultManifestLimits(), "parser-1")
	require.NoError(t, err)
	m, receipt := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("published")))
	public, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	defer public.Close()
	before, err := public.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, before.Sessions, 1)
	tombstone := m.Manifest
	tombstone.Kind = rawsync.ManifestTombstone
	tombstone.Entries = nil
	tombstone.CaptureID = "removed"
	tombstone.ExpectedParentReceipt = receipt.Receipt
	_, err = f.custody.CommitManifest(t.Context(), m.Identity, tombstone)
	require.NoError(t, err)
	canonical, err := rawsync.ValidateAndCanonicalize(m.Identity, tombstone, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	isolated, err := rawderive.NewSubprocessParser(time.Second)
	require.NoError(t, err)
	worker, err := rawderive.NewWorker(rawderive.WorkerConfig{Queue: f.jobs, Manifests: rawderive.ManifestLoader{Store: f.objects, Limits: rawsync.DefaultManifestLimits()}, Materializer: rawderive.Materializer{Store: f.objects, BaseDir: t.TempDir(), MaxTotalBytes: 1}, Parser: isolated, Projection: f.sink, Owner: "tombstone-worker", BatchSize: 1, LeaseDuration: time.Minute, HeartbeatInterval: time.Second, AttemptTimeout: time.Second, RetryBase: time.Second, RetryMax: time.Minute, MaxAttempts: 2})
	require.NoError(t, err)
	result, err := worker.RunBatch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Succeeded)
	assert.Zero(t, result.Retried)
	var active, proof int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_session_branches WHERE active`).Scan(&active))
	assert.Equal(t, 1, active)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM session_sources`).Scan(&proof))
	assert.Equal(t, 1, proof)
	after, err := public.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, before.Sessions, after.Sessions)
	retained, err := public.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.NotNil(t, retained)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE manifest_id=$1`, canonical.ManifestID).Scan(&state))
	assert.Equal(t, "complete", state)
}

func TestHostedRuntimeAcceptsOnlyRequiredProjectionMutations(t *testing.T) {
	f := newProjectionFixture(t)
	// Friction rows are replaced on a rule-version refresh. Other child removal
	// follows the sessions FK, so those immutable tables need no DELETE grant.
	for _, table := range []string{"raw_projection_generations", "raw_source_contributions", "raw_session_public_aliases", "raw_embedding_outbox", "messages", "tool_calls", "tool_result_events", "usage_events", "secret_findings"} {
		_, err := f.admin.Exec(`REVOKE UPDATE,DELETE ON "` + table + `" FROM "` + f.role + `"`)
		require.NoError(t, err)
	}
	_, err := f.admin.Exec(`GRANT UPDATE(observed_outcome,observed_repeat) ON tool_calls TO "` + f.role + `"`)
	require.NoError(t, err)
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "owner provisioning required")
	_, err = f.admin.Exec(`GRANT UPDATE(sequence_ending) ON tool_calls TO "` + f.role + `"`)
	require.NoError(t, err)
	for _, table := range []string{"friction_findings"} {
		_, err := f.admin.Exec(`REVOKE UPDATE ON "` + table + `" FROM "` + f.role + `"`)
		require.NoError(t, err)
	}
	for _, table := range []string{"tool_calls", "tool_result_events", "usage_events", "pinned_messages"} {
		_, err := f.admin.Exec(`REVOKE USAGE ON SEQUENCE "` + table + `_id_seq" FROM "` + f.role + `"; GRANT UPDATE ON SEQUENCE "` + table + `_id_seq" TO "` + f.role + `"`)
		require.NoError(t, err)
	}
	_, err = f.admin.Exec(`REVOKE USAGE ON SEQUENCE secret_findings_id_seq FROM "` + f.role + `"`)
	require.NoError(t, err)
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "messages.dialogue_text")
	_, err = f.admin.Exec(`GRANT UPDATE(dialogue_text) ON messages TO "` + f.role + `"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	m, receipt := f.accept(t, "device-a", "first", "")
	out := projectionOutcome("first")
	out.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"token":"AKIA7QHWN2DKR4FYPLJM"}`
	out.Outcome.Results[0].Result.Messages[1].ToolCalls[0].ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", Source: "tool_result", Status: "errored", Content: "Error: synthetic hosted failure"}}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	for _, table := range []string{"tool_result_events", "usage_events", "secret_findings", "friction_findings"} {
		var count int
		require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		assert.Positive(t, count, table)
	}
	// Drop both the stored observations and their cached snapshot, so the
	// replay must write them back with only the column UPDATE grants.
	_, err = f.admin.Exec(`UPDATE tool_calls SET observed_outcome=NULL,observed_repeat=NULL,sequence_ending=NULL`)
	require.NoError(t, err)
	_, err = f.admin.Exec(`UPDATE raw_content_revisions SET recency_state = recency_state - 'ToolObservations'`)
	require.NoError(t, err)
	replay, replayReceipt := f.accept(t, "device-a", "observation-refresh", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, replay), replay, out))
	receipt = replayReceipt
	var restored int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM tool_calls WHERE observed_outcome IS NOT NULL AND observed_repeat IS NOT NULL`).Scan(&restored))
	assert.Positive(t, restored)
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "starred", true))
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "note"))
	n, _ := f.accept(t, "device-a", "next", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, n), n, projectionOutcome("replacement")))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
	removed, err := f.sink.ExcludeTrashedSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.True(t, removed)
	var count int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions`).Scan(&count))
	assert.Zero(t, count)
}

func TestHostedRuntimeUsagePolicyRequiresVectorRemovalPrivileges(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.Exec(`REVOKE DELETE ON vector_documents FROM "` + f.role + `"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentUsage), "vector_documents")
	_, err = f.admin.Exec(`GRANT DELETE ON vector_documents TO "` + f.role + `"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentUsage))
	f.sink.options.Content.ArchiveContent = config.ArchiveContentUsage
	m, _ := f.accept(t, "device-a", "usage", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("discarded transcript")))
	var content string
	require.NoError(t, f.runtime.QueryRow(`SELECT content FROM messages ORDER BY ordinal LIMIT 1`).Scan(&content))
	assert.Empty(t, content)
}

func TestHostedRuntimeRequiresProvisionedObservationColumns(t *testing.T) {
	f := newProjectionFixture(t)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	// A schema that owner provisioning has not upgraded lacks the columns.
	_, err := f.admin.Exec(`ALTER TABLE tool_calls DROP COLUMN sequence_ending`)
	require.NoError(t, err)
	err = CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull)
	require.ErrorContains(t, err, "owner provisioning required")
}
