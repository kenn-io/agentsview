//go:build pgtest

package postgres

import (
	"crypto/sha256"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func parityProjectionBinding(t *testing.T, f projectionFixture) rawderive.ParityBinding {
	t.Helper()
	binding := parityPhysicalBinding(f.tenant)
	binding.Request.Cohort = rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"}
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT data_version,quality_signal_version,secrets_rules_version FROM sessions LIMIT 1`).Scan(
		&binding.Versions.Data, &binding.Versions.Quality, &binding.Versions.SecretRules,
	))
	return binding
}

func TestParityInventoryExactSourceAuthoritySurvivesInactiveTarget(t *testing.T) {
	f := newProjectionFixture(t)
	own, _ := f.acceptScoped(t, "device-a", "own", "", parser.AgentCodex, "root-a", "own.jsonl")
	combined := projectionOutcome("exact parent")
	combined.Outcome.Results = append(combined.Outcome.Results, linkChildOutcome().Outcome.Results...)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, own), own, combined))
	alternate, _ := f.acceptScoped(t, "device-a", "alternate", "", parser.AgentCodex, "root-a", "alternate.jsonl")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, alternate), alternate, projectionOutcome("fallback parent")))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id)
		SELECT 'codex:portable',group_id FROM raw_session_branches WHERE source_id=$1 ON CONFLICT DO NOTHING`, rawSourceID(alternate))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET active=FALSE WHERE source_id=$1 AND member_id='codex:portable'`, rawSourceID(own))
	require.NoError(t, err)

	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	resolved, err := readParityLinkTarget(t.Context(), tx, rawSourceID(own), rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "codex:portable"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Equal(t, rawSourceID(own), resolved.Target.SourceID)
	assert.Equal(t, "portable", resolved.Target.LogicalKey)
	assert.Empty(t, resolved.Unresolved)
}

func TestParityEvidencePreparesGraphFromSealedRelationshipProof(t *testing.T) {
	f, store, lease, _ := boundParityFixture(t)
	owner := rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "child", Kind: "session"}
	original := rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "provider-parent"}
	resolved := original
	resolved.Unresolved = ""
	resolved.Target = rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "parent", Kind: "session"}
	lookup, encoded := encodeParityRelationshipDependency(owner, original, resolved)
	keyDigest, expected := parityTestSHA(lookup), parityTestSHA(encoded)
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest)
		VALUES($1,$2,'source-a','raw','device-a','claude','root-a','manifest-a',1,'receipt-a',$3)`, lease.RunID, lease.InitEpoch, make([]byte, 32))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
		VALUES($1,$2,'source-a','relationship',$3,$4,$5)`, lease.RunID, lease.InitEpoch, encoded, keyDigest, expected)
	require.NoError(t, err)
	overlayLookup, overlayEncoded := encodeParityOverlayDependency(owner, rawderive.ParityOverlay{Excluded: true, ProviderExcluded: true})
	overlayDigest := sha256.Sum256(overlayLookup)
	overlayExpected := sha256.Sum256(overlayEncoded)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
		VALUES($1,$2,'source-a','overlay',$3,$4,$5)`, lease.RunID, lease.InitEpoch, overlayEncoded, overlayDigest[:], overlayExpected[:])
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE,state='running',inventory_digest=$2,baseline_digest=$2 WHERE run_id=$1`, lease.RunID, make([]byte, 32))
	require.NoError(t, err)

	graph, err := store.PrepareParityGraph(t.Context(), lease, rawderive.ParitySource{ID: "source-a"}, rawderive.ParityGraph{Key: owner, Links: []rawderive.ParityLink{original}, Overlay: rawderive.ParityOverlay{ProviderExcluded: true}})
	require.NoError(t, err)
	require.Len(t, graph.Links, 1)
	assert.Equal(t, resolved.Target, graph.Links[0].Target)
	assert.Empty(t, graph.Links[0].Unresolved)
	assert.True(t, graph.Overlay.Excluded, "sealed curation overlay is copied")
	assert.True(t, graph.Overlay.ProviderExcluded, "sealed curation must not erase independently derived provider exclusion")
}

func TestParityEvidenceReportsMissingProviderExclusionProofWithoutErasingCandidate(t *testing.T) {
	f, store, lease, _ := boundParityFixture(t)
	owner := rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "excluded", Kind: "session"}
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest)
		VALUES($1,$2,'source-a','raw','device-a','claude','root-a','manifest-a',1,'receipt-a',$3)`, lease.RunID, lease.InitEpoch, make([]byte, 32))
	require.NoError(t, err)
	overlayLookup, overlayEncoded := encodeParityOverlayDependency(owner, rawderive.ParityOverlay{})
	overlayDigest := sha256.Sum256(overlayLookup)
	overlayExpected := sha256.Sum256(overlayEncoded)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
		VALUES($1,$2,'source-a','overlay',$3,$4,$5)`, lease.RunID, lease.InitEpoch, overlayEncoded, overlayDigest[:], overlayExpected[:])
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE,state='running',inventory_digest=$2,baseline_digest=$2 WHERE run_id=$1`, lease.RunID, make([]byte, 32))
	require.NoError(t, err)

	graph, err := store.PrepareParityGraph(t.Context(), lease, rawderive.ParitySource{ID: "source-a"}, rawderive.ParityGraph{
		Key: owner, Overlay: rawderive.ParityOverlay{ProviderExcluded: true},
	})
	require.ErrorIs(t, err, rawderive.ErrParityExclusionProvenanceUnavailable)
	assert.True(t, graph.Overlay.ProviderExcluded)
}

