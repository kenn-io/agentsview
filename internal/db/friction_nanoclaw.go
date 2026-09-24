package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// SessionIDsUnderPath lists live sessions whose file_path lies under dir.
// It compares a case-sensitive prefix (LIKE would fold ASCII case).
func (db *DB) SessionIDsUnderPath(ctx context.Context, dir string) ([]string, error) {
	prefix := strings.TrimRight(dir, `/\`) + string(filepath.Separator)
	rows, err := db.getReader().Query(ctx,
		"SELECT id FROM sessions"+
			" WHERE substr(file_path, 1, length(?)) = ?"+
			" AND deleted_at IS NULL"+
			" ORDER BY id",
		prefix, prefix,
	)
	if err != nil {
		return nil, fmt.Errorf("listing sessions under %s: %w", dir, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning session under %s: %w", dir, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
