// Package ledgerpushstatus defines the persisted PostgreSQL ledger push
// status shared by the sync writer and ledger status reports.
package ledgerpushstatus

import (
	"encoding/json/v2"
	"fmt"
)

// KeyPrefix identifies the sync-state records that hold ledger push status.
const KeyPrefix = "pg_ledger_push_status_v1:"

// ZoneCounts counts one zone's segments in one push.
type ZoneCounts struct {
	Pushed    int `json:"pushed"`
	Identical int `json:"identical"`
	HeldBack  int `json:"held_back"`
}

// Status is the last ledger phase outcome for one target. Failures are
// [zone, source, seq, message]: local segments that fail their checksum,
// and identities the hub already holds with different content. They are
// retried on every push until they succeed.
type Status struct {
	At       string                `json:"at"`
	Zones    map[string]ZoneCounts `json:"zones"`
	Failures [][4]string           `json:"failures"`
	// HeldBackSegments are [zone, source, seq] identities retried on later pushes.
	HeldBackSegments [][3]string `json:"held_back_segments,omitempty"`
}

// Decode parses a stored ledger push status.
func Decode(value string) (Status, error) {
	var status Status
	if err := json.Unmarshal([]byte(value), &status); err != nil {
		return Status{}, fmt.Errorf("decoding ledger push status: %w", err)
	}
	return status, nil
}
