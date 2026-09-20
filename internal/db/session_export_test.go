package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllSessionExportKeepsTerminationCutoffAcrossPages(t *testing.T) {
	d := testSessionExportDB(t)
	synctest.Test(t, func(t *testing.T) {
		// Keep the older activity timestamps within the valid 2000..2100 range.
		time.Sleep(time.Hour)
		for _, id := range []string{"active-a", "active-b"} {
			ts := time.Now().UTC().Add(-9 * time.Minute).Format(time.RFC3339)
			insertExportSession(t, d, Session{
				ID: id, Project: "termination", StartedAt: &ts, EndedAt: &ts,
			})
		}
		pages, err := d.exportAllSessionSummaries(t.Context(), SessionExportOptions{
			Filter: SessionFilter{Project: "termination", Termination: "active"},
			Limit:  1,
		}, func(page int, _ *sql.Tx) error {
			if page == 1 {
				// Both sessions age out of the active window during the export.
				time.Sleep(2 * time.Minute)
			}
			return nil
		}, nil)
		require.NoError(t, err)
		var ids []string
		for _, page := range pages {
			for _, row := range page.Rows {
				ids = append(ids, row.ID)
			}
		}
		assert.Equal(t, []string{"active-a", "active-b"}, ids)
	})
}

func TestSessionSummaryExportRowsAreContentFreeAndMetadataScoped(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	seedSessionExportPricing(t, d)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:          "alpha",
			Machine:          "laptop",
			RootPath:         "/repo/alpha/worktrees/feature",
			GitRemote:        "https://github.com/acme/alpha.git",
			GitRemoteName:    "origin",
			WorktreeName:     "feature",
			WorktreeRootPath: "/repo/alpha",
			ObservedAt:       time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}), "upsert alpha project identity")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:    "unreturned",
			Machine:    "laptop",
			RootPath:   "/repo/unreturned",
			GitRemote:  "https://github.com/acme/unreturned.git",
			ObservedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}), "upsert unrelated project identity")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:          "alpha",
			Machine:          "laptop",
			RootPath:         "/repo/alpha/main",
			GitRemote:        "https://github.com/acme/alpha.git",
			GitRemoteName:    "origin",
			WorktreeName:     "main",
			WorktreeRootPath: "/repo/alpha/main",
			ObservedAt:       time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}), "upsert alpha main project identity")

	insertExportSession(t, d, Session{
		ID:                   "alpha-parent",
		Project:              "alpha",
		Machine:              "laptop",
		Agent:                "claude",
		StartedAt:            Ptr("2026-05-01T10:00:00Z"),
		EndedAt:              Ptr("2026-05-01T10:10:00Z"),
		MessageCount:         3,
		UserMessageCount:     1,
		TotalOutputTokens:    90,
		PeakContextTokens:    1200,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
		Cwd:                  "/repo/alpha/worktrees/feature/pkg",
		GitBranch:            "feature/export-contracts",
	})
	insertMessages(t, d,
		Message{
			SessionID: "alpha-parent", Ordinal: 0, Role: "user",
			Timestamp: "2026-05-01T10:00:30Z",
		},
		Message{
			SessionID: "alpha-parent", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-05-01T10:01:00Z",
			Model:     "model-computed",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500,` +
					`"cache_creation_input_tokens":200,` +
					`"cache_read_input_tokens":300}`),
			HasContextTokens: true, HasOutputTokens: true,
		},
		Message{
			SessionID: "alpha-parent", Ordinal: 2, Role: "assistant",
			Timestamp: "2026-05-01T10:02:00Z",
			Model:     "model-reported",
			TokenUsage: jsontext.Value(
				`{"input_tokens":20,"output_tokens":10}`),
			HasContextTokens: true, HasOutputTokens: true,
		},
	)

	insertExportSession(t, d, Session{
		ID:               "alpha-child",
		Project:          "alpha",
		Machine:          "laptop",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T10:02:00Z"),
		EndedAt:          Ptr("2026-05-01T10:05:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
		ParentSessionID:  Ptr("alpha-parent"),
		RelationshipType: "subagent",
		IsAutomated:      true,
		Cwd:              "/repo/alpha/worktrees/feature/sub",
	})
	reported := money.MustParseDollars("0.0123")
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "alpha-child", []UsageEvent{{
		Source:          "provider",
		Model:           "model-reported",
		InputTokens:     200,
		OutputTokens:    40,
		ReasoningTokens: 7,
		Cost:            &reported,
		OccurredAt:      "2026-05-01T10:04:00Z",
	}}), "replace usage events")

	insertExportSession(t, d, Session{
		ID:               "beta-root",
		Project:          "beta",
		Machine:          "desktop",
		Agent:            "codex",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		EndedAt:          Ptr("2026-05-01T09:30:00Z"),
		MessageCount:     1,
		UserMessageCount: 2,
	})
	insertMessages(t, d, Message{
		SessionID: "beta-root", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-01T09:10:00Z",
		Model:     "model-reported",
		TokenUsage: jsontext.Value(
			`{"input_tokens":10,"output_tokens":20}`),
		HasContextTokens: true, HasOutputTokens: true,
	})

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{IncludeChildren: true},
		Limit:  10,
		Format: "json",
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Len(t, result.Rows, 3, "export rows")

	rows := sessionExportRowsByID(result.Rows)
	parent := rows["alpha-parent"]
	require.NotNil(t, parent.ModelUsage, "parent model usage")
	assert.Equal(t, "alpha-parent", parent.ID)
	assert.Equal(t, "alpha", parent.Project)
	assert.Equal(t, "laptop", parent.Machine)
	assert.Equal(t, "/repo/alpha/worktrees/feature/pkg", parent.Cwd)
	assert.Equal(t, "feature/export-contracts", parent.GitBranch)
	assert.Equal(t, 3, parent.MessageCount)
	assert.Equal(t, 1, parent.UserMessageCount)
	assert.Equal(t, 2, parent.AssistantMessageCount)
	assert.Equal(t, 1, parent.TurnCount)
	require.NotNil(t, parent.DurationSeconds, "duration_seconds")
	assert.Equal(t, int64(600), *parent.DurationSeconds)
	assert.Equal(t, export.SessionClassificationInteractive, parent.Classification)
	assert.False(t, parent.IsAutomated)
	assert.Nil(t, parent.ParentSessionID)
	assert.Nil(t, parent.RelationshipType)
	assert.Equal(t, []string{"model-computed", "model-reported"}, parent.ModelUsage.Models)
	assert.Equal(t, 1020, parent.ModelUsage.InputTokens)
	assert.Equal(t, 510, parent.ModelUsage.OutputTokens)
	assert.Equal(t, 200, parent.ModelUsage.CacheCreationInputTokens)
	assert.Equal(t, 300, parent.ModelUsage.CacheReadInputTokens)
	assert.True(t, parent.ModelUsage.HasCost)
	require.Contains(t, parent.ModelUsage.ByModel, "model-computed")
	assert.Equal(t, 1000, parent.ModelUsage.ByModel["model-computed"].InputTokens)
	assert.Equal(t, export.CostSourceComputed,
		parent.ModelUsage.ByModel["model-computed"].CostSource)
	require.Contains(t, parent.ModelUsage.ByModel, "model-reported")
	assert.Equal(t, 20, parent.ModelUsage.ByModel["model-reported"].InputTokens)
	assert.Equal(t, export.CostSourceComputed,
		parent.ModelUsage.ByModel["model-reported"].CostSource)
	require.NotNil(t, parent.Worktree, "parent worktree")
	assert.Equal(t, Ptr("feature"), parent.Worktree.Name)
	assert.Equal(t, Ptr("/repo/alpha"), parent.Worktree.RootPath)

	child := rows["alpha-child"]
	require.NotNil(t, child.ModelUsage, "child model usage")
	assert.Equal(t, export.SessionClassificationAutomated, child.Classification)
	assert.True(t, child.IsAutomated)
	require.NotNil(t, child.ParentSessionID, "child parent_session_id")
	assert.Equal(t, "alpha-parent", *child.ParentSessionID)
	require.NotNil(t, child.RelationshipType, "child relationship_type")
	assert.Equal(t, "subagent", *child.RelationshipType)
	assert.Equal(t, 7, child.ModelUsage.ReasoningTokens)
	assert.Equal(t, reported, child.ModelUsage.Cost)
	assert.True(t, child.ModelUsage.HasCost)
	require.NotNil(t, child.Worktree, "child worktree")
	assert.Equal(t, Ptr("feature"), child.Worktree.Name)
	assert.Equal(t, Ptr("/repo/alpha"), child.Worktree.RootPath)

	beta := rows["beta-root"]
	require.NotNil(t, beta.ModelUsage, "beta model usage")
	assert.Equal(t, "desktop", beta.Machine)
	assert.Nil(t, beta.Worktree)

	require.NotNil(t, result.Pricing, "pricing block")
	assert.ElementsMatch(t, []string{"model-computed", "model-reported"},
		sortedSetKeysFromMap(result.Pricing.Models))
	assert.Equal(t, export.CostSourceComputed,
		result.Pricing.Models["model-computed"].CostSource)
	assert.Equal(t, export.CostSourceMixed,
		result.Pricing.Models["model-reported"].CostSource)
	projectLabels := make([]string, 0, len(result.Projects))
	for key, project := range result.Projects {
		assert.NotContains(t, key, "alpha")
		assert.NotContains(t, key, "beta")
		projectLabels = append(projectLabels, project.DisplayLabel)
	}
	assert.ElementsMatch(t, []string{"alpha", "beta"}, projectLabels)
	assertContentFreeJSON(t, result)

	for _, row := range result.Rows {
		assertJSONHasKey(t, row, "parent_session_id")
		assertJSONHasKey(t, row, "relationship_type")
		assertContentFreeJSON(t, row)
	}
}

func TestSessionSummaryExportKeepsFirstConclusiveProjectIdentity(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	insertExportSession(t, d, Session{
		ID:               "stable-project-session",
		Project:          "/Users/alice/private/project",
		Machine:          "laptop",
		Agent:            "codex",
		Cwd:              "/Users/alice/private/project/pkg",
		StartedAt:        Ptr("2026-05-01T10:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:01:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	first := export.ProjectIdentityObservation{
		SessionID:            "stable-project-session",
		Project:              "/Users/alice/private/project",
		Machine:              "laptop",
		RootPath:             "/Users/alice/private/project",
		GitRemote:            "https://github.com/acme/project.git",
		GitRemoteName:        "origin",
		RepositoryPath:       "/Users/alice/private/project",
		WorktreeRootPath:     "/Users/alice/private/project",
		WorktreeRelationship: export.WorktreeMain,
		CheckoutState:        export.CheckoutBranch,
		GitBranch:            "main",
		ObservedAt:           time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, first))

	missingDirectoryRefresh := first
	missingDirectoryRefresh.RootPath = "/Users/alice/private/project/pkg"
	missingDirectoryRefresh.GitRemote = ""
	missingDirectoryRefresh.GitRemoteName = ""
	missingDirectoryRefresh.RemoteResolution = export.ProjectResolutionUnknown
	missingDirectoryRefresh.ObservedAt = first.ObservedAt.Add(time.Hour)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, missingDirectoryRefresh))

	changedRemoteRefresh := first
	changedRemoteRefresh.GitRemote = "https://gitlab.com/acme/other.git"
	changedRemoteRefresh.ObservedAt = first.ObservedAt.Add(2 * time.Hour)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, changedRemoteRefresh))

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.NotNil(t, result.Rows[0].ProjectReference.Identity)
	assert.Equal(t, "github.com/acme/project",
		result.Rows[0].ProjectReference.Identity.NormalizedRemote)
	assert.NotEmpty(t, result.Rows[0].ProjectReference.Identity.RootKey,
		"session row retains its immutable machine-local root")
	assert.Equal(t, export.WorktreeMain,
		result.Rows[0].ProjectReference.Worktree.Relationship)
	assert.Equal(t, export.CheckoutBranch,
		result.Rows[0].ProjectReference.Checkout.State)
	assert.Equal(t, "main", result.Rows[0].ProjectReference.Checkout.Branch)
	assert.Empty(t, result.Rows[0].ProjectReference.DisplayLabel)
	project := result.Projects[result.Rows[0].ProjectReference.ProjectKey]
	require.NotNil(t, project.Identity)
	assert.Empty(t, project.Identity.RootKey,
		"remote-backed catalog entries must not select a row-dependent root")

	payload, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "/Users/alice")
}

func TestSessionSummaryExportCatalogMarksConflictingSessionIdentitiesAmbiguous(
	t *testing.T,
) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	for i, remote := range []string{
		"https://github.com/acme/first.git",
		"https://github.com/acme/second.git",
	} {
		id := fmt.Sprintf("shared-project-%d", i)
		insertExportSession(t, d, Session{
			ID: id, Project: "shared-project", Machine: "laptop", Agent: "codex",
			StartedAt: Ptr(fmt.Sprintf("2026-05-01T10:0%d:00Z", i)),
			EndedAt:   Ptr(fmt.Sprintf("2026-05-01T10:0%d:30Z", i)),
		})
		require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: id, Project: "shared-project", Machine: "laptop",
				RootPath: "/workspace/shared-project", GitRemote: remote,
				GitRemoteName: "origin", RepositoryPath: "/workspace/shared-project",
				WorktreeRootPath:     "/workspace/shared-project",
				WorktreeRelationship: export.WorktreeMain,
				ObservedAt:           time.Date(2026, 5, 1, 10, i, 0, 0, time.UTC),
			},
		))
	}

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	require.Len(t, result.Projects, 1)
	for _, row := range result.Rows {
		require.NotNil(t, row.ProjectReference.Identity)
		assert.Equal(t, row.ProjectReference.ProjectKey,
			result.Rows[0].ProjectReference.ProjectKey)
	}
	for _, project := range result.Projects {
		assert.Equal(t, export.ProjectResolutionAmbiguous, project.Resolution)
		assert.Nil(t, project.Identity)
	}
}

func TestSessionSummaryExportKeepsConclusiveMachineRootSnapshot(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	insertExportSession(t, d, Session{
		ID: "machine-root-session", Project: "local-app", Machine: "laptop",
		Agent: "codex", Cwd: "/workspace/local-app",
		StartedAt: Ptr("2026-05-01T10:00:00Z"),
		EndedAt:   Ptr("2026-05-01T10:01:00Z"),
	})
	first := export.ProjectIdentityObservation{
		SessionID: "machine-root-session", Project: "local-app",
		Machine: "laptop", RootPath: "/workspace/local-app",
		RepositoryPath:   "/workspace/local-app",
		RemoteResolution: export.ProjectResolutionResolved,
		ObservedAt:       time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, first))
	changed := first
	changed.GitRemote = "https://github.com/acme/local-app.git"
	changed.ObservedAt = first.ObservedAt.Add(time.Hour)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, changed))

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.NotNil(t, result.Rows[0].ProjectReference.Identity)
	assert.Equal(t, export.ProjectKindMachineRoot,
		result.Rows[0].ProjectReference.Identity.Kind)
	assert.Empty(t, result.Rows[0].ProjectReference.Identity.NormalizedRemote)
}

func TestSessionSummaryExportWithoutSnapshotDoesNotDeriveFromCWD(t *testing.T) {
	d := testSessionExportDB(t)
	insertExportSession(t, d, Session{
		ID: "no-snapshot", Project: "app", Machine: "laptop",
		Agent: "codex", Cwd: "/workspace/app/private/subdir",
		StartedAt: Ptr("2026-05-01T10:00:00Z"),
		EndedAt:   Ptr("2026-05-01T10:01:00Z"),
	})

	result, err := d.ExportSessionSummaries(
		t.Context(), SessionExportOptions{Limit: 10},
	)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, export.ProjectResolutionUnknown,
		result.Rows[0].ProjectReference.Resolution)
	assert.Nil(t, result.Rows[0].ProjectReference.Identity)
}

func TestSessionSummaryExportIncludesReasoningOnlyUsageRows(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	seedSessionExportPricing(t, d)
	insertExportSession(t, d, Session{
		ID:        "reasoning-only",
		Project:   "alpha",
		Machine:   "laptop",
		Agent:     "codex",
		StartedAt: Ptr("2026-05-01T10:00:00Z"),
		EndedAt:   Ptr("2026-05-01T10:01:00Z"),
	})
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "reasoning-only", []UsageEvent{{
		Source:          "provider",
		Model:           "model-computed",
		ReasoningTokens: 25,
		OccurredAt:      "2026-05-01T10:00:30Z",
	}}), "replace usage events")

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  10,
		Format: "json",
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Len(t, result.Rows, 1)

	usage := result.Rows[0].ModelUsage
	require.NotNil(t, usage, "reasoning-only usage must contribute")
	assert.Equal(t, []string{"model-computed"}, usage.Models)
	assert.Zero(t, usage.InputTokens)
	assert.Zero(t, usage.OutputTokens)
	assert.Equal(t, 25, usage.ReasoningTokens)
	assert.True(t, usage.HasCost)
	assert.Equal(t, money.MustParseDollars("0.0005"), usage.Cost)
	require.Contains(t, usage.ByModel, "model-computed")
	assert.Equal(t, 25, usage.ByModel["model-computed"].ReasoningTokens)
	assert.Equal(t, export.CostSourceComputed,
		usage.ByModel["model-computed"].CostSource)

	require.NotNil(t, result.Pricing)
	require.Contains(t, result.Pricing.Models, "model-computed")
	assert.Equal(t, export.CostSourceComputed,
		result.Pricing.Models["model-computed"].CostSource)
}

func TestSessionSummaryExportIncludesMessageReasoningTokens(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	seedSessionExportPricing(t, d)
	insertExportSession(t, d, Session{
		ID:        "message-reasoning",
		Project:   "alpha",
		Machine:   "laptop",
		Agent:     "codex",
		StartedAt: Ptr("2026-05-01T10:00:00Z"),
		EndedAt:   Ptr("2026-05-01T10:01:00Z"),
	})
	insertMessages(t, d, Message{
		SessionID: "message-reasoning",
		Ordinal:   0,
		Role:      "assistant",
		Timestamp: "2026-05-01T10:00:30Z",
		Model:     "model-computed",
		TokenUsage: jsontext.Value(
			`{"input_tokens":10,"output_tokens":0,"reasoning_tokens":25}`),
	})

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  10,
		Format: "json",
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Len(t, result.Rows, 1)

	usage := result.Rows[0].ModelUsage
	require.NotNil(t, usage, "message reasoning usage must contribute")
	assert.Equal(t, []string{"model-computed"}, usage.Models)
	assert.Equal(t, 10, usage.InputTokens)
	assert.Zero(t, usage.OutputTokens)
	assert.Equal(t, 25, usage.ReasoningTokens)
	assert.True(t, usage.HasCost)
	assert.Equal(t, money.MustParseDollars("0.0006"), usage.Cost)
	require.Contains(t, usage.ByModel, "model-computed")
	assert.Equal(t, 25, usage.ByModel["model-computed"].ReasoningTokens)
}

func TestSessionSummaryExportRequiresExistingDatabaseID(t *testing.T) {
	d := testDB(t)
	insertExportSession(t, d, Session{
		ID:               "missing-db-id",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "codex",
		StartedAt:        Ptr("2026-05-01T10:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:01:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	_, err := d.rawWriter().ExecContext(t.Context(), `
		DELETE FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	)
	require.NoError(t, err)

	_, err = d.ExportSessionSummaries(t.Context(), SessionExportOptions{
		Limit: 10,
	})
	require.ErrorIs(t, err, ErrDatabaseIDMissing)
}

