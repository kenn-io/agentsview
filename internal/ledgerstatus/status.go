// Package ledgerstatus assembles `ledger status` for the CLI and the
// /api/v1/ledger/status route: stored counts, sources, gaps and verify
// failures from any store, plus import and push state where the store is
// the local SQLite archive.
package ledgerstatus

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/postgres"
)

// ZoneReport is one zone's status.
type ZoneReport struct {
	ledger.ZoneStatus `json:",inline"`
	Imports           []ImportReport `json:"imports"`
	Push              []PushReport   `json:"push"`
}

// ImportReport is the last import of one followed directory.
type ImportReport struct {
	Path      string         `json:"path"`
	UpdatedAt string         `json:"updated_at"`
	Report    jsontext.Value `json:"report"`
}

// PushReport is one PostgreSQL target's last ledger push for a zone.
type PushReport struct {
	Target    string      `json:"target"`
	At        string      `json:"at"`
	Pushed    int         `json:"pushed"`
	Identical int         `json:"identical"`
	HeldBack  int         `json:"held_back"`
	Failures  [][4]string `json:"failures"`
}

// Store is what every backend offers.
type Store interface {
	LedgerStatus(ctx context.Context, zone string) (ledger.ZoneStatus, error)
}

// localState is the extra state only the SQLite archive keeps.
type localState interface {
	GetLedgerImportState(ctx context.Context, zone, path string) (*db.LedgerImportState, error)
	ListSyncStateByPrefix(ctx context.Context, prefix string) (map[string]string, error)
}

// Collect builds one report per zone, in the order given.
func Collect(ctx context.Context, cfg config.LedgerConfig, st Store, zones []string) ([]ZoneReport, error) {
	local, isLocal := st.(localState)
	pushes := map[string]string{}
	if isLocal {
		var err error
		if pushes, err = local.ListSyncStateByPrefix(ctx, postgres.LedgerPushStatusKeyPrefix); err != nil {
			return nil, err
		}
	}
	reports := make([]ZoneReport, 0, len(zones))
	for _, z := range zones {
		zs, err := st.LedgerStatus(ctx, z)
		if err != nil {
			return nil, err
		}
		r := ZoneReport{ZoneStatus: zs, Imports: []ImportReport{}, Push: pushReports(pushes, z)}
		if zc, ok := cfg.Zone(z); ok && isLocal {
			dir, err := zc.ImportSegmentsDir()
			if err != nil {
				return nil, err
			}
			if dir != "" {
				imp, err := local.GetLedgerImportState(ctx, z, dir)
				if err != nil {
					return nil, err
				}
				if imp != nil {
					r.Imports = append(r.Imports, ImportReport{
						Path: dir, UpdatedAt: imp.UpdatedAt, Report: jsontext.Value(imp.LastReportJSON),
					})
				}
			}
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// pushReports extracts one zone's slice of every target's stored push
// status, targets in name order.
func pushReports(pushes map[string]string, zone string) []PushReport {
	out := []PushReport{}
	for _, target := range slices.Sorted(maps.Keys(pushes)) {
		st, err := postgres.DecodeLedgerPushStatus(pushes[target])
		if err != nil {
			continue
		}
		counts := st.Zones[zone]
		r := PushReport{
			Target: target, At: st.At, Pushed: counts.Pushed,
			Identical: counts.Identical, HeldBack: counts.HeldBack,
			Failures: [][4]string{},
		}
		for _, f := range st.Failures {
			if f[0] == zone {
				r.Failures = append(r.Failures, f)
			}
		}
		out = append(out, r)
	}
	return out
}

// FormatID renders "source-seq" with jilog's six-digit padding.
func FormatID(source, seq string) string {
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return source + "-" + seq
	}
	return fmt.Sprintf("%s-%06d", source, n)
}

// WriteText prints reports the way `agentsview ledger status` does.
func WriteText(w io.Writer, enabled bool, reports []ZoneReport) error {
	var b strings.Builder
	if !enabled {
		b.WriteString("ledger: off ([ledger] enabled = false)\n")
	}
	for _, r := range reports {
		fmt.Fprintf(&b, "zone %s: %d segment(s), %d event(s)\n", r.Zone, r.Segments, r.Events)
		for _, source := range slices.Sorted(maps.Keys(r.Sources)) {
			fmt.Fprintf(&b, "  source %s: latest seq %d\n", source, r.Sources[source])
		}
		for _, g := range r.Gaps {
			fmt.Fprintf(&b, "  gap: %s\n", FormatID(g[0], g[1]))
		}
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "  verify failure: %s: %s\n", FormatID(f[0], f[1]), f[2])
		}
		for _, imp := range r.Imports {
			fmt.Fprintf(&b, "  import %s at %s: %s\n", imp.Path, imp.UpdatedAt, string(imp.Report))
		}
		for _, p := range r.Push {
			target := p.Target
			if target == "" {
				target = "(default)"
			}
			fmt.Fprintf(&b, "  push [%s] at %s: %d pushed, %d already present, %d held back, %d refused\n",
				target, p.At, p.Pushed, p.Identical, p.HeldBack, len(p.Failures))
			for _, f := range p.Failures {
				fmt.Fprintf(&b, "    refused %s: %s\n", FormatID(f[1], f[2]), f[3])
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
