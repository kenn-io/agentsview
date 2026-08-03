package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const (
	identityArchiveBID   = "bun-identity-archive-b"
	identityArchiveBSalt = "bun-identity-salt-b"
	identityAlphaProject = "identity-alpha"
	identityBetaProject  = "identity-beta"
	identityAlphaAID     = "bun-identity-alpha-a"
	identityAlphaBID     = "bun-identity-alpha-b"
	identityBetaID       = "bun-identity-beta"
)

// IdentityFixture names the literal source-scoped identity rows used by every
// Task 6 backend contract.
type IdentityFixture struct {
	ArchiveAID      string
	ArchiveBID      string
	AlphaProject    string
	BetaProject     string
	AlphaSessionAID string
	AlphaSessionBID string
}

// InsertBunIdentityFixture inserts two observations owned by distinct source
// archives into a generated canonical schema.
func InsertBunIdentityFixture(
	ctx context.Context,
	store bun.IDB,
	archiveAID string,
	archiveASalt string,
) (IdentityFixture, error) {
	archives := []bunmodel.SourceArchive{
		{SourceArchiveID: archiveAID, SourceArchiveSalt: archiveASalt},
		{SourceArchiveID: identityArchiveBID, SourceArchiveSalt: identityArchiveBSalt},
	}
	for index := range archives {
		if _, err := store.NewInsert().Model(&archives[index]).Exec(ctx); err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting identity source archive %s: %w",
				archives[index].SourceArchiveID, err,
			)
		}
	}
	for _, row := range bunIdentityObservationRows(archiveAID, archiveASalt) {
		if _, err := store.NewInsert().Model(&row).Exec(ctx); err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting identity observation %s: %w", row.Project, err,
			)
		}
	}
	for _, row := range bunIdentitySessionRows(archiveAID) {
		if _, err := store.NewInsert().Model(&row).Exec(ctx); err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting identity session %s: %w", row.ID, err,
			)
		}
	}
	for _, row := range bunIdentityMappingRows(archiveAID) {
		query := store.NewInsert().Model(&row).
			Column(
				"id", "source_archive_id", "machine", "path_prefix", "layout",
				"project", "original_project", "enabled", "created_at", "updated_at",
			).
			Value("enabled", "?", row.Enabled)
		if _, err := query.Exec(ctx); err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting identity mapping %s: %w", row.PathPrefix, err,
			)
		}
	}
	return identityFixture(archiveAID), nil
}