func TestSessionSummaryExportIdentitySurvivesRebuild(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	require.NoError(t, source.SetArchiveIdentityForTest(ctx, "archive-a", strings.Repeat("a", 64)))
	require.NoError(t, source.SetDatabaseIDForTest(ctx, "generation-one"))
	insertExportSession(t, source, Session{
		ID: "retained-session", Project: "project-a", UserMessageCount: 1,
		EndedAt: Ptr("2026-05-01T10:00:00Z"),
	})
	before, err := source.ExportSessionSummaries(ctx, SessionExportOptions{})
	require.NoError(t, err)
	require.Len(t, before.Rows, 1)
	require.NoError(t, source.CloseConnections(ctx))

	rebuilt := testDB(t)
	require.NoError(t, rebuilt.SetDatabaseIDForTest(ctx, "generation-two"))
	require.NoError(t, rebuilt.CopyArchiveIdentityFrom(source.Path()))
	count, err := rebuilt.CopyOrphanedDataFrom(source.Path())
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.NoError(t, rebuilt.CopySessionMetadataFrom(source.Path()))
	after, err := rebuilt.ExportSessionSummaries(ctx, SessionExportOptions{})
	require.NoError(t, err)
	require.Len(t, after.Rows, 1)
	assert.Equal(t, "archive-a", before.ArchiveID)
	assert.Equal(t, "archive-a", after.ArchiveID)
	assert.Equal(t, "generation-one", before.DatabaseID)
	assert.Equal(t, "generation-two", after.DatabaseID)
	assert.Equal(t, "retained-session", after.Rows[0].ID)
	assert.Equal(t, before.Rows[0].ProjectReference.ProjectKey, after.Rows[0].ProjectReference.ProjectKey)
}

