package db

import (
	"context"
	"time"

	dbgit "go.kenn.io/agentsview/internal/db/git"
)

// CommitRepositoryCandidates names only locally attributed, non-attached session
// paths. The engine must still apply its filesystem/protected-path gates.
func (d *DB) CommitRepositoryCandidates(ctx context.Context, machine, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	release, err := d.acquireBulkRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := d.getReader().QueryContext(ctx, `WITH eligible AS (
 SELECT id,cwd FROM sessions WHERE machine IN ('','local',?) AND deleted_at IS NULL
 AND instr(id,'~')=0 AND COALESCE(file_path,'') NOT LIKE 'archive://%' AND COALESCE(file_path,'') NOT LIKE 's3://%'
),paths AS (
 SELECT cwd AS path FROM eligible UNION SELECT p.repository_path FROM session_project_identity_snapshots p JOIN eligible e ON e.id=p.session_id
) SELECT DISTINCT path FROM paths WHERE path>? AND path<>'' ORDER BY path LIMIT ?`, machine, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// ScanCommitTrailersCached is called only while the engine owns the archive
// generation. It releases no reader into host Git and retains no raw bodies.
func (d *DB) ScanCommitTrailersCached(ctx context.Context, repo string, tips, excludedTips []string, offset int) ([]dbgit.CommitObject, error) {
	writer := d.rawWriter()
	if writer == nil {
		return nil, ErrReadOnly
	}
	_, err := writer.ExecContext(ctx, "DELETE FROM git_cache WHERE kind IN ('commit-scan-v1','commit-scan-v2') AND computed_at < ?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	return dbgit.ScanSessionTrailersPageCachedExcluding(ctx, dbgit.NewCache(writer), repo, tips, excludedTips, offset)
}

// CommitLinkSessionMachine prevents foreign archive paths from being probed.
func (d *DB) CommitLinkSessionMachine(ctx context.Context, id string) (string, error) {
	var machine string
	err := d.getReader().QueryRowContext(ctx, "SELECT machine FROM sessions WHERE id=? AND deleted_at IS NULL", id).Scan(&machine)
	return machine, err
}
