package postgres

import (
	"context"

	"go.kenn.io/agentsview/internal/rawparity"
)

// Only the fenced activation engine constructs this adapter. Shared physical
// query bodies remain usable by ordinary RC projection, while activation must
// retain its caller-owned RR RW transaction. The public adapter stays RR RO.
//
//nolint:unused // constructed by the later fenced activation engine
type rawActivationReader struct{ RawParityReader }

//nolint:unused // constructed by the later fenced activation engine
func (r rawActivationReader) Candidates(ctx context.Context) ([]rawparity.Candidate, error) {
	return loadRawParityCandidates(ctx, r.Tx)
}

//nolint:unused // constructed by the later fenced activation engine
func (r rawActivationReader) Readiness(ctx context.Context, scope rawparity.Scope) ([]rawparity.Readiness, error) {
	rows, err := loadRawParityReadiness(ctx, r.Tx, RawParityScope{Device: scope.Device, Provider: scope.Provider}, r.Version)
	out := make([]rawparity.Readiness, 0, len(rows))
	for _, s := range rows {
		out = append(out, rawparity.Readiness{Device: s.Device, Provider: s.Provider, Projected: s.Projected, Pending: s.Pending, Failed: s.Failed, Unsupported: s.Unsupported})
	}
	return out, err
}

//nolint:unused // constructed by the later fenced activation engine
func (r rawActivationReader) Snapshots(ctx context.Context, ids []string) (map[string]rawparity.Snapshot, error) {
	return loadRawParitySnapshots(ctx, r.Tx, ids)
}

//nolint:unused // constructed by the later fenced activation engine
func (r rawActivationReader) Links(ctx context.Context, id string) ([]rawparity.Link, error) {
	return loadRawParityLinks(ctx, r.Tx, id)
}