func TestSessionSummaryExportEmptyArchiveRequiresIdentity(t *testing.T) {
	d := testDB(t)
	_, err := d.rawWriter().ExecContext(t.Context(), `DELETE FROM archive_metadata WHERE key = ?`, archiveMetadataArchiveIDKey)
	require.NoError(t, err)
	_, err = d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.ErrorIs(t, err, ErrArchiveIDMissing)
	var count int
	require.NoError(t, d.rawWriter().QueryRowContext(t.Context(), `SELECT count(*) FROM archive_metadata WHERE key = ?`,
		archiveMetadataArchiveIDKey).Scan(&count))
	assert.Zero(t, count, "export must not initialize missing identity")
}

func TestAllSessionExportIdentityUsesRowSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.SetArchiveIdentityForTest(ctx, "archive-before", strings.Repeat("a", 64)))
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "generation-before"))
	for _, id := range []string{"session-a", "session-b"} {
		insertExportSession(t, d, Session{
			ID: id, Project: "project-a", UserMessageCount: 1,
			EndedAt: Ptr("2026-05-01T10:00:00Z"),
		})
	}
	_, err := d.getWriter().Exec(ctx, `UPDATE sessions
		SET transcript_revision = '7', local_modified_at = '2026-05-01T10:01:00Z'`)
	require.NoError(t, err)
	pages, err := d.exportAllSessionSummaries(ctx, SessionExportOptions{Limit: 1}, func(page int, _ *sql.Tx) error {
		if page == 1 {
			if _, err := d.getWriter().Exec(ctx, `UPDATE sessions
				SET transcript_revision = '8', local_modified_at = '2026-05-01T10:02:00Z'`); err != nil {
				return err
			}
			if err := d.SetArchiveIdentityForTest(ctx, "archive-after", strings.Repeat("b", 64)); err != nil {
				return err
			}
			return d.SetDatabaseIDForTest(ctx, "generation-after")
		}
		return nil
	}, nil)
	require.NoError(t, err)
	require.Len(t, pages, 2)
	for _, page := range pages {
		assert.Equal(t, "archive-before", page.ArchiveID)
		assert.Equal(t, "generation-before", page.DatabaseID)
		require.Len(t, page.Rows, 1)
		assert.Equal(t, "7", page.Rows[0].TranscriptRevision)
		require.NotNil(t, page.Rows[0].LocalModifiedAt)
		assert.Equal(t, "2026-05-01T10:01:00Z", *page.Rows[0].LocalModifiedAt)
	}
	current, err := d.ExportSessionSummaries(ctx, SessionExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, "archive-after", current.ArchiveID)
	assert.Equal(t, "generation-after", current.DatabaseID)
	for _, row := range current.Rows {
		assert.Equal(t, "8", row.TranscriptRevision)
		require.NotNil(t, row.LocalModifiedAt)
		assert.Equal(t, "2026-05-01T10:02:00Z", *row.LocalModifiedAt)
	}
}

