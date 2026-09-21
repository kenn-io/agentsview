package db

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

// Resolve once per export snapshot, in bounded queries, not once per hour.
// Aggregate label catalogs are not evidence for individual contributions.
func (db *DB) reportingSessionReferences(ctx context.Context, tx *sql.Tx, sessions []activity.SessionMeta) (map[string]export.ProjectReference, error) {
	archiveID, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveIDKey, ErrArchiveIDMissing, "archive id")
	if err != nil {
		return nil, err
	}
	salt, err := sessionExportMetadataValue(ctx, tx, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing, "archive salt")
	if err != nil {
		return nil, err
	}
	scope := export.IdentityScope{ArchiveID: archiveID, ArchiveSalt: salt}
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.SessionID
	}
	references := make(map[string]export.ProjectReference, len(ids))
	err = queryChunked(ids, func(chunk []string) error {
		snapshots, err := db.listSessionProjectIdentitySnapshotsFrom(ctx, tx, chunk)
		if err != nil {
			return err
		}
		for id, observation := range snapshots {
			references[id] = export.ResolveProjectReferenceFromObservation(observation, scope)
		}
		return nil
	})
	return references, err
}

func reportingProjectEvidence(contributors map[string]bool, sessions map[string]activity.SessionMeta, projects map[string]export.ProjectMapEntry, references map[string]export.ProjectReference) map[string]export.ProjectMapEntry {
	type evidence struct {
		entry                 export.ProjectMapEntry
		incomplete, ambiguous bool
	}
	states := make(map[string]*evidence)
	for id := range contributors {
		session := sessions[id]
		key := export.ProjectKeyForEntry(projects[session.Project])
		if key == "" {
			continue
		}
		state := states[key]
		if state == nil {
			state = &evidence{entry: export.ProjectMapEntry{DisplayLabel: export.SafeProjectDisplayLabel(session.Project)}}
			states[key] = state
		}
		reference := references[id]
		if reference.ProjectKey != key || reference.Resolution == export.ProjectResolutionUnknown || reference.Resolution == "" {
			state.incomplete = true
		} else if reference.Resolution == export.ProjectResolutionAmbiguous {
			state.ambiguous = true
		} else if reference.Identity == nil {
			state.incomplete = true
		} else if state.entry.Identity == nil {
			state.entry.Identity = export.ProjectCatalogIdentity(reference.Identity)
		} else if state.entry.Identity.Key != reference.Identity.Key {
			state.ambiguous = true
		}
	}
	out := make(map[string]export.ProjectMapEntry, len(states))
	for key, state := range states {
		switch {
		case state.ambiguous:
			state.entry.Resolution, state.entry.Identity = export.ProjectResolutionAmbiguous, nil
		case state.incomplete:
			state.entry.Resolution, state.entry.Identity = export.ProjectResolutionUnknown, nil
		default:
			state.entry.Resolution = export.ProjectResolutionResolved
		}
		out[key] = state.entry
	}
	return out
}