func parityTestSHA(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func TestParityInventoryFallbackIgnoresOtherDevicesAndRoots(t *testing.T) {
	f := newProjectionFixture(t)
	outcomeFor := func(id string) rawderive.ParsedManifest {
		outcome := projectionOutcome(id)
		outcome.Outcome.Results[0].Result.Session.ID = "codex:" + id
		outcome.Outcome.Results[0].Result.Session.SourceSessionID = id
		return outcome
	}
	owner, _ := f.acceptScoped(t, "device-a", "owner", "", parser.AgentCodex, "root-a", "owner")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, owner), owner, outcomeFor("owner")))
	matching, _ := f.acceptScoped(t, "device-a", "matching", "", parser.AgentCodex, "root-a", "matching")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, matching), matching, outcomeFor("matching")))
	otherDevice, _ := f.acceptScoped(t, "device-b", "other-device", "", parser.AgentCodex, "root-a", "other-device")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, otherDevice), otherDevice, outcomeFor("device")))
	otherRoot, _ := f.acceptScoped(t, "device-a", "other-root", "", parser.AgentCodex, "root-b", "other-root")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, otherRoot), otherRoot, outcomeFor("root")))
	for _, manifest := range []rawsync.CanonicalManifest{matching, otherDevice, otherRoot} {
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id) SELECT 'shared-parent',group_id FROM raw_session_branches WHERE source_id=$1 ON CONFLICT DO NOTHING`, rawSourceID(manifest))
		require.NoError(t, err)
	}
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	resolved, err := readParityLinkTarget(t.Context(), tx, rawSourceID(owner), rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "shared-parent"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Equal(t, rawSourceID(matching), resolved.Target.SourceID)
	plural, _ := f.acceptScoped(t, "device-a", "plural", "", parser.AgentCodex, "root-a", "plural")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, plural), plural, outcomeFor("plural")))
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id) SELECT 'shared-parent',group_id FROM raw_session_branches WHERE source_id=$1 ON CONFLICT DO NOTHING`, rawSourceID(plural))
	require.NoError(t, err)
	tx, err = f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	resolved, err = readParityLinkTarget(t.Context(), tx, rawSourceID(owner), rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "shared-parent"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Empty(t, resolved.Target.SourceID, "plural same-cohort targets remain unresolved")
	assert.Equal(t, "shared-parent", resolved.Unresolved)
}

