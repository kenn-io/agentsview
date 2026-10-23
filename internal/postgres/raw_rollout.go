package postgres

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"time"
)

// RawRolloutScope selects all sources, one device, one provider, or their
// intersection. Empty selectors mean all. Parity is the last stored gate report.
type RawRolloutScope struct {
	DeviceID                string         `json:"device_id"`
	Provider                string         `json:"provider"`
	State                   string         `json:"state"`
	Parity                  jsontext.Value `json:"parity"`
	ActivatedCorpusRevision *int64         `json:"activated_corpus_revision"`
	UpdatedAt               time.Time      `json:"updated_at"`
	UpdatedBy               string         `json:"updated_by"`
	Served                  int64          `json:"served"`
	Shadow                  int64          `json:"shadow"`
	Superseded              int64          `json:"superseded"`
}

func effectiveRawRollout(scopes []RawRolloutScope, device, provider string) string {
	for _, key := range [][2]string{{device, provider}, {device, ""}, {"", provider}, {"", ""}} {
		for _, scope := range scopes {
			if scope.DeviceID == key[0] && scope.Provider == key[1] {
				return scope.State
			}
		}
	}
	return "active"
}

// lockRawRolloutShared must be the transaction's first statement, before head,
// lease, alias, group or publication locks. Scope writers use its exclusive twin.
func lockRawRolloutShared(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('agentsview-rollout:'||current_schema(),0))`)
	return err
}
