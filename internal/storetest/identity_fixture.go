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
)

// IdentityFixture names the literal source-scoped identity rows used by every
// Task 6 backend contract.
type IdentityFixture struct {
	ArchiveAID   string
	ArchiveBID   string
	AlphaProject string
	BetaProject  string
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
	return identityFixture(archiveAID), nil
}

func identityFixture(archiveAID string) IdentityFixture {
	return IdentityFixture{
		ArchiveAID: archiveAID, ArchiveBID: identityArchiveBID,
		AlphaProject: identityAlphaProject, BetaProject: identityBetaProject,
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
