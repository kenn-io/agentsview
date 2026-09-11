//go:build pgtest

package postgres

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionPreservesAbsentProviderTitleFromSameSource(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "named", "")
	named := projectionOutcome("first")
	named.Outcome.Results[0].Result.Session.SessionName = "Provider title"
	named.Outcome.Results[0].Result.Session.SessionNamePresent = true
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, named))
	next, receipt := f.accept(t, "device-a", "unnamed", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("changed content")))
	identity, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var title *string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT session_name FROM sessions WHERE id=$1`, identity.SessionID).Scan(&title))
	require.NotNil(t, title)
	assert.Equal(t, "Provider title", *title)
	cleared, _ := f.accept(t, "device-a", "cleared", receipt.Receipt)
	explicit := projectionOutcome("changed content")
	explicit.Outcome.Results[0].Result.Session.SessionNamePresent = true
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, cleared), cleared, explicit))
	identity, err = f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT session_name FROM sessions WHERE id=$1`, identity.SessionID).Scan(&title))
	assert.Nil(t, title)
}

func TestRawProjectionEqualSourcesNoOpAndRemoval(t *testing.T) {
	f := newProjectionFixture(t)
	a, ar := f.accept(t, "device-a", "equal-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("equal")))
	first, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	a2, ar2 := f.accept(t, "device-a", "equal-a-retry", ar.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a2), a2, projectionOutcome("equal")))
	retry, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, first.CorpusRevision, retry.CorpusRevision)
	assert.Equal(t, first.IdentityRevision, retry.IdentityRevision)
	b, _ := f.accept(t, "device-b", "equal-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("equal")))
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM session_sources`).Scan(&count))
	assert.Equal(t, 2, count)
	oldAlias := f.alias(t, a)
	require.NoError(t, f.sink.SetCuration(t.Context(), oldAlias, "display_name", "retained name"))
	removed, rr := f.accept(t, "device-a", "removed-a", ar2.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, removed), removed, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}))
	gone, err := f.sink.Resolve(t.Context(), oldAlias)
	require.NoError(t, err)
	assert.Equal(t, RawIdentityGone, gone.State)
	surviving, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, surviving.State)
	assert.Equal(t, first.SessionID, surviving.SessionID)
	returned, _ := f.accept(t, "device-a", "returned-a", rr.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, returned), returned, projectionOutcome("returned")))
	again, err := f.sink.Resolve(t.Context(), oldAlias)
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, again.State)
	var name string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT display_name FROM sessions WHERE id=$1`, again.SessionID).Scan(&name))
	assert.Equal(t, "retained name", name)
	private, err := f.sink.Resolve(t.Context(), again.SessionID)
	require.NoError(t, err)
	assert.Equal(t, RawIdentityGone, private.State)
}