func TestAllSessionExportMaterializesActivitySort(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	d.rawReader().SetMaxOpenConns(1)
	d.rawReader().SetMaxIdleConns(1)

	project := "activity-cache"
	rootID := "activity-a"
	insertExportSession(t, d, Session{
		ID:        "activity-a",
		Project:   project,
		StartedAt: Ptr("2026-05-01T09:00:00Z"),
		EndedAt:   Ptr("2026-05-01T10:00:00-04:00"),
	})
	insertExportSession(t, d, Session{
		ID:        "activity-b",
		Project:   project,
		StartedAt: Ptr("2026-05-01T09:00:00Z"),
		EndedAt:   Ptr("2026-05-01T14:00:00Z"),
	})
	insertExportSession(t, d, Session{
		ID:        "activity-missing-ended",
		Project:   project,
		StartedAt: Ptr("2026-05-01T09:00:00Z"),
	})
	insertMessages(t, d, Message{
		SessionID: "activity-missing-ended",
		Ordinal:   0,
		Role:      "user",
		Content:   "activity",
		Timestamp: "2026-05-01T14:00:00+00:00",
	})

	for _, session := range []Session{
		{
			ID:               "child-subagent",
			Project:          project,
			StartedAt:        Ptr("2026-05-01T12:00:00Z"),
			EndedAt:          Ptr("2026-05-01T13:00:00Z"),
			ParentSessionID:  &rootID,
			RelationshipType: "subagent",
		},
		{
			ID:               "child-fork",
			Project:          project,
			StartedAt:        Ptr("2026-05-01T11:00:00Z"),
			EndedAt:          Ptr("2026-05-01T12:00:00Z"),
			ParentSessionID:  &rootID,
			RelationshipType: "fork",
		},
		{
			ID:               "child-continuation",
			Project:          project,
			StartedAt:        Ptr("2026-05-01T10:00:00Z"),
			EndedAt:          Ptr("2026-05-01T11:00:00Z"),
			ParentSessionID:  &rootID,
			RelationshipType: "continuation",
		},
		{
			ID:              "imported-migrated",
			Project:         project,
			StartedAt:       Ptr("2026-05-01T12:00:00Z"),
			EndedAt:         Ptr("2026-05-01T12:30:00Z"),
			SourceSessionID: "remote-session",
			SourceVersion:   "remote-v1",
		},
		{
			ID:        "source-missing",
			Project:   project,
			StartedAt: Ptr("2026-05-01T10:00:00Z"),
			EndedAt:   Ptr("2026-05-01T11:30:00Z"),
		},
		{
			ID:        "trashed",
			Project:   project,
			StartedAt: Ptr("2026-05-01T15:00:00Z"),
			EndedAt:   Ptr("2026-05-01T16:00:00Z"),
		},
		{
			ID:        "tombstoned",
			Project:   project,
			StartedAt: Ptr("2026-05-01T17:00:00Z"),
			EndedAt:   Ptr("2026-05-01T18:00:00Z"),
		},
	} {
		insertExportSession(t, d, session)
	}
	require.NoError(t, d.SoftDeleteSession(ctx, "trashed"), "soft-delete trashed session")
	require.NoError(t, d.DeleteSession(ctx, "tombstoned"), "tombstone session")
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions SET source_missing_at = ? WHERE id = ?`,
		"2026-05-01T19:00:00Z", "source-missing",
	)
	require.NoError(t, err, "mark source-missing session")

	filter := SessionFilter{Project: project, IncludeChildren: true}
	expectedIDs := []string{
		"activity-a",
		"activity-b",
		"activity-missing-ended",
		"child-subagent",
		"imported-migrated",
		"child-fork",
		"source-missing",
		"child-continuation",
	}
	var tableSQL, indexSQL string
	var populationCounts []int
	var watermarkQuery, firstPageQuery, laterPageQuery string
	var watermarkPlan, firstPagePlan, laterPagePlan []string
	var observedQueries []struct {
		query string
		args  []any
	}
	// Observe executed SQL so the plan assertions cannot drift from production.
	observe := func(query string, args []any) {
		observedQueries = append(observedQueries, struct {
			query string
			args  []any
		}{query: query, args: args})
	}
	explainPlan := func(tx *sql.Tx, query string, queryArgs []any) []string {
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, queryArgs...)
		require.NoError(t, err, "explain session export query")
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail),
				"scan session export query plan")
			details = append(details, detail)
		}
		require.NoError(t, rows.Err(), "iterate session export query plan")
		return details
	}
	assertNoActivityTable := func() {
		var count int
		require.NoError(t, d.rawReader().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_temp_master
			 WHERE type = 'table' AND name = ?`, sessionExportActivityTable,
		).Scan(&count), "inspect reader temp schema after export")
		assert.Zero(t, count, "activity table must not survive the export")
	}

	pages, err := d.exportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: filter,
		Limit:  1,
	}, func(page int, tx *sql.Tx) error {
		var currentTableSQL, currentIndexSQL string
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_temp_master
			 WHERE type = 'table' AND name = ?`, sessionExportActivityTable,
		).Scan(&currentTableSQL), "inspect activity table")
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_temp_master
			 WHERE type = 'index' AND name = ?`, sessionExportActivityIndex,
		).Scan(&currentIndexSQL), "inspect activity index")
		var population int
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+sessionExportActivityTable,
		).Scan(&population), "count materialized activity rows")
		populationCounts = append(populationCounts, population)
		if page == 1 {
			tableSQL = currentTableSQL
			indexSQL = currentIndexSQL
			require.Len(t, observedQueries, 2,
				"watermark and first page must be observed")
			watermarkQuery = observedQueries[0].query
			firstPageQuery = observedQueries[1].query
			watermarkPlan = explainPlan(
				tx, watermarkQuery, observedQueries[0].args,
			)
			firstPagePlan = explainPlan(
				tx, firstPageQuery, observedQueries[1].args,
			)
		}
		if page == 2 {
			require.Len(t, observedQueries, 3,
				"later page must be observed")
			laterPageQuery = observedQueries[2].query
			laterPagePlan = explainPlan(
				tx, laterPageQuery, observedQueries[2].args,
			)
		}
		require.Equal(t, len(expectedIDs), population,
			"one populated activity table must serve every page")
		require.Equal(t, tableSQL, currentTableSQL,
			"all pages must use one temp table")
		require.Equal(t, indexSQL, currentIndexSQL,
			"all pages must use one temp index")
		return nil
	}, observe)
	require.NoError(t, err, "materialized all-session export")
	assertNoActivityTable()
	require.Len(t, pages, len(expectedIDs), "one page per session")
	var gotIDs []string
	var gotActivities []string
	for _, page := range pages {
		gotIDs = append(gotIDs, sessionExportRowIDs(page.Rows)...)
		for _, row := range page.Rows {
			gotActivities = append(gotActivities, row.LastActivityAt)
		}
	}
	assert.Equal(t, expectedIDs, gotIDs)
	assert.Equal(t, []string{
		"2026-05-01T10:00:00-04:00",
		"2026-05-01T14:00:00Z",
		"2026-05-01T14:00:00+00:00",
		"2026-05-01T13:00:00Z",
		"2026-05-01T12:30:00Z",
		"2026-05-01T12:00:00Z",
		"2026-05-01T11:30:00Z",
		"2026-05-01T11:00:00Z",
	}, gotActivities)
	assert.Equal(t, []int{len(expectedIDs)}, slices.Compact(populationCounts))
	require.Contains(t, tableSQL, sessionExportActivityTable)
	require.Contains(t, tableSQL, "last_activity_at")
	require.Contains(t, tableSQL, "last_activity_sort")
	require.Contains(t, indexSQL, sessionExportActivityIndex)
	require.Contains(t, indexSQL, "last_activity_sort DESC, id ASC")
	for _, query := range []string{
		watermarkQuery, firstPageQuery, laterPageQuery,
	} {
		require.Contains(t, query, sessionExportActivityTable)
		require.Contains(t, query, sessionExportActivityIndex)
		require.NotContains(t, query, sessionExportLastActivityExpr())
		require.NotContains(t, query, sessionExportLastActivitySortExpr())
	}
	planText := func(details []string) string {
		return strings.ToLower(strings.Join(details, " | "))
	}
	for _, plan := range [][]string{
		watermarkPlan, firstPagePlan, laterPagePlan,
	} {
		require.Contains(t, planText(plan),
			strings.ToLower("USING INDEX "+sessionExportActivityIndex),
			"materialized query must use the activity sort index")
	}
	t.Logf("activity table SQL: %s", strings.Join(strings.Fields(tableSQL), " "))
	t.Logf("activity index SQL: %s", strings.Join(strings.Fields(indexSQL), " "))
	t.Logf("watermark SQL: %s", strings.Join(strings.Fields(watermarkQuery), " "))
	t.Logf("watermark plan: %s", strings.Join(watermarkPlan, " | "))
	t.Logf("first page SQL: %s", strings.Join(strings.Fields(firstPageQuery), " "))
	t.Logf("first page plan: %s", strings.Join(firstPagePlan, " | "))
	t.Logf("later page SQL: %s", strings.Join(strings.Fields(laterPageQuery), " "))
	t.Logf("later page plan: %s", strings.Join(laterPagePlan, " | "))

	expression, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: filter,
		Limit:  len(expectedIDs),
	})
	require.NoError(t, err, "expression-based single-page export")
	assertNoActivityTable()
	assert.Equal(t, expectedIDs, sessionExportRowIDs(expression.Rows))
	for i, row := range expression.Rows {
		assert.Equal(t, gotActivities[i], row.LastActivityAt)
	}

	firstCursor := pages[0].NextCursor
	require.NotEmpty(t, firstCursor, "first page cursor")
	resumed, err := d.ExportAllSessionSummaries(ctx, SessionExportOptions{
		Cursor:          firstCursor,
		UseCursorFilter: true,
		Limit:           1,
	})
	require.NoError(t, err, "cursor-owned all-session export")
	assertNoActivityTable()
	var resumedIDs []string
	for _, page := range resumed {
		resumedIDs = append(resumedIDs, sessionExportRowIDs(page.Rows)...)
	}
	assert.Equal(t, expectedIDs[1:], resumedIDs)

	filteredPages, err := d.exportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: filter,
		Limit:  1,
	}, func(page int, tx *sql.Tx) error {
		if page == 1 {
			var count int
			require.NoError(t, tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM `+sessionExportActivityTable,
			).Scan(&count), "count filtered activity rows")
			require.Equal(t, len(expectedIDs), count)
		}
		return nil
	}, nil)
	require.NoError(t, err, "filtered all-session export")
	assertNoActivityTable()
	assert.Equal(t, expectedIDs, func() []string {
		var ids []string
		for _, page := range filteredPages {
			ids = append(ids, sessionExportRowIDs(page.Rows)...)
		}
		return ids
	}())

	emptyPages, err := d.exportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "no-such-project"},
		Limit:  1,
	}, func(page int, tx *sql.Tx) error {
		require.Equal(t, 1, page)
		var count int
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+sessionExportActivityTable,
		).Scan(&count), "count empty activity rows")
		assert.Zero(t, count)
		return nil
	}, nil)
	require.NoError(t, err, "empty all-session export")
	assertNoActivityTable()
	require.Len(t, emptyPages, 1)
	assert.Empty(t, emptyPages[0].Rows)

	callbackErr := errors.New("after-page callback failed")
	_, err = d.exportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: filter,
		Limit:  1,
	}, func(int, *sql.Tx) error {
		return callbackErr
	}, nil)
	require.ErrorIs(t, err, callbackErr)
	assertNoActivityTable()

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = d.exportAllSessionSummaries(cancelCtx, SessionExportOptions{
		Filter: filter,
		Limit:  1,
	}, func(page int, _ *sql.Tx) error {
		if page == 1 {
			cancel()
		}
		return nil
	}, nil)
	require.ErrorIs(t, err, context.Canceled)
	// A cancelled reader must allow a fresh export to open cleanly.
	_, err = d.ExportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: filter,
		Limit:  1,
	})
	require.NoError(t, err, "export after cancelled transaction")
	assertNoActivityTable()
}

func TestSessionSummaryExportUsesMessageActivityForOpenSessions(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "open-active",
		Project:          "activity",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T10:00:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	})
	insertMessages(t, d,
		Message{
			SessionID: "open-active",
			Ordinal:   0,
			Role:      "user",
			Timestamp: "2026-05-01T10:00:00Z",
		},
		Message{
			SessionID: "open-active",
			Ordinal:   1,
			Role:      "assistant",
			Timestamp: "2026-05-01T10:20:00Z",
		},
	)
	insertExportSession(t, d, Session{
		ID:               "closed-stale",
		Project:          "activity",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:10:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "activity"},
		Limit:  1,
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"open-active"}, sessionExportRowIDs(first.Rows))
	row := first.Rows[0]
	assert.Equal(t, "2026-05-01T10:20:00Z", row.LastActivityAt)
	require.NotNil(t, row.DurationSeconds, "open session duration_seconds")
	assert.Equal(t, int64(1200), *row.DurationSeconds)

	payload, err := d.decodeSessionExportCursor(first.NextCursor)
	require.NoError(t, err, "decode cursor")
	assert.Equal(t, "2026-05-01T10:20:00Z", payload.Watermark)
	assert.Equal(t, "2026-05-01T10:20:00Z", payload.LastActivityAt)

	second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "activity"},
		Cursor: first.NextCursor,
		Limit:  1,
	})
	require.NoError(t, err, "second page")
	assert.Equal(t, []string{"closed-stale"}, sessionExportRowIDs(second.Rows))
}

func TestSessionSummaryExportOrdersLastActivityByParsedInstant(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "whole-second",
		Project:          "activity-sort",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T10:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:00:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	insertExportSession(t, d, Session{
		ID:               "fractional-later",
		Project:          "activity-sort",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T10:00:00.123Z"),
		EndedAt:          Ptr("2026-05-01T10:00:00.123Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "activity-sort"},
		Limit:  1,
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"fractional-later"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, "2026-05-01T10:00:00.123Z", first.Rows[0].LastActivityAt)

	second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "activity-sort"},
		Cursor: first.NextCursor,
		Limit:  1,
	})
	require.NoError(t, err, "second page")
	require.Equal(t, []string{"whole-second"}, sessionExportRowIDs(second.Rows))
	assert.Equal(t, "2026-05-01T10:00:00Z", second.Rows[0].LastActivityAt)
}

func TestSessionSummaryExportClosedSessionActivityPrefersEndedAt(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "closed-with-late-message",
		Project:          "activity",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:10:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	})
	insertMessages(t, d,
		Message{
			SessionID: "closed-with-late-message",
			Ordinal:   0,
			Role:      "user",
			Timestamp: "2026-05-01T09:00:00Z",
		},
		Message{
			SessionID: "closed-with-late-message",
			Ordinal:   1,
			Role:      "assistant",
			Timestamp: "2026-05-01T10:30:00Z",
		},
	)

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "activity"},
		Limit:  10,
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Len(t, result.Rows, 1)
	row := result.Rows[0]
	assert.Equal(t, "2026-05-01T10:10:00Z", row.LastActivityAt)
	require.NotNil(t, row.DurationSeconds, "duration_seconds")
	assert.Equal(t, int64(4200), *row.DurationSeconds)
}

func TestSessionSummaryExportActiveSinceUsesMessageAwareActivity(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "open-active-after-cutoff",
		Project:          "activity-filter",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	})
	insertMessages(t, d,
		Message{
			SessionID: "open-active-after-cutoff",
			Ordinal:   0,
			Role:      "user",
			Timestamp: "2026-05-01T09:00:00Z",
		},
		Message{
			SessionID: "open-active-after-cutoff",
			Ordinal:   1,
			Role:      "assistant",
			Timestamp: "2026-05-01T10:30:00Z",
		},
	)
	insertExportSession(t, d, Session{
		ID:               "open-inactive-before-cutoff",
		Project:          "activity-filter",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:     "activity-filter",
			ActiveSince: "2026-05-01T10:00:00Z",
		},
		Limit: 10,
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Equal(t, []string{"open-active-after-cutoff"}, sessionExportRowIDs(result.Rows))
	assert.Equal(t, "2026-05-01T10:30:00Z", result.Rows[0].LastActivityAt)
}

func TestSessionSummaryExportActiveSinceIncludeChildrenKeepsOlderChildrenOfActiveRoot(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "active-root",
		Project:          "activity-tree",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	})
	insertMessages(t, d,
		Message{
			SessionID: "active-root",
			Ordinal:   0,
			Role:      "user",
			Timestamp: "2026-05-01T09:00:00Z",
		},
		Message{
			SessionID: "active-root",
			Ordinal:   1,
			Role:      "assistant",
			Timestamp: "2026-05-01T10:30:00Z",
		},
	)
	insertExportSession(t, d, Session{
		ID:               "older-child",
		Project:          "activity-tree",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:10:00Z"),
		EndedAt:          Ptr("2026-05-01T09:20:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
		ParentSessionID:  Ptr("active-root"),
		RelationshipType: "subagent",
	})

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:         "activity-tree",
			ActiveSince:     "2026-05-01T10:00:00Z",
			IncludeChildren: true,
		},
		Limit: 10,
	})
	require.NoError(t, err, "ExportSessionSummaries")
	assert.Equal(t, []string{"active-root", "older-child"},
		sessionExportRowIDs(result.Rows))
}

func TestSessionSummaryExportActiveSinceIncludeChildrenDoesNotPromoteActiveChildOfInactiveRoot(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	insertExportSession(t, d, Session{
		ID:               "inactive-root",
		Project:          "activity-tree",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:00:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	insertExportSession(t, d, Session{
		ID:               "active-child",
		Project:          "activity-tree",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T09:10:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
		ParentSessionID:  Ptr("inactive-root"),
		RelationshipType: "subagent",
	})
	insertMessages(t, d,
		Message{
			SessionID: "active-child",
			Ordinal:   0,
			Role:      "user",
			Timestamp: "2026-05-01T09:10:00Z",
		},
		Message{
			SessionID: "active-child",
			Ordinal:   1,
			Role:      "assistant",
			Timestamp: "2026-05-01T10:30:00Z",
		},
	)

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:         "activity-tree",
			ActiveSince:     "2026-05-01T10:00:00Z",
			IncludeChildren: true,
		},
		Limit: 10,
	})
	require.NoError(t, err, "ExportSessionSummaries")
	assert.Empty(t, result.Rows)
}

func TestSessionSummaryExportDoesNotFallbackWorktreeWithoutPathMatch(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:          "alpha",
			Machine:          "local",
			RootPath:         "/repo/alpha/worktrees/feature",
			GitRemote:        "https://github.com/acme/alpha.git",
			GitRemoteName:    "origin",
			WorktreeName:     "feature",
			WorktreeRootPath: "/repo/alpha/worktrees/feature",
			ObservedAt:       time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}), "upsert project identity")
	insertExportSession(t, d, Session{
		ID:               "outside-worktree",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/tmp/outside-alpha",
		StartedAt:        Ptr("2026-05-01T10:00:00Z"),
		EndedAt:          Ptr("2026-05-01T10:01:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  10,
	})
	require.NoError(t, err, "ExportSessionSummaries")
	require.Len(t, result.Rows, 1)
	assert.Nil(t, result.Rows[0].Worktree)
}

func TestSessionSummaryExportDefaultLimitIsMaxSessionLimit(t *testing.T) {
	d := testSessionExportDB(t)
	for i := range MaxSessionLimit + 1 {
		insertExportSession(t, d, Session{
			ID:               "default-limit-" + sortableInt(i),
			Project:          "limit",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr("2026-05-01T00:00:00Z"),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}

	result, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.NoError(t, err, "ExportSessionSummaries")
	assert.Len(t, result.Rows, MaxSessionLimit)
	assert.NotEmpty(t, result.NextCursor)
}

func TestSessionExportCursorEmbedsSnapshotAndPaginatesStably(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "cursor-db"),
		"set database id")

	for _, row := range []struct {
		id, ended string
	}{
		{"page-a", "2026-05-01T10:00:00Z"},
		{"page-b", "2026-05-01T09:00:00Z"},
		{"page-c", "2026-05-01T08:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "alpha",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	insertExportSession(t, d, Session{
		ID:               "other-project",
		Project:          "beta",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T00:00:00Z"),
		EndedAt:          Ptr("2026-05-01T07:00:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  2,
		Format: "json",
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"page-a", "page-b"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)

	payload, err := d.decodeSessionExportCursor(first.NextCursor)
	require.NoError(t, err, "decode export cursor")
	assert.Equal(t, "cursor-db", payload.DatabaseID)
	assert.Equal(t, "alpha", payload.Filters.Project)
	assert.Equal(t, "last_activity_at DESC, id ASC", payload.Order)
	assert.NotEmpty(t, payload.Watermark)
	assert.Equal(t, "2026-05-01T09:00:00Z", payload.LastActivityAt)
	assert.Equal(t, "page-b", payload.LastID)
	assert.Equal(t, 2, payload.Limit)
	assert.Equal(t, 3, payload.SnapshotCount)
	assert.NotEmpty(t, payload.SnapshotDigest)
	assert.Equal(t, 2, payload.PrefixCount)
	assert.NotEmpty(t, payload.PrefixDigest)

	second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Cursor: first.NextCursor,
		Limit:  10,
		Format: "json",
	})
	require.NoError(t, err, "second page")
	require.Equal(t, []string{"page-c"}, sessionExportRowIDs(second.Rows))

	insertExportSession(t, d, Session{
		ID:               "insert-under-watermark-after-cursor",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T00:00:00Z"),
		EndedAt:          Ptr("2026-05-01T08:30:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	insertExportSession(t, d, Session{
		ID:               "insert-above-watermark",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2099-01-01T00:00:00Z"),
		EndedAt:          Ptr("2099-01-01T00:00:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	_, err = d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Cursor: first.NextCursor,
		Limit:  10,
		Format: "json",
	})
	require.Error(t, err, "changed watermarked set should reset cursor")
	assert.ErrorIs(t, err, ErrSessionExportCursorReset,
		"expected reset error, got %v", err)
}

func TestSessionExportCursorPrefixUsesSameSnapshotAsPageQuery(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()

	for _, row := range []struct {
		id, ended string
	}{
		{"page-a", "2026-05-01T10:00:00Z"},
		{"page-b", "2026-05-01T09:00:00Z"},
		{"page-c", "2026-05-01T08:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "snapshot",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}

	where, args := buildSessionExportFilterForAlias(
		SessionFilter{Project: "snapshot"}, "",
	)
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err, "begin read snapshot")
	defer func() { require.NoError(t, tx.Rollback(), "rollback read snapshot") }()

	_, watermarkSort, err := d.sessionExportWatermarkFrom(
		ctx, tx, where, args, sessionExportActivitySource{},
	)
	require.NoError(t, err, "snapshot watermark")
	rows, err := d.querySessionExportRowsFrom(
		ctx, tx, where, args, watermarkSort, sessionExportCursorPayload{}, 2,
		sessionExportActivitySource{},
	)
	require.NoError(t, err, "snapshot page")
	require.Len(t, rows, 3, "page query returns limit plus one")
	emittedRows := rows[:2]
	require.Equal(t, []string{"page-a", "page-b"},
		sessionExportRowIDs(emittedRows))

	last := emittedRows[len(emittedRows)-1]
	snapshotCount, snapshotDigest, err := d.sessionExportPrefixFingerprint(
		ctx, tx, where, args, watermarkSort, last.lastActivitySort, last.ID)
	require.NoError(t, err, "snapshot prefix before mutation")
	require.Equal(t, 2, snapshotCount)

	insertExportSession(t, d, Session{
		ID:               "page-c",
		Project:          "snapshot",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T00:00:00Z"),
		EndedAt:          Ptr("2026-05-01T09:30:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})

	liveCount, _, err := d.sessionExportPrefixFingerprint(
		ctx, d.getReader(), where, args, watermarkSort, last.lastActivitySort, last.ID)
	require.NoError(t, err, "live prefix after mutation")
	assert.Equal(t, 3, liveCount,
		"live reads now include the row that moved before the emitted cursor")

	afterCount, afterDigest, err := d.sessionExportPrefixFingerprint(
		ctx, tx, where, args, watermarkSort, last.lastActivitySort, last.ID)
	require.NoError(t, err, "snapshot prefix after mutation")
	assert.Equal(t, snapshotCount, afterCount)
	assert.Equal(t, snapshotDigest, afterDigest)
}

func TestSessionExportUsageUsesPageReadSnapshot(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	seedSessionExportPricing(t, d)
	insertExportSession(t, d, Session{
		ID: "usage-snapshot", Project: "alpha", Machine: "local",
	})
	insertMessages(t, d, Message{
		SessionID: "usage-snapshot", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-05-01T10:00:00Z", Model: "model-computed",
		TokenUsage:       jsontext.Value(`{"input_tokens":100,"output_tokens":10}`),
		HasContextTokens: true,
		HasOutputTokens:  true,
	})

	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err, "begin page snapshot")
	defer func() { require.NoError(t, tx.Rollback(), "rollback page snapshot") }()
	var messageCount int
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE session_id = ?`,
		"usage-snapshot").Scan(&messageCount), "establish page snapshot")
	require.Equal(t, 1, messageCount)

	_, err = d.getWriter().ExecContext(ctx, `
		UPDATE messages
		SET token_usage = '{"input_tokens":900,"output_tokens":90}'
		WHERE session_id = ?`, "usage-snapshot")
	require.NoError(t, err, "mutate live usage after page snapshot")

	rows := []SessionSummaryRow{{ID: "usage-snapshot"}}
	_, err = d.attachSessionExportUsage(ctx, tx, rows)
	require.NoError(t, err, "attach usage from page snapshot")
	require.NotNil(t, rows[0].ModelUsage)
	assert.Equal(t, 100, rows[0].ModelUsage.InputTokens)
	assert.Equal(t, 10, rows[0].ModelUsage.OutputTokens)
}