func TestParityInventoryFallbackCoalescesSharedPhysicalAuthority(t *testing.T) {
	f := newProjectionFixture(t)
	owner, _ := f.acceptScoped(t, "device-a", "owner", "", parser.AgentCodex, "root-a", "owner")
	ownerOutcome := projectionOutcome("owner")
	ownerOutcome.Outcome.Results[0].Result.Session.ID = "codex:owner"
	ownerOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "owner"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, owner), owner, ownerOutcome))
	var targetIDs []string
	for _, key := range []string{"target-a", "target-b"} {
		manifest, _ := f.acceptScoped(t, "device-a", key, "", parser.AgentCodex, "root-a", key)
		target := projectionOutcome("identical target")
		target.Outcome.Results[0].Result.Session.ID = "codex:shared"
		target.Outcome.Results[0].Result.Session.SourceSessionID = "shared"
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, target))
		targetIDs = append(targetIDs, rawSourceID(manifest))
	}
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id)
		SELECT 'shared-physical',group_id FROM raw_session_branches WHERE source_id=$1 ON CONFLICT DO NOTHING`, targetIDs[0])
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	resolved, err := readParityLinkTarget(t.Context(), tx, rawSourceID(owner), rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "shared-physical"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	expectedSource := targetIDs[0]
	if targetIDs[1] < expectedSource {
		expectedSource = targetIDs[1]
	}
	assert.Equal(t, expectedSource, resolved.Target.SourceID)
	assert.Equal(t, "shared", resolved.Target.LogicalKey)
	assert.Empty(t, resolved.Unresolved)
}

func TestParityInventoryFallbackRejectsOrphanAliasTarget(t *testing.T) {
	f := newProjectionFixture(t)
	owner, _ := f.acceptScoped(t, "device-a", "owner", "", parser.AgentCodex, "root-a", "owner")
	ownerOutcome := projectionOutcome("owner")
	ownerOutcome.Outcome.Results[0].Result.Session.ID = "codex:owner"
	ownerOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "owner"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, owner), owner, ownerOutcome))
	target, _ := f.acceptScoped(t, "device-a", "target", "", parser.AgentCodex, "root-a", "target")
	targetOutcome := projectionOutcome("target")
	targetOutcome.Outcome.Results[0].Result.Session.ID = "codex:target"
	targetOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "target"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, target), target, targetOutcome))
	var physical string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT physical_session_id FROM session_sources WHERE source_id=$1`, rawSourceID(target)).Scan(&physical))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_public_aliases(alias_id,group_id) SELECT 'orphan-target',group_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(target))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM session_sources WHERE source_id=$1`, rawSourceID(target))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, physical)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	resolved, err := readParityLinkTarget(t.Context(), tx, rawSourceID(owner), rawderive.ParityLink{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Unresolved: "orphan-target"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Empty(t, resolved.Target.SourceID)
	assert.Equal(t, "orphan-target", resolved.Unresolved)
}

func TestParityInventoryMarksMissingSessionSourceProofAmbiguous(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
	outcome := projectionOutcome("content")
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = ""
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, outcome))
	_, err := f.runtime.ExecContext(t.Context(), `DELETE FROM session_sources WHERE source_id=$1`, rawSourceID(manifest))
	require.NoError(t, err)
	binding := parityProjectionBinding(t, f)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 1)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, members, 1)
	assert.NotEmpty(t, members[0].key.LogicalKey)
	assert.Equal(t, "ambiguous", members[0].mappingState)
	assert.Equal(t, rawderive.ParityAmbiguous, members[0].verdict)
}

func TestParityInventoryUsesParserIdentityFallbackWhenSourceSessionIDIsEmpty(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
	outcome := projectionOutcome("content")
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = ""
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, outcome))
	_, expectedLogical := rawderive.GroupID(manifest, db.Session{ID: "codex:portable", Agent: "codex"})
	binding := parityProjectionBinding(t, f)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 1)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, members, 1)
	assert.Equal(t, expectedLogical, members[0].key.LogicalKey)
	assert.Equal(t, "exact", members[0].mappingState)
	assert.Empty(t, members[0].verdict)
}

func TestParityInventoryRejectsWrongPhysicalAndGroupProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, projectionFixture, string, string)
	}{
		{
			name: "wrong physical session",
			mutate: func(t *testing.T, f projectionFixture, sourceID, otherPhysical string) {
				_, err := f.runtime.ExecContext(t.Context(), `UPDATE session_sources SET physical_session_id=$2 WHERE source_id=$1`, sourceID, otherPhysical)
				require.NoError(t, err)
			},
		},
		{
			name: "wrong materialized group",
			mutate: func(t *testing.T, f projectionFixture, sourceID, _ string) {
				_, err := f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_group_id='wrong-group' WHERE id=(SELECT physical_session_id FROM session_sources WHERE source_id=$1)`, sourceID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, projectionOutcome("content")))
			other, _ := f.acceptScoped(t, "device-a", "other", "", parser.AgentCodex, "root-a", "source-b")
			otherOutcome := projectionOutcome("other")
			otherOutcome.Outcome.Results[0].Result.Session.ID = "codex:other"
			otherOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "other"
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, otherOutcome))
			var otherPhysical string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT physical_session_id FROM session_sources WHERE source_id=$1`, rawSourceID(other)).Scan(&otherPhysical))
			test.mutate(t, f, rawSourceID(manifest), otherPhysical)
			binding := parityProjectionBinding(t, f)
			tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			require.NoError(t, err)
			members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 1)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			require.Len(t, members, 1)
			assert.Equal(t, "ambiguous", members[0].mappingState)
			assert.Equal(t, rawderive.ParityAmbiguous, members[0].verdict)
		})
	}
}

func TestParityInventoryUsesOnlyActiveMembershipAndKeepsCurationAsOverlay(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, projectionOutcome("content")))
	binding := parityProjectionBinding(t, f)
	read := func() []parityCapturedMember {
		tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(t, err)
		members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 8)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return members
	}
	var groupID, branchID string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT group_id,branch_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(manifest)).Scan(&groupID, &branchID))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,$2,'excluded','true')`, groupID, branchID)
	require.NoError(t, err)
	members := read()
	require.Len(t, members, 1)
	assert.Equal(t, "session", members[0].key.Kind)
	assert.True(t, members[0].overlay.Excluded)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET active=FALSE WHERE source_id=$1`, rawSourceID(manifest))
	require.NoError(t, err)
	assert.Empty(t, read(), "retained inactive history is not current membership")
}

func TestParityInventoryUnresolvedParserParentBlocksMatching(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
	outcome := projectionOutcome("content")
	parent := "missing-parent"
	outcome.Outcome.Results[0].Result.Session.ParentSessionID = parent
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, outcome))
	binding := parityProjectionBinding(t, f)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 8)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, members, 1)
	assert.Equal(t, rawderive.ParityAmbiguous, members[0].verdict)
	require.Len(t, members[0].links, 2)
	assert.Equal(t, "parent", members[0].links[0].Kind)
	assert.Equal(t, parent, members[0].links[0].Unresolved)
	assert.Equal(t, "parser-parent", members[0].links[1].Kind)
	assert.Equal(t, parent, members[0].links[1].Unresolved)
}

func TestParityInventoryKeepsParentAndParserParentAsDistinctProof(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.acceptScoped(t, "device-a", "capture", "", parser.AgentCodex, "root-a", "source-a")
	outcome := projectionOutcome("parent")
	child := projectionOutcome("child").Outcome.Results[0]
	child.Result.Session.ID = "codex:child"
	child.Result.Session.SourceSessionID = "child"
	parserParent := "codex:portable"
	child.Result.Session.ParentSessionID = "codex:portable"
	outcome.Outcome.Results = append(outcome.Outcome.Results, child)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, outcome))
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE sessions SET parser_parent_session_id=$1 WHERE id='codex:child'`, parserParent)
	require.NoError(t, err)
	binding := parityProjectionBinding(t, f)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	members, _, err := readParityRawMemberPage(t.Context(), tx, binding, rawSourceID(manifest), "", 4)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var childMember *parityCapturedMember
	for i := range members {
		if members[i].key.LogicalKey == "child" {
			childMember = &members[i]
		}
	}
	require.NotNil(t, childMember)
	assert.Empty(t, childMember.verdict)
	require.Len(t, childMember.links, 2)
	assert.Equal(t, "parent", childMember.links[0].Kind)
	assert.Empty(t, childMember.links[0].Unresolved)
	assert.Equal(t, "parser-parent", childMember.links[1].Kind)
	assert.Empty(t, childMember.links[1].Unresolved)
	assert.Equal(t, childMember.links[0].Target, childMember.links[1].Target)
}

func TestParityInventoryResumeSkipsCompletedAndStaleSourcesAcrossGenerations(t *testing.T) {
	f, store, lease, _ := boundParityFixture(t)
	for _, source := range []struct {
		id       string
		complete bool
		verdict  any
	}{
		{id: "source-complete", complete: true},
		{id: "source-pending"},
		{id: "source-stale", verdict: string(rawderive.ParityStale)},
	} {
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,evidence_generation,candidate_complete)
			VALUES($1,$2,$3,'raw','device-a','claude','root-a','manifest',1,'receipt',$4,$5,0,$6)`, lease.RunID, lease.InitEpoch, source.id, make([]byte, 32), source.verdict, source.complete)
		require.NoError(t, err)
	}
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE,state='running',inventory_digest=$2,baseline_digest=$2 WHERE run_id=$1`, lease.RunID, make([]byte, 32))
	require.NoError(t, err)
	sources, err := store.NextParitySources(t.Context(), lease, "", 8)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "source-pending", sources[0].ID)
}
