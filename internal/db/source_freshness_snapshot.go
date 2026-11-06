package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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

// SourceFreshnessState mirrors GetFileInfoByAgentPath plus
// GetSourceRepairStateByAgentPath for one (agent, file_path).
type SourceFreshnessState struct {
	// Active mirrors a successful live repair-state lookup, including non-NULL
	// size and mtime. Project comes from the newest active row; DataVersion
	// is the minimum across active rows.
	Active      bool
	Project     string
	DataVersion int
}

// LoadSourceFreshnessSnapshot reads only the requested providers and their
// stored agents through one reader transaction.
func (db *DB) LoadSourceFreshnessSnapshot(ctx context.Context, agents []parser.AgentType) (*SourceFreshnessSnapshot, error) {
	if len(agents) == 0 {
		return nil, nil
	}
	providerArgs := make([]any, 0, len(agents))
	storedArgs := []any{}
	for _, agent := range agents {
		providerArgs = append(providerArgs, string(agent))
		for _, stored := range parser.ProviderStoredAgents(agent) {
			storedArgs = append(storedArgs, string(stored))
		}
	}
	placeholders := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
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
		"SELECT agent, file_path, stat_hash FROM provider_freshness WHERE agent IN ("+placeholders(len(providerArgs))+")", providerArgs...)
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
			" FROM sessions"+
			" WHERE file_path IS NOT NULL AND source_missing_at IS NULL AND agent IN ("+placeholders(len(storedArgs))+")", storedArgs...)
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
	if err := sessRows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
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

// State returns the source's stored freshness state. ok is false when the
// snapshot holds no non-missing row or the newest active rows tie on mtime.
// Ties query live because the live query specifies no secondary ordering.
func (s *SourceFreshnessSnapshot) State(agent, path string) (SourceFreshnessState, bool) {
	if s == nil {
		return SourceFreshnessState{}, false
	}
	row, ok := s.rows[SessionSourcePath{agent, path}]
	if !ok || row.tied {
		return SourceFreshnessState{}, false
	}
	if !row.active || !row.newestFileMtime.Valid || !row.newestFileSize.Valid {
		return SourceFreshnessState{}, true
	}
	return SourceFreshnessState{
		Active:      true,
		Project:     row.project,
		DataVersion: row.minDataVersion,
	}, true
}
