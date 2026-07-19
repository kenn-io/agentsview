package duckdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// MirrorProbe summarizes a DuckDB mirror file's shape and push-scope
// metadata without mutating it. ProbeMirror is the read-only counterpart to
// rebuildMirror: callers use it to decide whether a mirror is safe to serve
// as-is or needs a full rebuild.
type MirrorProbe struct {
	FileExists bool
	ShapeOK    bool   // tables+columns+metadata parse
	ShapeIssue string // human-readable reason when !ShapeOK
	// LockConflict is true when ShapeIssue is specifically a DuckDB
	// cross-process lock conflict (another process, typically a running
	// 'duckdb serve' or 'duckdb quack serve', already has the mirror file
	// open) rather than a genuinely malformed or incompatible file. See
	// isMirrorLockConflictError.
	LockConflict     bool
	SchemaVersion    int
	DataVersion      int
	Scope            string // canonical scope string, see canonicalPushScope
	LastPushCutoff   string
	LastPushAt       string
	LastPushMachine  string
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
		return MirrorProbe{
			FileExists:   true,
			ShapeIssue:   err.Error(),
			LockConflict: isMirrorLockConflictError(err),
		}, nil
	}
	defer func() { _ = conn.Close() }()

	return probeOpenMirror(ctx, conn), nil
}

// isMirrorLockConflictError reports whether err indicates DuckDB refused to
// open the mirror because another process already holds it open — the
// signature of a running 'duckdb serve' or 'duckdb quack serve'. DuckDB is
// single-writer/exclusive across processes: even a read-only open cannot
// coexist with another process's handle on the same file, so while the
// mirror is served, incremental update is impossible and rebuild is the
// only way to make progress. Rebuild-into-temp-then-rename still works
// during this window because the lock the serving process holds binds the
// inode it opened, not the path string: swapMirrorFile's rename replaces
// what the path points at without touching that inode, so the serving
// process keeps running against its (now unlinked but still open) old
// handle until it notices the replacement and reopens.
func isMirrorLockConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Could not set lock") ||
		strings.Contains(msg, "Conflicting lock")
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
		probe.ShapeIssue, probe.LockConflict = classifyProbeError(err)
		return probe
	}
	if shapeIssue != "" {
		probe.ShapeIssue = shapeIssue
		return probe
	}

	meta, err := readMirrorMetadata(ctx, conn)
	if err != nil {
		probe.ShapeIssue, probe.LockConflict = classifyProbeError(err)
		return probe
	}

	probe.ShapeOK = true
	probe.SchemaVersion = meta.SchemaVersion
	probe.DataVersion = meta.DataVersion
	probe.Scope = meta.Scope
	probe.LastPushCutoff = meta.LastPushCutoff
	probe.LastPushAt = meta.LastPushAt
	probe.LastPushMachine = meta.LastPushMachine
	probe.DeletionRevision = meta.DeletionRevision
	probe.IdentityRevision = meta.IdentityRevision
	return probe
}

// classifyProbeError converts an error encountered while querying an
// already-open mirror connection (inside mirrorShapeIssue or
// readMirrorMetadata) into the ShapeIssue/LockConflict pair MirrorProbe
// reports. duckdb-go opens connections lazily, so a cross-process lock
// conflict often does not surface from openReadOnlyMirror itself but only
// once the first real query runs against the connection; without routing
// that error through isMirrorLockConflictError here as well, it would
// degrade to a generic shape issue instead of being recognized as a lock
// conflict (see isMirrorLockConflictError and rebuildReason).
func classifyProbeError(err error) (shapeIssue string, lockConflict bool) {
	if err == nil {
		return "", false
	}
	return err.Error(), isMirrorLockConflictError(err)
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

// rebuildReason returns a human-readable explanation for why probe forces a
// rebuild instead of an incremental push, or "" when an incremental push
// can proceed. It is the diagnostic/logging counterpart to NeedsRebuild:
// every condition NeedsRebuild's bool contract covers (missing/damaged
// file, schema/data version drift, scope change) is reported here with the
// specific "why", plus three conditions NeedsRebuild does not see: a
// cross-process lock conflict (LockConflict), the mirror having been last
// pushed by a different machine name than the one pushing now, and the
// mirror's deletion journal cursor sitting ahead of the local archive's own
// counter, which happens when the local archive was rebuilt or replaced
// (e.g. by a resync) and its deletion journal no longer covers the range
// the mirror already advanced past — applying a delta in that state would
// otherwise fail with an invalid publication window rather than just
// rebuilding.
//
// The machine-change check exists because mirror rows are machine-stamped
// (see the sessions.machine column and duckSessionFingerprintFields): an
// incremental push only rewrites sessions whose LOCAL content changed
// within the current mirror window, so a session that has not changed
// since the mirror's last push stays permanently labeled with the OLD
// machine name even after the push metadata's LastPushMachine flips to the
// new one — silently stranding it under a machine filter (see
// readMachineStatus) that will never again select it. A full rebuild
// re-pushes every session under the new machine name instead.
//
// localDeletionRevision is the caller's local.SessionDeletionPublicationRevision
// read, passed in rather than threaded through NeedsRebuild's pure
// scope/version signature.
func rebuildReason(
	probe MirrorProbe, scope string, sourceDataVersion int, full bool,
	localDeletionRevision int64, machine string,
) string {
	switch {
	case full:
		return "--full requested"
	case !probe.FileExists:
		return "missing file"
	case probe.LockConflict:
		return "mirror locked by another process — likely a running serve; " +
			"rebuilding from scratch because incremental update cannot " +
			"proceed while it is served"
	case !probe.ShapeOK:
		return "shape issue: " + probe.ShapeIssue
	case probe.SchemaVersion != SchemaVersion:
		return fmt.Sprintf(
			"schema version %d vs %d", probe.SchemaVersion, SchemaVersion,
		)
	case probe.DataVersion != sourceDataVersion:
		return fmt.Sprintf(
			"data version %d vs %d", probe.DataVersion, sourceDataVersion,
		)
	case probe.Scope != scope:
		return "scope changed"
	case probe.LastPushMachine != "" && probe.LastPushMachine != machine:
		return fmt.Sprintf(
			"machine name changed from %s to %s", probe.LastPushMachine, machine,
		)
	case probe.DeletionRevision > localDeletionRevision:
		return "mirror deletion cursor ahead of archive; archive was rebuilt"
	default:
		return ""
	}
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
