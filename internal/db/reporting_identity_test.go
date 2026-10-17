package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

func TestReportingProjectEvidenceRootConsensus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		roots      []string
		wantRoot   string
		resolution export.ProjectResolution
	}{
		{"one root", []string{"root-a"}, "root-a", export.ProjectResolutionResolved},
		{"same root", []string{"root-a", "root-a"}, "root-a", export.ProjectResolutionResolved},
		{"different roots", []string{"root-a", "root-b", "root-a"}, "", export.ProjectResolutionResolved},
		{"missing root fact", []string{"root-a", ""}, "", export.ProjectResolutionResolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contributors := map[string]bool{}
			sessions := map[string]activity.SessionMeta{}
			references := map[string]export.ProjectReference{}
			for i, root := range tc.roots {
				id := []string{"session-a", "session-b", "session-c"}[i]
				contributors[id] = true
				sessions[id] = activity.SessionMeta{Project: "api"}
				references[id] = export.ProjectReference{
					ProjectKey: "project-a", Resolution: export.ProjectResolutionResolved,
					Identity: &export.ProjectIdentity{Key: "identity-a", Kind: export.ProjectKindMachineRoot, RepositoryKey: "repository-a", RootKey: root},
				}
			}
			projects := map[string]export.ProjectMapEntry{"api": {ProjectKey: "project-a"}}
			for range 32 {
				got := reportingProjectEvidence(contributors, sessions, projects, references)["project-a"]
				require.Equal(t, tc.resolution, got.Resolution)
				require.NotNil(t, got.Identity)
				require.Equal(t, &export.ProjectIdentity{Key: "identity-a", Kind: export.ProjectKindMachineRoot, RepositoryKey: "repository-a", RootKey: tc.wantRoot}, got.Identity)
			}
			for i, root := range tc.roots {
				assert.Equal(t, root, references[[]string{"session-a", "session-b", "session-c"}[i]].Identity.RootKey, "aggregation must not alter session facts")
			}
		})
	}
}

func TestReportingWorktreeIdentityExportsRemainStable(t *testing.T) {
	version := export.ReportingJointSchemaVersion
	d := testDB(t)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	for i, root := range []string{"/synthetic/main", "/synthetic/linked"} {
		id := []string{"session-a", "session-b"}[i]
		insertSession(t, d, id, "api", func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:01:00Z")
		})
		insertMessages(t, d,
			Message{SessionID: id, Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"},
			Message{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:01:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)},
		)
		require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
			SessionID: id, Project: "api", Machine: "synthetic", RootPath: root,
			RepositoryPath: "/synthetic/main", WorktreeRootPath: root,
			WorktreeRelationship: []export.WorktreeRelationship{export.WorktreeMain, export.WorktreeLinked}[i], ObservedAt: date,
		}))
	}
	opts := ReportingExportOptions{Date: date, Now: date.Add(24 * time.Hour), SchemaVersion: version, Bucket: "5m"}
	first, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	firstBytes, err := export.MarshalCanonical(first)
	require.NoError(t, err)
	require.Len(t, first.Hours[12].Joint.Projects, 1)
	for _, project := range first.Hours[12].Joint.Projects {
		require.Equal(t, export.ProjectResolutionResolved, project.Resolution)
		require.NotNil(t, project.Identity)
		assert.Equal(t, export.ProjectKindMachineRoot, project.Identity.Kind)
		assert.NotEmpty(t, project.Identity.Key)
		assert.NotEmpty(t, project.Identity.RepositoryKey)
		require.Empty(t, project.Identity.RootKey, "no individual worktree represents the shared repository")
	}
	for range 8 {
		day, err := d.ExportReportingDay(t.Context(), opts)
		require.NoError(t, err)
		dayBytes, err := export.MarshalCanonical(day)
		require.NoError(t, err)
		assert.Equal(t, firstBytes, dayBytes)
		digests, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
			From: date, To: date, Now: opts.Now, SchemaVersion: version, Bucket: "5m",
		})
		require.NoError(t, err)
		require.Len(t, digests, 1)
		assert.Equal(t, first.Digest, digests[0].DayDigest)
		require.Len(t, digests[0].HourDigests, 24)
		for i, hour := range first.Hours {
			assert.Equal(t, hour.Digest, digests[0].HourDigests[i])
		}
	}
}

