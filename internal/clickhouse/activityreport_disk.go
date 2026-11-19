package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
)

func (s *Store) openActivityReportDisk(t Target) error {
	mirror, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("parsing the clickhouse URL for the activity report cache: %w", err)
	}
	database, err := t.DatabaseName()
	if err != nil {
		return err
	}
	// The mirror is named by its host and database only; the URL's
	// credentials stay out of the path.
	mirrorID := sha256.Sum256([]byte(mirror.Host + "/" + database))
	id := hex.EncodeToString(mirrorID[:8])
	if err := s.reports.OpenDisk("clickhouse-activity-reports", id); err != nil {
		return err
	}
	return s.usageDisk.Open("clickhouse-usage", id, 128<<20, 256<<20)
}

// StartBackground prepares usage before serving and starts report cleanup.
func (s *Store) StartBackground(ctx context.Context) {
	if _, err := s.preparedUsageState(ctx); err != nil {
		log.Printf("clickhouse: preparing usage state at startup: %v", err)
	}
	s.reports.StartSweep(ctx, s.usageDisk.Sweep)
}

func (s *Store) stopBackground() { s.reports.Close() }
