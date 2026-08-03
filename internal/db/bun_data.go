package db

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
)

const bunDataListBatchSize = 500

// ListProjectIdentityObservations returns canonical source-scoped identity
// observations ordered by archive and raw project identity.
func (s *BunStore) ListProjectIdentityObservations(
	ctx context.Context,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var observations []export.ProjectIdentityObservation
	err := s.view(ctx, func(store bun.IDB) error {
		var err error
		observations, err = listBunProjectIdentityObservations(ctx, store, labels)
		return err
	})
	if err != nil {
		return nil, err
	}
	return observations, nil
}

func listBunProjectIdentityObservations(
	ctx context.Context,
	store bun.IDB,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	if labels != nil && len(labels) == 0 {
		return []export.ProjectIdentityObservation{}, nil
	}
	if labels == nil {
		return listBunProjectIdentityObservationChunk(ctx, store, nil)
	}

	sortedLabels := slices.Clone(labels)
	slices.Sort(sortedLabels)
	sortedLabels = slices.Compact(sortedLabels)
	observations := make([]export.ProjectIdentityObservation, 0)
	for start := 0; start < len(sortedLabels); start += bunDataListBatchSize {
		end := min(start+bunDataListBatchSize, len(sortedLabels))
		rows, err := listBunProjectIdentityObservationChunk(
			ctx, store, sortedLabels[start:end],
		)
		if err != nil {
			return nil, err
		}
		observations = append(observations, rows...)
	}
	sortBunProjectIdentityObservations(observations)
	return observations, nil
}

func listBunProjectIdentityObservationChunk(
	ctx context.Context,
	store bun.IDB,
	labels []string,
) ([]export.ProjectIdentityObservation, error) {
	var rows []bunmodel.SourceProjectIdentityObservation
	query := store.NewSelect().Model(&rows)
	if len(labels) > 0 {
		query = query.Where("project IN (?)", bun.List(labels))
	}
	if err := query.OrderExpr("source_archive_id ASC").
		OrderExpr("project ASC").OrderExpr("machine ASC").
		OrderExpr("root_path ASC").OrderExpr("git_remote ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing project identity observations: %w", err)
	}
	observations := make([]export.ProjectIdentityObservation, 0, len(rows))
	for _, row := range rows {
		observations = append(observations, projectIdentityObservationFromBunRow(row))
	}
	return observations, nil
}

func projectIdentityObservationFromBunRow(
	row bunmodel.SourceProjectIdentityObservation,
) export.ProjectIdentityObservation {
	return export.ProjectIdentityObservation{
		SourceArchiveID: row.SourceArchiveID, SourceArchiveSalt: row.SourceArchiveSalt,
		Project: row.Project, Machine: row.Machine, RootPath: row.RootPath,
		GitRemote: row.GitRemote, GitRemoteName: row.GitRemoteName,
		RepositoryPath: row.RepositoryPath, WorktreeName: row.WorktreeName,
		WorktreeRootPath:     row.WorktreeRootPath,
		WorktreeRelationship: export.WorktreeRelationship(row.WorktreeRelationship),
		CheckoutState:        export.CheckoutState(row.CheckoutState), GitBranch: row.GitBranch,
		RemoteResolution:     export.ProjectResolution(row.RemoteResolution),
		RemoteCandidateCount: row.RemoteCandidateCount,
		ObservedAt:           row.ObservedAt.UTC(), NormalizedRemote: row.NormalizedRemote,
		KeySource: row.KeySource, Key: row.Key,
	}
}

func sortBunProjectIdentityObservations(
	observations []export.ProjectIdentityObservation,
) {
	sort.SliceStable(observations, func(i, j int) bool {
		left, right := observations[i], observations[j]
		if left.SourceArchiveID != right.SourceArchiveID {
			return left.SourceArchiveID < right.SourceArchiveID
		}
		if left.Project != right.Project {
			return left.Project < right.Project
		}
		if left.Machine != right.Machine {
			return left.Machine < right.Machine
		}
		if left.RootPath != right.RootPath {
			return left.RootPath < right.RootPath
		}
		return left.GitRemote < right.GitRemote
	})
}

// BuildProjectIdentityMap resolves raw labels from canonical source-scoped
// observations and the complete set of contributing archive identities.
func (s *BunStore) BuildProjectIdentityMap(
	ctx context.Context,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if labels != nil && len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var projects map[string]export.ProjectMapEntry
	err := s.view(ctx, func(store bun.IDB) error {
		observations, err := listBunProjectIdentityObservations(ctx, store, labels)
		if err != nil {
			return err
		}
		scope, err := bunSourceArchiveIdentityScope(ctx, store, observations)
		if err != nil {
			return err
		}
		projects = export.BuildProjectsMapWithScope(labels, observations, scope)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return projects, nil
}

func bunSourceArchiveIdentityScope(
	ctx context.Context,
	store bun.IDB,
	observations []export.ProjectIdentityObservation,
) (export.IdentityScope, error) {
	var rows []bunmodel.SourceArchive
	if err := store.NewSelect().Model(&rows).
		OrderExpr("source_archive_id ASC").Scan(ctx); err != nil {
		return export.IdentityScope{}, fmt.Errorf("listing source archives: %w", err)
	}
	scopes := make([]export.IdentityScope, 0, len(rows))
	for _, row := range rows {
		scopes = append(scopes, export.IdentityScope{
			ArchiveID: row.SourceArchiveID, ArchiveSalt: row.SourceArchiveSalt,
		})
	}
	switch len(scopes) {
	case 0:
		return bunObservationIdentityScope(observations), nil
	case 1:
		return scopes[0], nil
	default:
		return export.AggregateIdentityScope(scopes), nil
	}
}

func bunObservationIdentityScope(
	observations []export.ProjectIdentityObservation,
) export.IdentityScope {
	unique := make(map[string]export.IdentityScope)
	for _, observation := range observations {
		scope := export.IdentityScope{
			ArchiveID:   strings.TrimSpace(observation.SourceArchiveID),
			ArchiveSalt: strings.TrimSpace(observation.SourceArchiveSalt),
		}
		if scope.ArchiveID == "" || scope.ArchiveSalt == "" {
			continue
		}
		unique[scope.ArchiveID+"\x00"+scope.ArchiveSalt] = scope
	}
	if len(unique) == 0 {
		return export.LegacySharedStoreIdentityScope()
	}
	scopes := make([]export.IdentityScope, 0, len(unique))
	for _, scope := range unique {
		scopes = append(scopes, scope)
	}
	if len(scopes) == 1 {
		return scopes[0]
	}
	return export.AggregateIdentityScope(scopes)
}
