package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/agentsview/internal/parser"
)

// SourceFreshnessSnapshot holds freshness at pass start. Use it only while
// the pass is the sole archive writer and each source is checked before its
// own writes. Missing or ambiguous keys must use the live queries.
type SourceFreshnessSnapshot struct {
	statHash map[SessionSourcePath]uint64
	rows     map[SessionSourcePath]*sourceFreshnessRow
}

type sourceFreshnessRow struct {
	// active reports a row that is neither deleted nor source-missing.
	active          bool
	tied            bool
	project         string
	minDataVersion  int
	newestFileMtime sql.NullInt64
	newestFileSize  sql.NullInt64
}

// LoadSourceFreshnessSnapshot reads only the requested providers, their stored
// agents, and discovered lookup paths through one reader transaction.
func (db *DB) LoadSourceFreshnessSnapshot(ctx context.Context, agents []parser.AgentType, paths []string) (*SourceFreshnessSnapshot, error) {
	var providers, storedAgents []string
	for _, agent := range agents {
		providers = append(providers, string(agent))
		for _, stored := range parser.ProviderStoredAgents(agent) {
			storedAgents = append(storedAgents, string(stored))
		}
	}
	providerIn, providerArgs := InPlaceholders(providers)
	storedIn, storedArgs := InPlaceholders(storedAgents)
	// A JSON list keeps large discoveries within SQLite's parameter limit.
	encodedPaths, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("encoding source freshness paths: %w", err)
	}
	providerArgs = append(providerArgs, string(encodedPaths))
	storedArgs = append(storedArgs, string(encodedPaths))
	snap := &SourceFreshnessSnapshot{
		statHash: map[SessionSourcePath]uint64{},
		rows:     map[SessionSourcePath]*sourceFreshnessRow{},
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("beginning source freshness snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	hashRows, err := tx.QueryContext(ctx,
		"SELECT agent, file_path, stat_hash FROM provider_freshness WHERE agent IN "+providerIn+
			" AND file_path IN (SELECT value FROM json_each(?))", providerArgs...)
	if err != nil {
		return nil, fmt.Errorf("loading provider_freshness: %w", err)
	}
	defer func() { _ = hashRows.Close() }()
	for hashRows.Next() {
		var agent, path string
		var h int64
		if err := hashRows.Scan(&agent, &path, &h); err != nil {
			return nil, fmt.Errorf("scanning provider_freshness: %w", err)
		}
		snap.statHash[SessionSourcePath{agent, path}] = uint64(h)
	}
	if err := hashRows.Err(); err != nil {
		return nil, err
	}
	_ = hashRows.Close()

	sessRows, err := tx.QueryContext(ctx,
		"SELECT agent, file_path, project, data_version,"+
			" file_mtime, file_size, deleted_at IS NULL"+
			" FROM sessions INDEXED BY idx_sessions_file_path"+
			" WHERE file_path IS NOT NULL AND source_missing_at IS NULL AND agent IN "+storedIn+
			" AND file_path IN (SELECT value FROM json_each(?))", storedArgs...)
	if err != nil {
		return nil, fmt.Errorf("loading source freshness rows: %w", err)
	}
	defer func() { _ = sessRows.Close() }()
	for sessRows.Next() {
		var agent, path, project string
		var dataVersion int
		var mtime, size sql.NullInt64
		var notDeleted bool
		if err := sessRows.Scan(&agent, &path, &project, &dataVersion, &mtime, &size, &notDeleted); err != nil {
			return nil, fmt.Errorf("scanning source freshness rows: %w", err)
		}
		key := SessionSourcePath{agent, path}
		row := snap.rows[key]
		if row == nil {
			row = &sourceFreshnessRow{}
			snap.rows[key] = row
		}
		if !notDeleted {
			continue
		}
		if !row.active || dataVersion < row.minDataVersion {
			row.minDataVersion = dataVersion
		}
		if !row.active || (mtime.Valid && (!row.newestFileMtime.Valid || mtime.Int64 > row.newestFileMtime.Int64)) {
			row.tied = false
			row.newestFileMtime = mtime
			row.newestFileSize = size
			row.project = project
		} else if mtime == row.newestFileMtime {
			row.tied = true
		}
		row.active = true
	}
	if err := sessRows.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

// StatHash mirrors GetProviderStatHash for sources the snapshot saw.
func (s *SourceFreshnessSnapshot) StatHash(agent, path string) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	h, ok := s.statHash[SessionSourcePath{agent, path}]
	return h, ok
}

// State mirrors GetFileInfoByAgentPath (found) plus
// GetSourceRepairStateByAgentPath (project, dataVersion, active) for one
// (agent, file_path). found is false when the snapshot holds no non-missing
// row or the newest active rows tie on mtime; ties query live because the
// live query specifies no secondary ordering.
func (s *SourceFreshnessSnapshot) State(agent, path string) (project string, dataVersion int, active, found bool) {
	if s == nil {
		return "", 0, false, false
	}
	row, ok := s.rows[SessionSourcePath{agent, path}]
	if !ok || row.tied {
		return "", 0, false, false
	}
	if !row.active || !row.newestFileMtime.Valid || !row.newestFileSize.Valid {
		return "", 0, false, true
	}
	return row.project, row.minDataVersion, true, true
}
