//go:build pgtest

package postgres

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

type projectionFixture struct {
	hostedFixture
	sink    *RawProjectionStore
	custody *rawsync.Service
	jobs    *RawIngestStore
	objects rawsync.ObjectStore
}

func newProjectionFixture(t *testing.T) projectionFixture {
	t.Helper()
	f := newHostedFixture(t, "tenant-projection")
	repo, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repo.Content())
	require.NoError(t, err)
	jobs, err := NewTenantRawIngestStore(f.runtime, f.tenant)
	require.NoError(t, err)
	custody, err := rawsync.NewService(objects, jobs, rawsync.DefaultManifestLimits(), "parser-1")
	require.NoError(t, err)
	sink, err := NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
	return projectionFixture{f, sink, custody, jobs, objects}
}
func (f projectionFixture) accept(t *testing.T, device, capture, parent string, providers ...parser.AgentType) (rawsync.CanonicalManifest, rawsync.CommitResult) {
	t.Helper()
	provider := parser.AgentCodex
	if len(providers) > 0 {
		provider = providers[0]
	}
	return f.acceptScoped(t, device, capture, parent, provider, "", "")
}

func (f projectionFixture) acceptScoped(t *testing.T, device, capture, parent string, provider parser.AgentType, root, source string) (rawsync.CanonicalManifest, rawsync.CommitResult) {
	t.Helper()
	digest := sha256.Sum256([]byte("synthetic-" + device))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_devices(device_id,display_name,credential_sha256,created_at) VALUES($1,$1,$2,clock_timestamp()) ON CONFLICT(device_id) DO NOTHING`, device, digest[:])
	require.NoError(t, err)
	identity, err := rawsync.NewAuthIdentity(f.tenant, device)
	require.NoError(t, err)
	body := []byte("synthetic custody " + capture)
	ref := rawCustodyObjectRef(t, body)
	_, err = f.custody.FinalizeObject(t.Context(), identity, provider, ref, bytes.NewReader(body))
	require.NoError(t, err)
	manifest := rawCustodyManifest(capture, parent, rawIngestCapturedAt(), ref)
	manifest.Provider = provider
	if root != "" {
		manifest.ConfiguredRootID = root
	}
	if source != "" {
		manifest.SourceKey = source
	}
	canonical, err := rawsync.ValidateAndCanonicalize(identity, manifest, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	result, err := f.custody.CommitManifest(t.Context(), identity, manifest)
	require.NoError(t, err)
	return canonical, result
}
func projectionOutcome(content string) rawderive.ParsedManifest {
	return rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
		Session:     parser.ParsedSession{ID: "codex:portable", SourceSessionID: "portable", Agent: parser.AgentCodex, Project: "synthetic", Machine: "capture-device", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		Messages:    []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: content}, {Ordinal: 1, Role: parser.RoleAssistant, Content: "checking", ToolCalls: []parser.ParsedToolCall{{ToolUseID: "call-1", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`}}}},
		UsageEvents: []parser.ParsedUsageEvent{{Source: "turn", Model: "synthetic-model", InputTokens: 11, OutputTokens: 7}},
	}}}}}
}
func (f projectionFixture) lease(t *testing.T, m rawsync.CanonicalManifest) rawderive.JobLease {
	t.Helper()
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "projection-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	return leases[0]
}

// Losing the atomic write/completion boundary or any normalized child write
// must fail this through actual custody, lease, and normal physical Store reads.
func TestRawProjectionPublishesNormalizedGraph(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	lease := f.lease(t, m)
	require.NoError(t, f.sink.Project(t.Context(), lease, m, projectionOutcome("hello")))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, resolved.State)
	store := &Store{pg: f.runtime}
	messages, err := store.GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "hello", messages[0].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "Bash", messages[1].ToolCalls[0].ToolName)
	events, err := store.GetSessionUsageRows(t.Context(), []string{resolved.SessionID})
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Rows, 1)
	assert.Equal(t, 11, events.Rows[0].InputTokens)
	assert.Equal(t, 7, events.Rows[0].OutputTokens)
	var state string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
	assert.Equal(t, "complete", state)
	assert.ErrorIs(t, f.sink.Project(t.Context(), lease, m, projectionOutcome("must not replace")), rawderive.ErrLeaseLost)
}

func TestRawProjectionGenerationAndLeaseFences(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	old := f.lease(t, m)
	generation, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	assert.Greater(t, generation, old.ProjectionGeneration)
	assert.ErrorIs(t, f.sink.Project(t.Context(), old, m, projectionOutcome("old")), rawderive.ErrLeaseLost)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "successor", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, projectionOutcome("new")))
	again, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	assert.Equal(t, generation, again)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_projection_generations`).Scan(&count))
	assert.Equal(t, 2, count)
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	messages, err := (&Store{pg: f.runtime}).GetMessages(t.Context(), resolved.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "new", messages[0].Content)
}
func TestRawProjectionExpiredAndSupersededLeases(t *testing.T) {
	for _, mode := range []string{"expired", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, accepted := f.accept(t, "device-a", "capture-a", "")
			lease := f.lease(t, m)
			if mode == "expired" {
				_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, lease.ID)
				require.NoError(t, err)
			} else {
				f.accept(t, "device-a", "capture-b", accepted.Receipt)
			}
			assert.ErrorIs(t, f.sink.Project(t.Context(), lease, m, projectionOutcome("old")), rawderive.ErrLeaseLost)
			var count int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}
func TestRawProjectionRollsBackPayloadAndProofWhenCompletionFails(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	lease := f.lease(t, m)
	_, err := f.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_projection_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='complete' THEN RAISE EXCEPTION 'synthetic completion dependency unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_projection_completion BEFORE UPDATE ON raw_ingest_jobs FOR EACH ROW EXECUTE FUNCTION reject_projection_completion()`)
	require.NoError(t, err)
	err = f.sink.Project(t.Context(), lease, m, projectionOutcome("rollback"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthetic completion dependency unavailable")
	for _, table := range []string{"sessions", "raw_session_branches", "raw_content_revisions", "raw_source_contributions", "raw_embedding_outbox"} {
		var count int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
	var revision int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&revision))
	assert.Zero(t, revision)
	var state string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
	assert.Equal(t, "leased", state)
}