func TestSessionSummaryExportSelectsCompleteClaudeSnapshotBeforeDedup(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	seedSessionExportPricing(t, d)
	for _, session := range []Session{
		{
			ID: "a-parent", Project: "snapshot", Machine: "local",
			Agent: "claude", StartedAt: Ptr("2026-05-01T10:00:00Z"),
		},
		{
			ID: "z-child", Project: "snapshot", Machine: "local",
			Agent: "claude", StartedAt: Ptr("2026-05-01T10:01:00Z"),
		},
	} {
		insertExportSession(t, d, session)
	}
	insertMessages(t, d,
		Message{
			SessionID: "a-parent", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:00:00Z", Model: "model-computed",
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":2}}`),
		},
		Message{
			SessionID: "z-child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:01:00Z", Model: "model-computed",
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":100}`),
		},
	)

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "snapshot"}, Limit: 10, Format: "json",
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)
	rows := sessionExportRowsByID(result.Rows)

	parent := rows["a-parent"].ModelUsage
	require.NotNil(t, parent)
	assert.Equal(t, 1000, parent.InputTokens)
	assert.Equal(t, 100, parent.OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 32_000}, parent.Cost)
	assert.True(t, parent.HasCost)
	require.Contains(t, parent.ByModel, "model-computed")
	assert.Equal(t, money.Money{Microdollars: 32_000},
		parent.ByModel["model-computed"].Cost)

	child := rows["z-child"].ModelUsage
	require.NotNil(t, child)
	assert.Zero(t, child.InputTokens)
	assert.Zero(t, child.OutputTokens)
	assert.Zero(t, child.Cost.Microdollars)
	assert.False(t, child.HasCost)
}