// InsertSQLiteIdentityFixture inserts the same observations through SQLite's
// shipped canonical tables. The first source archive already exists because it
// is the local archive identity.
func InsertSQLiteIdentityFixture(
	ctx context.Context,
	tx *sql.Tx,
	archiveAID string,
	archiveASalt string,
) (IdentityFixture, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES (?, ?)`, identityArchiveBID, identityArchiveBSalt); err != nil {
		return IdentityFixture{}, fmt.Errorf("inserting SQLite identity archive: %w", err)
	}
	for _, row := range bunIdentityObservationRows(archiveAID, archiveASalt) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_project_identity_observations (
				source_archive_id, source_archive_salt, project, machine,
				root_path, git_remote, git_remote_name, repository_path,
				worktree_name, worktree_root_path, worktree_relationship,
				checkout_state, git_branch, remote_resolution,
				remote_candidate_count, observed_at, normalized_remote,
				key_source, key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.SourceArchiveID, row.SourceArchiveSalt, row.Project, row.Machine,
			row.RootPath, row.GitRemote, row.GitRemoteName, row.RepositoryPath,
			row.WorktreeName, row.WorktreeRootPath, row.WorktreeRelationship,
			row.CheckoutState, row.GitBranch, row.RemoteResolution,
			row.RemoteCandidateCount, row.ObservedAt, row.NormalizedRemote,
			row.KeySource, row.Key,
		); err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting SQLite identity observation %s: %w", row.Project, err,
			)
		}
	}
	for _, row := range bunIdentitySessionRows(archiveAID) {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project, machine, agent, cwd, started_at, ended_at, created_at,
				source_archive_id, source_database_generation
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.Project, row.Machine, row.Agent, row.Cwd,
			row.StartedAt, row.EndedAt, row.CreatedAt,
			row.SourceArchiveID, row.SourceDatabaseGeneration,
		)
		if err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting SQLite identity session %s: %w", row.ID, err,
			)
		}
	}
	for _, row := range bunIdentityMappingRows(archiveAID) {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO source_worktree_project_mappings (
				id, source_archive_id, machine, path_prefix, layout, project,
				original_project, enabled, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.SourceArchiveID, row.Machine, row.PathPrefix, row.Layout,
			row.Project, row.OriginalProject, row.Enabled, row.CreatedAt, row.UpdatedAt,
		)
		if err != nil {
			return IdentityFixture{}, fmt.Errorf(
				"inserting SQLite identity mapping %s: %w", row.PathPrefix, err,
			)
		}
	}
	return identityFixture(archiveAID), nil
}

func identityFixture(archiveAID string) IdentityFixture {
	return IdentityFixture{
		ArchiveAID: archiveAID, ArchiveBID: identityArchiveBID,
		AlphaProject: identityAlphaProject, BetaProject: identityBetaProject,
		AlphaSessionAID: identityAlphaAID, AlphaSessionBID: identityAlphaBID,
	}
}

func bunIdentitySessionRows(archiveAID string) []bunmodel.Session {
	timestamp := func(day, hour, minute int) *bunmodel.Timestamp {
		value := bunmodel.NewTimestamp(
			time.Date(2026, 8, day, hour, minute, 0, 0, time.UTC),
		)
		return &value
	}
	requiredTimestamp := func(day, hour int) bunmodel.Timestamp {
		return *timestamp(day, hour, 0)
	}
	return []bunmodel.Session{
		{
			ID: identityAlphaAID, Project: identityAlphaProject,
			Machine: "identity-host-a", Agent: "codex", Cwd: "/workspace/alpha/cmd",
			StartedAt: timestamp(1, 12, 0), EndedAt: timestamp(1, 12, 30),
			CreatedAt: requiredTimestamp(1, 12), SourceArchiveID: archiveAID,
			SourceDatabaseGeneration: "identity-generation-a",
		},
		{
			ID: identityAlphaBID, Project: identityAlphaProject,
			Machine: "identity-host-a", Agent: "claude",
			Cwd:       `\workspace\alpha\frontend`,
			StartedAt: timestamp(1, 13, 0), EndedAt: timestamp(1, 13, 30),
			CreatedAt: requiredTimestamp(1, 13), SourceArchiveID: archiveAID,
			SourceDatabaseGeneration: "identity-generation-a",
		},
		{
			ID: identityBetaID, Project: identityBetaProject,
			Machine: "identity-host-b", Agent: "codex", Cwd: "/workspace/beta/service",
			StartedAt: timestamp(2, 12, 0), CreatedAt: requiredTimestamp(2, 12),
			SourceArchiveID:          identityArchiveBID,
			SourceDatabaseGeneration: "identity-generation-b",
		},
	}
}

func bunIdentityMappingRows(archiveAID string) []bunmodel.SourceWorktreeProjectMapping {
	timestamp := bunmodel.NewTimestamp(
		time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
	)
	return []bunmodel.SourceWorktreeProjectMapping{
		{
			ID: 1, SourceArchiveID: archiveAID, Machine: "identity-host-a",
			PathPrefix: "/workspace/alpha", Layout: "explicit",
			Project: identityAlphaProject, Enabled: true,
			CreatedAt: timestamp, UpdatedAt: timestamp,
		},
		{
			ID: 2, SourceArchiveID: archiveAID, Machine: "identity-host-a",
			PathPrefix: "/workspace/disabled", Layout: "explicit",
			Project: identityBetaProject, OriginalProject: identityAlphaProject,
			Enabled: false, CreatedAt: timestamp, UpdatedAt: timestamp,
		},
		{
			ID: 3, SourceArchiveID: identityArchiveBID, Machine: "identity-host-b",
			PathPrefix: "/workspace/beta", Layout: "explicit",
			Project: identityBetaProject, Enabled: true,
			CreatedAt: timestamp, UpdatedAt: timestamp,
		},
	}
}

func bunIdentityObservationRows(
	archiveAID string,
	archiveASalt string,
) []bunmodel.SourceProjectIdentityObservation {
	observedAt := func(day int) bunmodel.Timestamp {
		return bunmodel.NewTimestamp(
			time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC),
		)
	}
	return []bunmodel.SourceProjectIdentityObservation{
		{
			SourceArchiveID: archiveAID, SourceArchiveSalt: archiveASalt,
			Project: identityAlphaProject, Machine: "identity-host-a",
			RootPath: "/workspace/alpha", GitRemote: "https://example.com/org/alpha.git",
			GitRemoteName: "origin", RepositoryPath: "/workspace/alpha",
			WorktreeRelationship: "primary", CheckoutState: "branch",
			GitBranch: "main", RemoteResolution: "resolved",
			ObservedAt: observedAt(1), NormalizedRemote: "example.com/org/alpha",
			KeySource: "git_remote", Key: "example.com/org/alpha",
		},
		{
			SourceArchiveID:   identityArchiveBID,
			SourceArchiveSalt: identityArchiveBSalt,
			Project:           identityBetaProject, Machine: "identity-host-b",
			RootPath: "/workspace/beta", GitRemote: "git@example.com:org/beta.git",
			GitRemoteName: "origin", RepositoryPath: "/workspace/beta",
			WorktreeRelationship: "primary", CheckoutState: "branch",
			GitBranch: "release", RemoteResolution: "resolved",
			ObservedAt: observedAt(2), NormalizedRemote: "example.com/org/beta",
			KeySource: "git_remote", Key: "example.com/org/beta",
		},
	}
}