func TestReportingJointProjectIdentityFollowsReadSnapshot(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"session-a", "session-b"} {
		insertSession(t, d, id, "api", func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:01:00Z")
		})
		insertMessages(t, d,
			Message{SessionID: id, Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"},
			Message{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:01:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)},
		)
		require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
			SessionID: id, Project: "api", Machine: "synthetic", GitRemote: "https://example.com/team/api.git",
			ObservedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		}))
	}
	opts := ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: export.ReportingJointSchemaVersion,
	}
	day, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	hour := day.Hours[12]
	assert.InDelta(t, 2.0, hour.Activity.Totals.AgentMinutes, 0)
	require.Len(t, hour.Joint.Projects, 1)
	require.NotEmpty(t, hour.Joint.Cells)
	key := hour.Joint.Cells[0].ProjectKey
	identity := hour.Joint.Projects[key]
	require.Equal(t, export.ProjectResolutionResolved, identity.Resolution)
	require.NotNil(t, identity.Identity)
	assert.Equal(t, "example.com/team/api", identity.Identity.NormalizedRemote)

	opts.afterSnapshot = func() {
		_, err := d.getWriter().Exec(t.Context(), `DELETE FROM session_project_identity_snapshots WHERE session_id = 'session-b'`)
		require.NoError(t, err)
	}
	during, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, day.Digest, during.Digest, "identity and usage share the read snapshot")

	opts.afterSnapshot = nil
	after, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	afterHour := after.Hours[12]
	assert.NotEqual(t, hour.Digest, afterHour.Digest, "identity-only correction changes the hour digest")
	assert.NotEqual(t, day.Digest, after.Digest)
	assert.Equal(t, int64(20), afterHour.Usage.Totals.OutputTokens)
	require.Len(t, afterHour.Joint.Projects, 2)
	assert.Equal(t, identity, afterHour.Joint.Projects[key], "the verified repository keeps its group")
	for projectKey, entry := range afterHour.Joint.Projects {
		if projectKey != key {
			assert.Equal(t, export.ProjectResolutionUnknown, entry.Resolution)
			assert.Nil(t, entry.Identity)
		}
	}
}

func TestReportingProjectEvidenceIsIndependentOfMapOrder(t *testing.T) {
	// Two checkouts of one machine-local repository resolve to the same
	// project key but carry different root keys and display labels.
	identity := func(rootKey string) *export.ProjectIdentity {
		return &export.ProjectIdentity{
			Key: "p1:project", Kind: export.ProjectKindMachineRoot,
			RootKey: rootKey, RepositoryKey: "repo1:repository",
		}
	}
	contributors := map[string]bool{"session-b": true, "session-a": true}
	sessions := map[string]activity.SessionMeta{
		"session-a": {SessionID: "session-a", Project: "api"},
		"session-b": {SessionID: "session-b", Project: "api-worktree"},
	}
	projects := map[string]export.ProjectMapEntry{
		"api":          {ProjectKey: "pl1:project"},
		"api-worktree": {ProjectKey: "pl1:project"},
	}
	references := map[string]export.ProjectReference{
		"session-a": {ProjectKey: "pl1:project", Resolution: export.ProjectResolutionResolved, Identity: identity("r1:first")},
		"session-b": {ProjectKey: "pl1:project", Resolution: export.ProjectResolutionResolved, Identity: identity("r1:second")},
	}
	// Go randomizes map iteration, so repeat enough that an order-dependent
	// choice would surface.
	for range 64 {
		evidence := reportingProjectEvidence(contributors, sessions, projects, references)
		require.Contains(t, evidence, "pl1:project")
		entry := evidence["pl1:project"]
		require.NotNil(t, entry.Identity)
		assert.Equal(t, export.ProjectResolutionResolved, entry.Resolution)
		assert.Empty(t, entry.Identity.RootKey, "different worktrees have no shared root")
		assert.Equal(t, "api", entry.DisplayLabel)
	}
}