func TestSessionSummaryExportSelectsClaudeSnapshotFromExcludedSubagent(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	seedSessionExportPricing(t, d)
	parentID := "snapshot-parent"
	insertExportSession(t, d, Session{
		ID: parentID, Project: "snapshot-excluded", Machine: "local",
		Agent: "claude", StartedAt: Ptr("2026-05-01T10:00:00Z"),
		EndedAt: Ptr("2026-05-01T10:02:00Z"),
	})
	insertExportSession(t, d, Session{
		ID: "snapshot-child", Project: "snapshot-excluded", Machine: "local",
		Agent: "claude", StartedAt: Ptr("2026-05-01T10:01:00Z"),
		EndedAt: Ptr("2026-05-01T10:02:00Z"), ParentSessionID: &parentID,
		RelationshipType: "subagent",
	})
	insertMessages(t, d,
		Message{
			SessionID: parentID, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:00:00Z", Model: "model-computed",
			ClaudeMessageID: "excluded-message", ClaudeRequestID: "excluded-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":2}}`),
		},
		Message{
			SessionID: "snapshot-child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:01:00Z", Model: "model-computed",
			ClaudeMessageID: "excluded-message", ClaudeRequestID: "excluded-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":100}`),
		},
	)

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "snapshot-excluded"},
		Limit:  10,
		Format: "json",
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, parentID, result.Rows[0].ID)
	usage := result.Rows[0].ModelUsage
	require.NotNil(t, usage)
	assert.Equal(t, 1000, usage.InputTokens)
	assert.Equal(t, 100, usage.OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 32_000}, usage.Cost)
	assert.True(t, usage.HasCost)
}

func TestSessionSummaryExportSelectsClaudeSnapshotAcrossPages(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	seedSessionExportPricing(t, d)
	for _, session := range []Session{
		{
			ID: "snapshot-page-parent", Project: "snapshot-pages", Machine: "local",
			Agent: "claude", StartedAt: Ptr("2026-05-01T10:00:00Z"),
			EndedAt: Ptr("2026-05-01T12:00:00Z"),
		},
		{
			ID: "snapshot-page-peer", Project: "snapshot-pages", Machine: "local",
			Agent: "claude", StartedAt: Ptr("2026-05-01T10:01:00Z"),
			EndedAt: Ptr("2026-05-01T11:00:00Z"),
		},
	} {
		insertExportSession(t, d, session)
	}
	insertMessages(t, d,
		Message{
			SessionID: "snapshot-page-parent", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:00:00Z", Model: "model-computed",
			ClaudeMessageID: "paged-message", ClaudeRequestID: "paged-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":100,"output_tokens":5,` +
					`"server_tool_use":{"web_search_requests":2}}`),
		},
		Message{
			SessionID: "snapshot-page-peer", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-05-01T10:01:00Z", Model: "model-computed",
			ClaudeMessageID: "paged-message", ClaudeRequestID: "paged-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":100}`),
		},
	)

	pages, err := d.ExportAllSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "snapshot-pages"}, Limit: 1, Format: "json",
	})
	require.NoError(t, err)
	require.Len(t, pages, 2)
	require.Len(t, pages[0].Rows, 1)
	require.Len(t, pages[1].Rows, 1)
	assert.Equal(t, "snapshot-page-parent", pages[0].Rows[0].ID)
	assert.Equal(t, "snapshot-page-peer", pages[1].Rows[0].ID)

	parentUsage := pages[0].Rows[0].ModelUsage
	require.NotNil(t, parentUsage)
	assert.Equal(t, 1000, parentUsage.InputTokens)
	assert.Equal(t, 100, parentUsage.OutputTokens)
	assert.Equal(t, money.Money{Microdollars: 32_000}, parentUsage.Cost)
	assert.True(t, parentUsage.HasCost)

	peerUsage := pages[1].Rows[0].ModelUsage
	require.NotNil(t, peerUsage)
	assert.Zero(t, peerUsage.InputTokens)
	assert.Zero(t, peerUsage.OutputTokens)
	assert.Zero(t, peerUsage.Cost.Microdollars)
	assert.False(t, peerUsage.HasCost)
}

type capturedSessionExportQuery struct {
	query string
	args  []any
}

type sessionExportQueryCapture struct {
	inner   sessionExportQuerier
	queries []capturedSessionExportQuery
}

func (c *sessionExportQueryCapture) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	c.queries = append(c.queries, capturedSessionExportQuery{
		query: query,
		args:  append([]any(nil), args...),
	})
	return c.inner.QueryContext(ctx, query, args...)
}

func (c *sessionExportQueryCapture) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	return c.inner.QueryRowContext(ctx, query, args...)
}

func TestSessionExportClaudeSnapshotPeersUsesSnapshotIndex(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	const pairCount = maxSQLVars/2 + 1

	pageRows := make([]usageScanRow, 0, pairCount)
	for i := range pairCount {
		pageRows = append(pageRows, usageScanRow{
			sessionID:       fmt.Sprintf("page-%03d", i),
			claudeMessageID: fmt.Sprintf("message-%03d", i),
			claudeRequestID: fmt.Sprintf("request-%03d", i),
		})
	}
	pageRows[0].sessionID = "page-owned"

	for _, id := range []string{
		"page-owned", "peer-first", "peer-last", "cross-pair", "unmatched",
		"empty-message", "empty-request",
	} {
		insertExportSession(t, d, Session{
			ID: id, Project: "snapshot-index", Machine: "local", Agent: "claude",
			StartedAt: Ptr("2026-05-01T10:00:00Z"),
			EndedAt:   Ptr("2026-05-01T10:01:00Z"),
		})
	}
	message := func(sessionID, messageID, requestID string) Message {
		return Message{
			SessionID:       sessionID,
			Ordinal:         0,
			Role:            "assistant",
			Timestamp:       "2026-05-01T10:00:00Z",
			Model:           "model-computed",
			ClaudeMessageID: messageID,
			ClaudeRequestID: requestID,
			TokenUsage:      jsontext.Value(`{"input_tokens":1,"output_tokens":1}`),
		}
	}
	insertMessages(t, d,
		message("page-owned", "message-000", "request-000"),
		message("peer-first", "message-001", "request-001"),
		message("peer-last", "message-250", "request-250"),
		message("cross-pair", "message-001", "request-002"),
		message("unmatched", "message-outside", "request-outside"),
		message("empty-message", "", "request-001"),
		message("empty-request", "message-001", ""),
	)

	capture := &sessionExportQueryCapture{inner: d.getReader()}
	peers, err := sessionExportClaudeSnapshotPeers(
		ctx, capture, pageRows, []string{"page-owned"},
	)
	require.NoError(t, err)
	require.Len(t, capture.queries, 2)
	require.Len(t, capture.queries[0].args, maxSQLVars)
	require.Len(t, capture.queries[1].args, 2)

	for i, captured := range capture.queries {
		assert.Contains(t, captured.query, "m.claude_message_id != ''")
		assert.Contains(t, captured.query, "m.claude_request_id != ''")
		assert.Contains(t, captured.query,
			"(m.claude_message_id, m.claude_request_id) IN (VALUES")

		planRows, planErr := d.getReader().QueryContext(
			ctx, "EXPLAIN QUERY PLAN "+captured.query, captured.args...,
		)
		require.NoError(t, planErr)
		defer planRows.Close()
		details := explainQueryPlanDetails(t, planRows)
		plan := strings.Join(details, "\n")
		t.Logf("captured production query %d: args=%d plan=%s",
			i+1, len(captured.args), strings.Join(details, " | "))
		assert.Contains(t, plan,
			"SEARCH m USING INDEX idx_messages_claude_snapshot (claude_message_id=? AND claude_request_id=?)",
			"query %d must use both Claude snapshot identity columns", i)
	}

	require.Len(t, peers, 2)
	gotIDs := make([]string, 0, len(peers))
	for _, peer := range peers {
		gotIDs = append(gotIDs, peer.sessionID)
	}
	assert.Equal(t, []string{"peer-first", "peer-last"}, gotIDs)
	assert.NotContains(t, gotIDs, "page-owned")
	assert.NotContains(t, gotIDs, "cross-pair")
	assert.NotContains(t, gotIDs, "empty-message")
	assert.NotContains(t, gotIDs, "empty-request")
	assert.NotContains(t, gotIDs, "unmatched")
	assert.Equal(t, "message-001", peers[0].claudeMessageID)
	assert.Equal(t, "request-001", peers[0].claudeRequestID)
	assert.Equal(t, "message-250", peers[1].claudeMessageID)
	assert.Equal(t, "request-250", peers[1].claudeRequestID)
}

