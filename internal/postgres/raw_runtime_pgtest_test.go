//go:build pgtest

package postgres

import (
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
	n, err = f.sink.SettlePendingSignals(t.Context(), 1)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestHostedRuntimeRolloutResumesAndDoesNotResurrect(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "a", "")
	f.accept(t, "device-b", "b", "")
	first, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", "parser-2", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Selected)
	second, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", "parser-2", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, second.Selected)
	end, err := f.sink.ScheduleCurrentHeads(t.Context(), "rollout", "parser-2", 1)
	require.NoError(t, err)
	assert.True(t, end.Done)
	assert.Zero(t, end.Selected)
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-2")
	require.NoError(t, err)
	leases, err := jobs.ClaimRawParseJobs(t.Context(), "runtime", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, a.ManifestID, leases[0].ManifestID)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], a, projectionOutcome("hello")))
	_, err = f.sink.ScheduleCurrentHeads(t.Context(), "repeat", "parser-2", 10)
	require.NoError(t, err)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state))
	assert.Equal(t, "complete", state)
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

func TestHostedRuntimeSubprocessTombstoneRemovesPublicSource(t *testing.T) {
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
	assert.Zero(t, active)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM session_sources`).Scan(&proof))
	assert.Zero(t, proof)
	after, err := public.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, after.Sessions)
	gone, err := public.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Nil(t, gone)
	var state string
	require.NoError(t, f.runtime.QueryRow(`SELECT state FROM raw_ingest_jobs WHERE manifest_id=$1`, canonical.ManifestID).Scan(&state))
	assert.Equal(t, "complete", state)
}

func TestHostedRuntimeAcceptsOnlyRequiredProjectionMutations(t *testing.T) {
	f := newProjectionFixture(t)
	// These immutable payload/metadata tables require insertion and reads, not
	// arbitrary update/delete grants. Child removal follows the sessions FK.
	for _, table := range []string{"raw_projection_generations", "raw_source_contributions", "raw_session_public_aliases", "raw_embedding_outbox", "messages", "tool_calls", "tool_result_events", "usage_events", "secret_findings"} {
		_, err := f.admin.Exec(`REVOKE UPDATE,DELETE ON "` + table + `" FROM "` + f.role + `"`)
		require.NoError(t, err)
	}
	for _, table := range []string{"tool_calls", "tool_result_events", "usage_events", "pinned_messages"} {
		_, err := f.admin.Exec(`REVOKE USAGE ON SEQUENCE "` + table + `_id_seq" FROM "` + f.role + `"; GRANT UPDATE ON SEQUENCE "` + table + `_id_seq" TO "` + f.role + `"`)
		require.NoError(t, err)
	}
	_, err := f.admin.Exec(`REVOKE USAGE ON SEQUENCE secret_findings_id_seq FROM "` + f.role + `"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	m, receipt := f.accept(t, "device-a", "first", "")
	out := projectionOutcome("first")
	out.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"token":"AKIA7QHWN2DKR4FYPLJM"}`
	out.Outcome.Results[0].Result.Messages[1].ToolCalls[0].ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", Source: "tool_result", Status: "completed", Content: "result"}}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	for _, table := range []string{"tool_result_events", "usage_events", "secret_findings"} {
		var count int
		require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		assert.Positive(t, count, table)
	}
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
