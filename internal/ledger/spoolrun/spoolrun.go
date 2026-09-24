// Package spoolrun turns [[ledger.zones]] config into jilog spool runs and
// renders jilog's per-zone lines, shared by the CLI and the daemon route.
package spoolrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
	"go.kenn.io/agentsview/internal/pathutil"
)

const (
	IngestStateIngested      = "ingested"
	IngestStateSpoolDisabled = "spool_disabled"
	IngestStateNotAuthority  = "not_authority"
)

var (
	ErrNotAuthority           = errors.New("no zone has spool_authority = true — this host is not configured as the spool authority (set spool_authority = true on the [[ledger.zones]] entry)")
	ErrIngestFailures         = errors.New("spool ingest finished with failed segments (left in incoming/)")
	quoteRust, quoteRustSlice = segfile.RustDebugString, segfile.RustDebugStrings
)

// Zone is one [[ledger.zones]] entry resolved for spool work.
type Zone struct {
	ID        string
	SpoolRoot string // "" = spool disabled
	Authority bool
}

// DefaultCursorDir keeps emit cursors with the archive, never in the spool.
func DefaultCursorDir(dataDir string) string {
	return filepath.Join(dataDir, "ledger", "spool-cursors")
}

// ZonesFromConfig ports resolve_zones (spool.rs:150-188). It also refuses a
// spool root that contains the agentsview data directory: the archive's
// SQLite files would then be replicated with the spool tree.
func ZonesFromConfig(cfg config.LedgerConfig, only, dataDir string) ([]Zone, error) {
	if !cfg.Enabled {
		return nil, errors.New("[ledger] enabled = false; enable the ledger before using ledger spool")
	}
	var out []Zone
	ids := cfg.ZoneIDs()
	for _, id := range ids {
		if only != "" && id != only {
			continue
		}
		z, _ := cfg.Zone(id)
		zone := Zone{ID: z.ID, Authority: z.SpoolAuthority}
		if strings.TrimSpace(z.SpoolPath) != "" {
			root, err := pathutil.ExpandHome(z.SpoolPath)
			if err != nil {
				return nil, fmt.Errorf("[ledger.zones] %s spool_path: %w", z.ID, err)
			}
			if err := refuseDataDirInside(root, dataDir); err != nil {
				return nil, err
			}
			zone.SpoolRoot = root
		}
		out = append(out, zone)
	}
	if len(out) == 0 {
		want := "None"
		if only != "" {
			want = "Some(" + quoteRust(only) + ")"
		}
		return nil, fmt.Errorf("no zones matched (--zone %s, configured: %s)", want, quoteRustSlice(ids))
	}
	return out, nil
}

func refuseDataDirInside(spoolRoot, dataDir string) error {
	root, err := pathutil.LocalComparisonKey(spoolRoot)
	if err != nil {
		return err
	}
	data, err := pathutil.LocalComparisonKey(dataDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, data)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("spool_path %s contains the agentsview data directory; SQLite must never be written inside a synced spool tree", spoolRoot)
	}
	return nil
}

// Failure mirrors segfile.IngestFailure with a distinct OpenAPI schema name.
type Failure struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// ZoneIngest is one zone's ingest outcome.
type ZoneIngest struct {
	Zone      string    `json:"zone"`
	State     string    `json:"state"`
	Committed []string  `json:"committed"`
	Skipped   []string  `json:"skipped"`
	Failed    []Failure `json:"failed"`
}

// LedgerSpoolIngestRun is the route body and the CLI's rendering input.
type LedgerSpoolIngestRun struct {
	Zones []ZoneIngest `json:"zones"`
}