func (f projectionFixture) alias(t *testing.T, m rawsync.CanonicalManifest) string {
	t.Helper()
	var alias string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT a.alias_id FROM raw_session_public_aliases a JOIN raw_session_branches b ON b.branch_id=a.anchor_branch WHERE b.source_id=$1`, rawSourceID(m)).Scan(&alias))
	return alias
}
func TestRawProjectionCohortCurationFollowsAliases(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			f := newProjectionFixture(t)
			devices := []string{"device-a", "device-b"}
			if reverse {
				slices.Reverse(devices)
			}
			manifests := map[string]rawsync.CanonicalManifest{}
			receipts := map[string]string{}
			for _, device := range devices {
				m, accepted := f.accept(t, device, "capture-"+device, "")
				manifests[device] = m
				receipts[device] = accepted.Receipt
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome(device)))
			}
			a := f.alias(t, manifests["device-a"])
			b := f.alias(t, manifests["device-b"])
			require.NoError(t, f.sink.SetCuration(t.Context(), a, "trashed", true))
			require.NoError(t, f.sink.SetCuration(t.Context(), b, "starred", true))
			require.NoError(t, f.sink.SetCuration(t.Context(), a, "display_name", "alpha"))
			require.NoError(t, f.sink.SetCuration(t.Context(), b, "display_name", "beta"))
			// Convergence cannot overwrite either branch's choices. The visible branch
			// supplies the name and star; the hidden duplicate is not a trash-list item.
			m, accepted := f.accept(t, "device-a", "converged-a", receipts["device-a"])
			receipts["device-a"] = accepted.Receipt
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("device-b")))
			ar, err := f.sink.Resolve(t.Context(), a)
			require.NoError(t, err)
			br, err := f.sink.Resolve(t.Context(), b)
			require.NoError(t, err)
			assert.Equal(t, ar.SessionID, br.SessionID)
			var name string
			var trashed, starred bool
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE id=$1`, ar.SessionID).Scan(&name, &trashed, &starred))
			assert.Equal(t, "beta", name)
			assert.False(t, trashed)
			assert.True(t, starred)
			require.NoError(t, f.sink.SetCuration(t.Context(), a, "starred", false))
			require.NoError(t, f.sink.SetCuration(t.Context(), a, "trashed", true))
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE id=$1`, ar.SessionID).Scan(&trashed, &starred))
			assert.True(t, trashed)
			assert.False(t, starred)
			require.NoError(t, f.sink.SetCuration(t.Context(), b, "trashed", false))
			require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "display_name", "shared"))
			m, _ = f.accept(t, "device-a", "split-a", receipts["device-a"])
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("split")))
			for _, alias := range []string{a, b} {
				r, err := f.sink.Resolve(t.Context(), alias)
				require.NoError(t, err)
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE id=$1`, r.SessionID).Scan(&name, &trashed, &starred))
				assert.Equal(t, "shared", name)
				assert.False(t, trashed)
				assert.False(t, starred)
			}
			r, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, RawIdentityAmbiguous, r.State)
			require.Len(t, r.Variants, 2)
		})
	}
}

func TestRawProjectionPinsRemainUnresolvedAfterOrdinalReuse(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "pin-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("original")))
	alias := f.alias(t, m)
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "group note"))
	require.NoError(t, f.sink.SetPin(t.Context(), alias, 1, true, "other pin"))
	before, err := f.sink.Resolve(t.Context(), alias)
	require.NoError(t, err)
	pins, err := (&Store{pg: f.runtime}).ListPinnedMessages(t.Context(), before.SessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	m, _ = f.accept(t, "device-a", "pin-b", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("replacement")))
	after, err := f.sink.Resolve(t.Context(), alias)
	require.NoError(t, err)
	pins, err = (&Store{pg: f.runtime}).ListPinnedMessages(t.Context(), after.SessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	refs, err := f.sink.PinReferences(t.Context(), alias)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	var unresolved int
	for _, ref := range refs {
		if !ref.Resolved {
			unresolved++
			assert.Equal(t, 0, ref.Ordinal)
			assert.Equal(t, "group note", ref.Note)
		}
	}
	assert.Equal(t, 1, unresolved)
	require.NoError(t, f.sink.SetPin(t.Context(), alias, 1, false, ""))
	pins, err = (&Store{pg: f.runtime}).ListPinnedMessages(t.Context(), after.SessionID, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestRawProjectionClaimsNeverResurrectUnselectedGeneration(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "selection-claim", "")
	old := f.lease(t, m)
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID)
	require.NoError(t, err)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "selected-only", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, "parser-2", leases[0].ProcessingVersion)
}
