package duckdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// MirrorProbe summarizes a DuckDB mirror file's shape and push-scope
// metadata without mutating it. ProbeMirror is the read-only counterpart to
// rebuildMirror: callers use it to decide whether a mirror is safe to serve
// as-is or needs a full rebuild.
type MirrorProbe struct {
	FileExists       bool
	ShapeOK          bool   // tables+columns+metadata parse
	ShapeIssue       string // human-readable reason when !ShapeOK
	SchemaVersion    int
	DataVersion      int
	Scope            string // canonical scope string, see canonicalPushScope
	LastPushCutoff   string
	LastPushAt       string
	DeletionRevision int64
	IdentityRevision int64
}

// ProbeMirror inspects the mirror file at path without creating or mutating
// it. A missing file is reported as MirrorProbe{} (FileExists false) with a
// nil error; a present-but-unopenable or malformed file is reported with
// ShapeOK false rather than an error, so callers can uniformly decide to
// rebuild instead of threading a distinct error path through every caller.
func ProbeMirror(ctx context.Context, path string) (MirrorProbe, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return MirrorProbe{}, nil
	} else if err != nil {
		return MirrorProbe{}, fmt.Errorf("statting duckdb mirror %s: %w", path, err)
	}

	conn, err := openReadOnlyMirror(path)
	if err != nil {
		return MirrorProbe{FileExists: true, ShapeIssue: err.Error()}, nil
	}
	defer func() { _ = conn.Close() }()

	return probeOpenMirror(ctx, conn), nil
}

// openReadOnlyMirror opens an existing DuckDB file read-only so probing can
// never create or write to it. DuckDB accepts "access_mode=read_only" as a
// DSN query parameter, which the duckdb-go driver forwards to the native
// config; a read-only connection rejects any writer that would otherwise
// race the mirror's owner (a running push or serve process).
func openReadOnlyMirror(path string) (*sql.DB, error) {
	conn, err := openDuckDB(path + "?access_mode=read_only")
	if err != nil {
		return nil, fmt.Errorf("opening duckdb mirror %s read-only: %w", path, err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	return conn, nil
}

func probeOpenMirror(ctx context.Context, conn *sql.DB) MirrorProbe {
	probe := MirrorProbe{FileExists: true}

	shapeIssue, err := mirrorShapeIssue(ctx, conn)
	if err != nil {
		probe.ShapeIssue = err.Error()
		return probe
	}
	if shapeIssue != "" {
		probe.ShapeIssue = shapeIssue
		return probe
	}

	meta, err := readMirrorMetadata(ctx, conn)
	if err != nil {
		probe.ShapeIssue = err.Error()
		return probe
	}

	probe.ShapeOK = true
	probe.SchemaVersion = meta.SchemaVersion
	probe.DataVersion = meta.DataVersion
	probe.Scope = meta.Scope
	probe.LastPushCutoff = meta.LastPushCutoff
	probe.LastPushAt = meta.LastPushAt
	probe.DeletionRevision = meta.DeletionRevision
	probe.IdentityRevision = meta.IdentityRevision
	return probe
}

// mirrorShapeIssue reports the first missing table/column found, or "" when
// the mirror has every table and column mirrorTables declares.
func mirrorShapeIssue(ctx context.Context, conn *sql.DB) (string, error) {
	existing, err := loadColumns(ctx, conn)
	if err != nil {
		return "", err
	}
	var missing []string
	for _, table := range mirrorTables {
		have, ok := existing[table.name]
		if !ok || len(have) == 0 {
			missing = append(missing, "missing table "+table.name)
			continue
		}
		for _, column := range table.columns {
			if !have[column.name] {
				missing = append(missing, table.name+"."+column.name)
			}
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	sort.Strings(missing)
	return "duckdb mirror shape incompatible: " + missing[0], nil
}

// NeedsRebuild reports whether the probed mirror can serve scope/
// sourceDataVersion as-is, or must be rebuilt with rebuildMirror. A missing
// file, a shape problem, a schema version mismatch in either direction, a
// stale source data version, or a different push scope all require a
// rebuild; there is no in-place migration path for mirror schema v3.
func (p MirrorProbe) NeedsRebuild(scope string, sourceDataVersion int) bool {
	if !p.FileExists || !p.ShapeOK {
		return true
	}
	return p.SchemaVersion != SchemaVersion ||
		p.DataVersion != sourceDataVersion ||
		p.Scope != scope
}

// canonicalPushScope renders a push's project filters into a deterministic
// string suitable for storing in mirror metadata and comparing across runs.
// Unfiltered pushes (no include/exclude projects) canonicalize to "" so the
// common case never round-trips through JSON.
func canonicalPushScope(projects, excludeProjects []string) string {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return ""
	}
	scope := struct {
		Projects []string `json:"projects,omitempty"`
		Exclude  []string `json:"exclude,omitempty"`
	}{
		Projects: sortedCopy(projects),
		Exclude:  sortedCopy(excludeProjects),
	}
	data, err := json.Marshal(scope)
	if err != nil {
		// json.Marshal only fails on unsupported types; []string always
		// marshals, so this is unreachable in practice.
		return ""
	}
	return string(data)
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