// IngestZones ports run_ingest's loop (spool.rs:462-561) minus the index
// refresh. A zone-level I/O error aborts, as jilog's `?` does.
func IngestZones(ctx context.Context, zones []Zone, dst segfile.Appender) (LedgerSpoolIngestRun, error) {
	run := LedgerSpoolIngestRun{Zones: []ZoneIngest{}}
	for _, z := range zones {
		zi := ZoneIngest{Zone: z.ID, Committed: []string{}, Skipped: []string{}, Failed: []Failure{}}
		switch {
		case z.SpoolRoot == "":
			zi.State = IngestStateSpoolDisabled
		case !z.Authority:
			zi.State = IngestStateNotAuthority
		default:
			zi.State = IngestStateIngested
			rep, err := segfile.SpoolIngest(ctx, z.SpoolRoot, z.ID, dst)
			if err != nil {
				return run, fmt.Errorf("ingest spool %s: %w", z.SpoolRoot, err)
			}
			zi.Committed = append(zi.Committed, rep.Committed...)
			zi.Skipped = append(zi.Skipped, rep.Skipped...)
			for _, f := range rep.Failed {
				zi.Failed = append(zi.Failed, Failure{File: f.File, Error: f.Err})
			}
		}
		run.Zones = append(run.Zones, zi)
	}
	return run, nil
}

// Render prints jilog's ingest lines and returns jilog's final error.
func (r LedgerSpoolIngestRun) Render(stdout, _ io.Writer) error {
	ranAny, hadFailures := false, false
	for _, z := range r.Zones {
		switch z.State {
		case IngestStateSpoolDisabled:
			fmt.Fprintf(stdout, "spool ingest [%s]: spool disabled (no spool_path) — not an ingest target\n", z.Zone)
		case IngestStateNotAuthority:
			fmt.Fprintf(stdout, "spool ingest [%s]: spool_authority not set — skipping (producer host?)\n", z.Zone)
		default:
			ranAny = true
			rep := segfile.IngestReport{Committed: z.Committed, Skipped: z.Skipped}
			for _, f := range z.Failed {
				rep.Failed = append(rep.Failed, segfile.IngestFailure{File: f.File, Err: f.Error})
			}
			fmt.Fprintf(stdout, "spool ingest [%s]: %s", z.Zone, rep.Summary())
			hadFailures = hadFailures || len(z.Failed) > 0
		}
	}
	if !ranAny {
		return ErrNotAuthority
	}
	if hadFailures {
		return ErrIngestFailures
	}
	return nil
}

// EmitZones ports run_emit's zone loop (spool.rs:241-456).
func EmitZones(
	ctx context.Context, stdout, stderr io.Writer,
	zones []Zone, src segfile.Lister, source, cursorDir string,
) error {
	if source == "" {
		return errors.New("cannot determine this host's ledger source (installation ID missing); refusing to emit — pass --source explicitly")
	}
	if !ledger.ValidSourceName(source) {
		return fmt.Errorf("invalid source name %s: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (the ingester rejects anything else)", quoteRust(source))
	}
	hadFailures := false
	for _, z := range zones {
		if z.SpoolRoot == "" {
			fmt.Fprintf(stdout, "spool emit [%s]: spool disabled — skipped\n", z.ID)
			continue
		}
		rep, err := segfile.SpoolEmit(ctx, src, z.ID, source, z.SpoolRoot, cursorDir)
		for _, line := range rep.Failures {
			fmt.Fprintln(stderr, line)
		}
		if err != nil {
			return err
		}
		hadFailures = hadFailures || len(rep.Failures) > 0
		fmt.Fprintln(stdout, rep.Line(z.ID, source))
	}
	if hadFailures {
		return segfile.ErrEmitFailures
	}
	return nil
}

// StatusZones ports run_status (spool.rs:567-665).
func StatusZones(stdout io.Writer, zones []Zone, cursorDir, source string) error {
	var unhealthy []string
	for _, z := range zones {
		if z.SpoolRoot == "" {
			fmt.Fprintf(stdout, "spool status [%s]: spool disabled\n", z.ID)
			continue
		}
		line, healthy := segfile.SpoolStatus(z.SpoolRoot, cursorDir, z.ID, source)
		fleet := "(none — producer host)"
		if z.Authority {
			fleet = "(this archive)"
		}
		fmt.Fprintf(stdout, "spool status [%s]: %s fleet_store=%s\n", z.ID, line, fleet)
		if !healthy {
			unhealthy = append(unhealthy, z.ID)
		}
	}
	if len(unhealthy) > 0 {
		return fmt.Errorf("spool status found problems (unreadable dirs/entries, or corrupt cursor) in zone(s): %s",
			strings.Join(unhealthy, ", "))
	}
	return nil
}