func TestSessionExportCopilotReportedCostReplacesSessionEstimates(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "copilot-model-a", InputPerMTok: money.MustParseDollars("10")},
		{ModelPattern: "copilot-model-b", InputPerMTok: money.MustParseDollars("20")},
	}))
	insertExportSession(t, d, Session{
		ID: "copilot:export-authoritative", Project: "alpha", Machine: "local",
		Agent: "copilot", StartedAt: Ptr("2026-06-16T10:00:00Z"),
		EndedAt: Ptr("2026-06-16T10:10:00Z"), UserMessageCount: 1,
	})
	reportedCost := money.MustParseDollars("0.03")
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx,
		"copilot:export-authoritative",
		[]UsageEvent{
			{
				Source: "shutdown", Model: "copilot-model-a",
				InputTokens: 1_000_000,
				OccurredAt:  "2026-06-16T10:05:00Z", DedupKey: "first",
			},
			{
				Source: "shutdown", Model: "copilot-model-b",
				InputTokens: 1_000_000,
				Cost:        &reportedCost, CostStatus: "exact",
				CostSource: CopilotReportedCostSource,
				OccurredAt: "2026-06-16T10:10:00Z", DedupKey: "final",
			},
		},
	))

	result, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{IncludeChildren: true}, Limit: 10, Format: "json",
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	require.NotNil(t, result.Rows[0].ModelUsage)
	assert.True(t, result.Rows[0].ModelUsage.HasCost)
	assert.Equal(t, reportedCost, result.Rows[0].ModelUsage.Cost)
	assert.Equal(t, money.MustParseDollars("0.01"),
		result.Rows[0].ModelUsage.ByModel["copilot-model-a"].Cost)
	assert.Equal(t, money.MustParseDollars("0.02"),
		result.Rows[0].ModelUsage.ByModel["copilot-model-b"].Cost)
	assert.Equal(t, result.Rows[0].ModelUsage.Cost,
		money.MustAdd(
			result.Rows[0].ModelUsage.ByModel["copilot-model-a"].Cost,
			result.Rows[0].ModelUsage.ByModel["copilot-model-b"].Cost,
		))
	assert.Equal(t, export.CostSourceComputed,
		result.Rows[0].ModelUsage.ByModel["copilot-model-a"].CostSource)
	assert.Equal(t, export.CostSourceComputed,
		result.Rows[0].ModelUsage.ByModel["copilot-model-b"].CostSource)
	require.NotNil(t, result.Pricing)
	assert.Equal(t, export.CostSourceMixed, result.Pricing.CostSource)
}

func TestAllSessionExportKeepsOnePricingSnapshotAcrossPages(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "snapshot-model", InputPerMTok: money.MustParseDollars("1"),
	}}))
	for i, endedAt := range []string{
		"2026-05-01T10:00:00Z", "2026-05-01T09:00:00Z",
	} {
		id := fmt.Sprintf("pricing-page-%d", i)
		insertExportSession(t, d, Session{
			ID: id, Project: "alpha", Machine: "local",
			EndedAt: &endedAt, UserMessageCount: 1,
		})
		insertMessages(t, d, Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: endedAt, Model: "snapshot-model",
			TokenUsage:       jsontext.Value(`{"input_tokens":1000000}`),
			HasContextTokens: true,
		})
	}

	pages, err := d.exportAllSessionSummaries(ctx, SessionExportOptions{
		Limit: 1, Filter: SessionFilter{IncludeChildren: true},
	}, func(page int, _ *sql.Tx) error {
		if page != 1 {
			return nil
		}
		return d.UpsertModelPricing([]ModelPricing{{
			ModelPattern: "snapshot-model", InputPerMTok: money.MustParseDollars("99"),
		}})
	}, nil)
	require.NoError(t, err)
	require.Len(t, pages, 2)
	require.NotEmpty(t, pages[0].NextCursor)
	internalCursor, err := d.decodeSessionExportCursor(pages[0].NextCursor)
	require.NoError(t, err)
	assert.Zero(t, internalCursor.SnapshotCount)
	assert.Empty(t, internalCursor.SnapshotDigest)
	assert.Zero(t, internalCursor.PrefixCount)
	assert.Empty(t, internalCursor.PrefixDigest)
	require.NotNil(t, pages[0].Pricing)
	require.NotNil(t, pages[1].Pricing)
	assert.Equal(t, pages[0].Pricing.Digest, pages[1].Pricing.Digest)
	for _, page := range pages {
		require.Len(t, page.Rows, 1)
		require.NotNil(t, page.Rows[0].ModelUsage)
		assert.Equal(t, money.MustParseDollars("1.0"), page.Rows[0].ModelUsage.Cost)
	}
}

func TestAllSessionExportHonorsCancellation(t *testing.T) {
	d := testSessionExportDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := d.ExportAllSessionSummaries(ctx, SessionExportOptions{Limit: 1})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSessionExportCursorResetsWhenRowMovesBeforeCursor(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "cursor-db"),
		"set database id")

	for _, row := range []struct {
		id, ended string
	}{
		{"page-a", "2026-05-01T10:00:00Z"},
		{"page-b", "2026-05-01T09:00:00Z"},
		{"page-c", "2026-05-01T08:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "alpha",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  2,
		Format: "json",
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"page-a", "page-b"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)

	insertExportSession(t, d, Session{
		ID:               "page-c",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        Ptr("2026-05-01T00:00:00Z"),
		EndedAt:          Ptr("2026-05-01T09:30:00Z"),
		MessageCount:     1,
		UserMessageCount: 1,
	})
	_, err = d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Cursor: first.NextCursor,
		Limit:  10,
		Format: "json",
	})
	require.Error(t, err, "moved row should reset cursor")
	assert.ErrorIs(t, err, ErrSessionExportCursorReset,
		"expected reset error, got %v", err)
}

func TestSessionExportCursorResetsWhenUnemittedRowMovesAboveWatermark(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "cursor-db"),
		"set database id")

	for _, row := range []struct {
		id, ended string
	}{
		{"page-a", "2026-05-01T10:00:00Z"},
		{"page-b", "2026-05-01T09:00:00Z"},
		{"page-c", "2026-05-01T08:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "watermark-reset",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "watermark-reset"},
		Limit:  1,
		Format: "json",
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"page-a"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)

	_, err = d.getWriter().ExecContext(ctx,
		`UPDATE sessions SET ended_at = ? WHERE id = ?`,
		"2026-05-01T11:00:00Z", "page-c",
	)
	require.NoError(t, err)

	_, err = d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "watermark-reset"},
		Cursor: first.NextCursor,
		Limit:  1,
		Format: "json",
	})
	require.Error(t, err, "moved suffix row should reset cursor")
	assert.ErrorIs(t, err, ErrSessionExportCursorReset,
		"expected reset error, got %v", err)
}

func TestSessionExportCursorRejectsWrongDatabaseAndChangedFilters(t *testing.T) {
	ctx := t.Context()
	d1 := testDB(t)
	d2 := testDB(t)
	require.NoError(t, d1.SetDatabaseIDForTest(ctx, "db-one"), "set db one")
	require.NoError(t, d2.SetDatabaseIDForTest(ctx, "db-two"), "set db two")
	// Two archives on the same install share the config cursor_secret; the
	// wrong-database reset path applies to that case. Cursors from another
	// install fail signature verification instead.
	sharedSecret := []byte("session-export-shared-install-secret")
	d1.SetCursorSecret(sharedSecret)
	d2.SetCursorSecret(sharedSecret)
	for _, d := range []*DB{d1, d2} {
		insertExportSession(t, d, Session{
			ID:               "cursor-reset-row",
			Project:          "alpha",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr("2026-05-01T10:00:00Z"),
			MessageCount:     1,
			UserMessageCount: 1,
		})
		insertExportSession(t, d, Session{
			ID:               "cursor-reset-row-2",
			Project:          "alpha",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr("2026-05-01T09:00:00Z"),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	first, err := d1.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Limit:  1,
		Format: "json",
	})
	require.NoError(t, err, "first page")
	require.NotEmpty(t, first.NextCursor)

	_, err = d2.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Cursor: first.NextCursor,
		Limit:  1,
		Format: "json",
	})
	require.Error(t, err, "wrong database cursor")
	require.ErrorIs(t, err, ErrSessionExportCursorReset,
		"expected reset error, got %v", err)

	_, err = d1.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "beta"},
		Cursor: first.NextCursor,
		Limit:  1,
		Format: "json",
	})
	require.Error(t, err, "changed filter cursor")
	require.ErrorIs(t, err, ErrSessionExportCursorConflict,
		"expected conflict error, got %v", err)

	formatChanged, err := d1.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "alpha"},
		Cursor: first.NextCursor,
		Limit:  1,
		Format: "human",
	})
	require.NoError(t, err, "changed format should not affect cursor identity")
	assert.Equal(t, []string{"cursor-reset-row-2"}, sessionExportRowIDs(formatChanged.Rows))
}

func TestSessionExportCursorAllowsEquivalentFilters(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	for _, row := range []struct {
		id, machine, outcome, ended string
	}{
		{"filter-a", "local", "success", "2026-05-01T10:00:00Z"},
		{"filter-b", "laptop", "failed", "2026-05-01T09:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "filter",
			Machine:          row.machine,
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
			Outcome:          row.outcome,
			HealthGrade:      Ptr("A"),
		})
		require.NoError(t, d.UpdateSessionSignals(ctx, row.id, SessionSignalUpdate{
			Outcome:     row.outcome,
			HealthGrade: Ptr("A"),
		}), "update filter signals %s", row.id)
	}

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:          "filter",
			Machine:          " local, laptop ",
			Outcome:          []string{"success", "failed"},
			HealthGrade:      []string{"A", "B"},
			ExcludeAutomated: true,
		},
		Limit: 1,
	})
	require.NoError(t, err, "first page")
	require.NotEmpty(t, first.NextCursor)

	second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:        "filter",
			Machine:        "laptop,local",
			Outcome:        []string{"failed", "success"},
			HealthGrade:    []string{"B", "A"},
			AutomatedScope: "human",
		},
		Cursor: first.NextCursor,
		Limit:  1,
	})
	require.NoError(t, err, "equivalent filters should not conflict")
	assert.Equal(t, []string{"filter-b"}, sessionExportRowIDs(second.Rows))
}