func TestRawProjectionPartialMembersKeepPriorProof(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "multi-a", "")
	outcome := projectionOutcome("first")
	second := projectionOutcome("second").Outcome.Results[0]
	second.Result.Session.ID = "codex:second"
	second.Result.Session.SourceSessionID = "second"
	outcome.Outcome.Results = append(outcome.Outcome.Results, second)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	previous, err := f.sink.Resolve(t.Context(), "codex:second")
	require.NoError(t, err)
	next, _ := f.accept(t, "device-a", "multi-b", accepted.Receipt)
	lease := f.lease(t, next)
	partial := projectionOutcome("updated first")
	partial.Outcome.ResultSetComplete = false
	partial.Outcome.SourceErrors = []parser.SourceError{{SessionID: "codex:second", Err: errors.New("synthetic parse failure"), Retryable: true}}
	require.ErrorIs(t, f.sink.Project(t.Context(), lease, next, partial), rawderive.ProjectionRetrying)
	retained, err := f.sink.Resolve(t.Context(), "codex:second")
	require.NoError(t, err)
	assert.Equal(t, previous.SessionID, retained.SessionID)
	var proof, state string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT manifest_id FROM session_sources WHERE session_id=$1`, retained.SessionID).Scan(&proof))
	assert.Equal(t, m.ManifestID, proof)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&state))
	assert.Equal(t, "retrying", state)
}
func TestRawProjectionRetainsProviderHistoryAndEveryAcceptedContributor(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "history-a", "", parser.AgentRooCode)
	outcome := projectionOutcome("retained history")
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentRooCode
	outcome.Outcome.Results[0].Result.Session.ID = "roocode:portable"
	outcome.Outcome.Results[0].Result.Session.File = parser.FileInfo{Path: "legacy/source.json", Size: 123, Mtime: 456, Hash: "prior-hash"}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	first, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	next, _ := f.accept(t, "device-a", "history-b", accepted.Receipt, parser.AgentRooCode)
	outcome.Outcome.Results[0].Result.Messages = nil
	outcome.Outcome.Results[0].Result.UsageEvents = nil
	outcome.Outcome.Results[0].Result.Session.File = parser.FileInfo{Path: "legacy/source.json", Size: 41, Mtime: 789, Hash: "empty-current-hash"}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, outcome))
	retained, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	assert.Equal(t, first.SessionID, retained.SessionID)
	messages, err := (&Store{pg: f.runtime}).GetMessages(t.Context(), retained.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "retained history", messages[0].Content)
	usage, err := (&Store{pg: f.runtime}).GetSessionUsageRows(t.Context(), []string{retained.SessionID})
	require.NoError(t, err)
	require.Len(t, usage.Rows, 1)
	assert.Equal(t, 7, usage.Rows[0].OutputTokens)
	rows, err := f.runtime.QueryContext(t.Context(), `SELECT c.manifest_id,c.projection_generation,c.processing_version,c.prior_contributed,c.payload,s.device_id,s.provider FROM raw_source_contributions c JOIN raw_session_branches b ON b.branch_id=c.branch_id JOIN raw_source_projections s ON s.source_id=b.source_id ORDER BY c.projection_generation`)
	require.NoError(t, err)
	defer rows.Close()
	for i, expected := range []struct {
		manifest, hash string
		size, mtime    int64
		contributed    bool
	}{
		{m.ManifestID, "prior-hash", 123, 456, false},
		{next.ManifestID, "empty-current-hash", 41, 789, true},
	} {
		require.True(t, rows.Next())
		var manifest, version, device, provider string
		var generation int64
		var contributed bool
		var payload []byte
		require.NoError(t, rows.Scan(&manifest, &generation, &version, &contributed, &payload, &device, &provider))
		assert.Equal(t, expected.manifest, manifest)
		assert.Equal(t, int64(i+1), generation)
		assert.Equal(t, "parser-1", version)
		assert.Equal(t, expected.contributed, contributed)
		assert.Equal(t, "device-a", device)
		assert.Equal(t, string(parser.AgentRooCode), provider)
		metadata, err := decodeRawPayload(payload)
		require.NoError(t, err)
		assert.Equal(t, new("legacy/source.json"), metadata.Session.FilePath)
		assert.Equal(t, new(expected.hash), metadata.Session.FileHash)
		assert.Equal(t, new(expected.size), metadata.Session.FileSize)
		assert.Equal(t, new(expected.mtime), metadata.Session.FileMtime)
		assert.Empty(t, metadata.Messages, "contribution metadata must not duplicate the shared transcript")
	}
	assert.False(t, rows.Next())
	require.NoError(t, rows.Err())
}

func TestRawProjectionUsageOnlyMemberAndDerivedFindings(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "complete-graph", "")
	outcome := projectionOutcome("inspect")
	outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"token":"AKIA7QHWN2DKR4FYPLJM"}`
	outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].ResultEvents = []parser.ParsedToolResultEvent{{ToolUseID: "call-1", Source: "tool", Status: "completed", Content: "synthetic result"}}
	zero := projectionOutcome("unused").Outcome.Results[0]
	zero.Result.Session.ID = "codex:usage-only"
	zero.Result.Session.SourceSessionID = "usage-only"
	zero.Result.Messages = nil
	outcome.Outcome.Results = append(outcome.Outcome.Results, zero)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	graph, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	store := &Store{pg: f.runtime}
	messages, err := store.GetMessages(t.Context(), graph.SessionID, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls, 1)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "synthetic result", messages[1].ToolCalls[0].ResultEvents[0].Content)
	findings, err := store.ListSecretFindings(t.Context(), db.SecretFindingFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, findings.Findings)
	assert.Equal(t, "tool_input", findings.Findings[0].LocationKind)
	assert.Equal(t, 1, findings.Findings[0].MessageOrdinal)
	session, err := store.GetSession(t.Context(), graph.SessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Positive(t, session.SecretLeakCount)
	usageOnly, err := f.sink.Resolve(t.Context(), "codex:usage-only")
	require.NoError(t, err)
	rows, err := store.GetSessionUsageRows(t.Context(), []string{usageOnly.SessionID})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, 7, rows.Rows[0].OutputTokens)
}

