package duckdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// OpenActivityReportDisk enables reuse across restarts for a mirror.
func (s *Store) OpenActivityReportDisk(ctx context.Context) error {
	id := s.trendsCacheID
	if s.path != "" {
		path, err := filepath.Abs(s.path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(path))
		id = hex.EncodeToString(sum[:8])
	}
	if id == "" {
		return nil
	}
	if err := s.reports.OpenDisk("duckdb-activity-reports", id); err != nil {
		return err
	}
	err := s.usageDisk.Open("duckdb-usage", id, 128<<20, 256<<20)
	s.reports.StartSweep(ctx, s.usageDisk.Sweep)
	return err
}