func TestSessionExportCursorPreservesTimezoneFilter(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	for _, row := range []struct {
		id, started, ended string
	}{
		{"new-york-late", "2024-06-17T02:30:00Z", "2024-06-17T03:00:00Z"},
		{"new-york-earlier", "2024-06-17T01:30:00Z", "2024-06-17T02:00:00Z"},
		{"utc-only", "2024-06-16T01:30:00Z", "2024-06-16T02:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "timezone-cursor",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr(row.started),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project:  "timezone-cursor",
			Date:     "2024-06-16",
			Timezone: " America/New_York ",
		},
		Limit: 1,
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"new-york-late"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)

	t.Run("cursor owned filter", func(t *testing.T) {
		second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
			Cursor:          first.NextCursor,
			UseCursorFilter: true,
			Limit:           1,
		})
		require.NoError(t, err, "resume with embedded filter")
		assert.Equal(t,
			[]string{"new-york-earlier"}, sessionExportRowIDs(second.Rows))
		assert.Empty(t, second.NextCursor)
	})

	t.Run("equivalent timezone", func(t *testing.T) {
		second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
			Filter: SessionFilter{
				Project:  "timezone-cursor",
				Date:     "2024-06-16",
				Timezone: "America/New_York",
			},
			Cursor: first.NextCursor,
			Limit:  1,
		})
		require.NoError(t, err, "resume with equivalent timezone")
		assert.Equal(t,
			[]string{"new-york-earlier"}, sessionExportRowIDs(second.Rows))
	})

	t.Run("changed timezone", func(t *testing.T) {
		_, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
			Filter: SessionFilter{
				Project:  "timezone-cursor",
				Date:     "2024-06-16",
				Timezone: "UTC",
			},
			Cursor: first.NextCursor,
			Limit:  1,
		})
		require.Error(t, err, "changed timezone")
		assert.ErrorIs(t, err, ErrSessionExportCursorConflict)
	})
}

func TestSessionExportCursorTreatsDefaultAndExplicitUTCAsEquivalent(t *testing.T) {
	d := testSessionExportDB(t)
	ctx := t.Context()
	for _, row := range []struct {
		id, started, ended string
	}{
		{"utc-later", "2024-06-16T03:00:00Z", "2024-06-16T04:00:00Z"},
		{"utc-earlier", "2024-06-16T01:00:00Z", "2024-06-16T02:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID: row.id, Project: "utc-cursor", Machine: "local", Agent: "claude",
			StartedAt: new(row.started), EndedAt: new(row.ended),
			MessageCount: 1, UserMessageCount: 1,
		})
	}

	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "utc-cursor", Date: "2024-06-16"},
		Limit:  1,
	})
	require.NoError(t, err, "first page")
	require.Equal(t, []string{"utc-later"}, sessionExportRowIDs(first.Rows))
	require.NotEmpty(t, first.NextCursor)

	second, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{
			Project: "utc-cursor", Date: "2024-06-16", Timezone: "UTC",
		},
		Cursor: first.NextCursor,
		Limit:  1,
	})
	require.NoError(t, err, "explicit UTC should match the default timezone")
	assert.Equal(t, []string{"utc-earlier"}, sessionExportRowIDs(second.Rows))
}

func TestSessionExportCursorTamperingReturnsInvalidCursor(t *testing.T) {
	ctx := t.Context()
	d := testSessionExportDB(t)
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "tamper-db"), "set database id")
	for _, row := range []struct {
		id, ended string
	}{
		{"tamper-a", "2026-05-01T10:00:00Z"},
		{"tamper-b", "2026-05-01T09:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "tamper",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "tamper"},
		Limit:  1,
	})
	require.NoError(t, err, "first page")

	_, err = d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "tamper"},
		Cursor: tamperSessionExportCursorDatabaseID(t, first.NextCursor, "other-db"),
		Limit:  1,
	})
	require.Error(t, err, "tampered cursor")
	require.ErrorIs(t, err, ErrInvalidCursor,
		"expected invalid cursor, got %v", err)
	assert.NotErrorIs(t, err, ErrSessionExportCursorReset,
		"tampered cursor must not be treated as a valid wrong-database cursor")
}

func TestSessionExportCursorRejectsForgeryWithKnownKey(t *testing.T) {
	ctx := t.Context()
	d := testSessionExportDB(t)
	require.NoError(t, d.SetDatabaseIDForTest(ctx, "forge-db"), "set database id")
	for _, row := range []struct {
		id, ended string
	}{
		{"forge-a", "2026-05-01T10:00:00Z"},
		{"forge-b", "2026-05-01T09:00:00Z"},
	} {
		insertExportSession(t, d, Session{
			ID:               row.id,
			Project:          "forge",
			Machine:          "local",
			Agent:            "claude",
			StartedAt:        Ptr("2026-05-01T00:00:00Z"),
			EndedAt:          Ptr(row.ended),
			MessageCount:     1,
			UserMessageCount: 1,
		})
	}
	first, err := d.ExportSessionSummaries(ctx, SessionExportOptions{
		Filter: SessionFilter{Project: "forge"},
		Limit:  1,
	})
	require.NoError(t, err, "first page")
	require.NotEmpty(t, first.NextCursor, "next cursor")

	// A consumer holding a cursor must not be able to widen its scope by
	// rewriting the payload and re-signing with a publicly known key, such
	// as the constant this cursor was signed with before per-install
	// secrets.
	parts := strings.Split(first.NextCursor, ".")
	require.Len(t, parts, 2, "cursor parts")
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err, "decode cursor payload")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload), "unmarshal payload")
	payload["filters"] = map[string]any{}
	forged, err := json.Marshal(payload)
	require.NoError(t, err, "marshal forged payload")
	mac := hmac.New(sha256.New, []byte("agentsview-session-summary-export-v1"))
	mac.Write(forged)
	forgedCursor := base64.RawURLEncoding.EncodeToString(forged) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	_, err = d.ExportSessionSummaries(ctx, SessionExportOptions{
		Cursor:          forgedCursor,
		UseCursorFilter: true,
		Limit:           1,
	})
	require.Error(t, err, "forged cursor")
	assert.ErrorIs(t, err, ErrInvalidCursor,
		"expected invalid cursor, got %v", err)
}

func testSessionExportDB(t *testing.T) *DB {
	t.Helper()
	d := testDB(t)
	require.NoError(t, d.SetDatabaseIDForTest(
		t.Context(), "session-export-db"),
		"set session export database id")
	return d
}

func seedSessionExportPricing(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:         "model-computed",
			InputPerMTok:         money.MustParseDollars("10"),
			OutputPerMTok:        money.MustParseDollars("20"),
			CacheCreationPerMTok: money.MustParseDollars("30"),
			CacheReadPerMTok:     money.MustParseDollars("1"),
		},
		{
			ModelPattern:         "model-reported",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("0.5"),
		},
		{
			ModelPattern:         "unreturned-model",
			InputPerMTok:         money.MustParseDollars("100"),
			OutputPerMTok:        money.MustParseDollars("200"),
			CacheCreationPerMTok: money.MustParseDollars("300"),
			CacheReadPerMTok:     money.MustParseDollars("50"),
		},
	}), "upsert pricing")
}

func insertExportSession(t *testing.T, d *DB, s Session) {
	t.Helper()
	if s.Machine == "" {
		s.Machine = defaultMachine
	}
	if s.Agent == "" {
		s.Agent = defaultAgent
	}
	if s.MessageCount == 0 {
		s.MessageCount = 1
	}
	require.NoError(t, d.UpsertSession(t.Context(), s), "upsert export session %s", s.ID)
}

func sessionExportRowsByID(rows []SessionSummaryRow) map[string]SessionSummaryRow {
	out := make(map[string]SessionSummaryRow, len(rows))
	for _, row := range rows {
		out[row.ID] = row
	}
	return out
}

func sessionExportRowIDs(rows []SessionSummaryRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func sortedSetKeysFromMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func tamperSessionExportCursorDatabaseID(
	t *testing.T, cursor, databaseID string,
) string {
	t.Helper()

	parts := strings.Split(cursor, ".")
	require.Len(t, parts, 2, "cursor parts")
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err, "decode cursor payload")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload), "unmarshal cursor payload")
	payload["database_id"] = databaseID
	tampered, err := json.Marshal(payload)
	require.NoError(t, err, "marshal tampered cursor payload")
	return base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[1]
}

func assertJSONHasKey(t *testing.T, v any, key string) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err, "marshal value")
	var obj map[string]any
	require.NoError(t, json.Unmarshal(data, &obj), "unmarshal object")
	assert.Contains(t, obj, key)
}

func assertContentFreeJSON(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err, "marshal content-free JSON")
	var decoded any
	require.NoError(t, json.Unmarshal(data, &decoded), "unmarshal content-free JSON")
	assertNoBannedJSONKeys(t, decoded)
}

func assertNoBannedJSONKeys(t *testing.T, v any) {
	t.Helper()
	banned := map[string]struct{}{
		"message": {}, "messages": {}, "prompt": {}, "response": {},
		"content": {}, "text": {}, "transcript": {},
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if _, ok := banned[k]; ok {
				assert.Failf(t, "banned JSON key", "found key %q", k)
			}
			assertNoBannedJSONKeys(t, child)
		}
	case []any:
		for _, child := range x {
			assertNoBannedJSONKeys(t, child)
		}
	}
}

func sortableInt(v int) string {
	return string(rune('a'+(v/100)%10)) +
		string(rune('a'+(v/10)%10)) +
		string(rune('a'+v%10))
}