func TestRawProjectionKeepsLegacyProofSeparate(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,first_message) VALUES('codex:portable','legacy','archive','codex','legacy preserved')`)
	require.NoError(t, err)
	m, _ := f.accept(t, "device-a", "legacy-separate", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw")))
	var kind, message string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT provenance_kind,first_message FROM sessions WHERE id='codex:portable'`).Scan(&kind, &message))
	assert.Equal(t, "legacy", kind)
	assert.Equal(t, "legacy preserved", message)
	r, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT provenance_kind FROM sessions WHERE id=$1`, r.SessionID).Scan(&kind))
	assert.Equal(t, "raw", kind)
}

func TestRawProjectionTombstoneHidesEveryPhysicalInventoryAndReactivationRestoresCuration(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "visible", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("needle")))
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "starred", true))
	require.NoError(t, f.sink.SetPin(t.Context(), "codex:portable", 0, true, "retained"))
	store := &Store{pg: f.runtime}
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	tomb := rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: m.Manifest.Provider, ConfiguredRootID: m.Manifest.ConfiguredRootID, SourceKey: m.Manifest.SourceKey, ExpectedParentReceipt: accepted.Receipt, CaptureID: "tombstone", CapturedAt: rawIngestCapturedAt(), Kind: rawsync.ManifestTombstone}
	canonical, err := rawsync.ValidateAndCanonicalize(m.Identity, tomb, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	removed, err := f.custody.CommitManifest(t.Context(), m.Identity, tomb)
	require.NoError(t, err)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, canonical), canonical, rawderive.ParsedManifest{Tombstone: true}))
	page, err := store.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	assert.Empty(t, page.Sessions)
	trash, err := store.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, trash)
	stars, err := store.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, stars)
	matches, err := store.Search(t.Context(), db.SearchFilter{Query: "needle"})
	require.NoError(t, err)
	assert.Empty(t, matches.Results)
	next, _ := f.accept(t, "device-a", "reactivated", removed.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, projectionOutcome("needle")))
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	stars, err = store.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{after.SessionID}, stars)
	pins, err := store.ListPinnedMessages(t.Context(), after.SessionID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
}

func TestRawProjectionProviderFallbackAliasSurvivesConflict(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "vibe-"+device, "", parser.AgentVibe)
		out := projectionOutcome(device)
		out.Outcome.Results[0].Result.Session.Agent = parser.AgentVibe
		out.Outcome.Results[0].Result.Session.ID = "vibe:portable"
		out.Outcome.Results[0].Result.Session.File.Path = "logs/session_fallback/messages.jsonl"
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	}
	alias, err := f.sink.Resolve(t.Context(), "vibe:session_fallback")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, alias.State)
	require.Len(t, alias.Variants, 2)
	for _, variant := range alias.Variants {
		resolved, err := f.sink.Resolve(t.Context(), variant)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, resolved.State)
	}
}

func TestRawProjectionNonportableIdentityHasExplicitAliasConflict(t *testing.T) {
	f := newProjectionFixture(t)
	a, ar := f.accept(t, "device-a", "opaque-a", "")
	out := projectionOutcome("equal text")
	out.Outcome.Results[0].Result.Session.SourceSessionID = ""
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, out))
	b, _ := f.accept(t, "device-b", "opaque-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, out))
	r, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, r.State)
	require.Len(t, r.Variants, 2)
	for _, alias := range r.Variants {
		variant, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, variant.State)
		assert.Equal(t, alias, variant.PublicID)
	}
	next, _ := f.accept(t, "device-a", "opaque-removed", ar.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}}))
	remaining, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, remaining.State)
	assert.Equal(t, "codex:portable", remaining.PublicID)
}
