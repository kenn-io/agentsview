package duckdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// OpenActivityReportDisk enables reuse across restarts for a local mirror.
func (s *Store) OpenActivityReportDisk(ctx context.Context) error {
	if s.path == "" {
		return nil
	}
	path, err := filepath.Abs(s.path)
	if err != nil {
		return err
	}
	id := sha256.Sum256([]byte(path))
	if err := s.reports.OpenDisk("duckdb-activity-reports", hex.EncodeToString(id[:8])); err != nil {
		return err
	}
	s.reports.StartSweep(ctx)
	return nil
}
