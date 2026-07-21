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
	// LockConflict is true when ShapeIssue is specifically a conflicting
	// hold on the file rather than a genuinely malformed or incompatible
	// one: a cross-process DuckDB lock conflict (see
	// isMirrorLockConflictError) or duckdb-go's same-process double-open
	// rejection (see isMirrorOpenInSameProcessError). Serve processes hold
	// the mirror read-only, and read-only handles coexist with the probe's
	// own read-only open, so a probe-time conflict means a WRITER holds the
	// file: another push in flight, or a serve process from a build that
	// predates the read-only serve change. Push fails closed on it.
	LockConflict bool
	// RecognizedMirror is true when the existing file is positively
	// identified as an agentsview DuckDB mirror: it carries the agentsview
	// sentinel, a sync_metadata(key, value) table containing the
	// schemaVersionMetadataKey row, which every mirror generation writes at
	// schema creation (see createSchema). Generic table names alone are not
	// enough — a foreign DuckDB database that happens to have a table named
	// "sessions" or "sync_metadata" must never be recognized, because
	// recognition lets a rebuild atomically overwrite the file. A
	// wrong-schema-version or shape-incompatible mirror still carries the
	// sentinel and is still recognized. Push refuses to rebuild over an
	// existing unrecognized file so a misdirected path (for example the
	// primary SQLite archive) is never silently replaced by a mirror
	// rebuild.
	RecognizedMirror bool
	SchemaVersion    int
	DataVersion      int
	// SourceDatabaseID is the database_id of the SQLite archive generation
	// the mirror was built from (see mirrorMetadata.SourceDatabaseID). "" on
	// mirrors written before the id was recorded.
	SourceDatabaseID string
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
// The probe opens read-only, so it coexists with read-only serve handles on
// the same file; only a writer's exclusive lock makes a file unopenable
// (see MirrorProbe.LockConflict).
func ProbeMirror(ctx context.Context, path string) (MirrorProbe, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return MirrorProbe{}, nil
	} else if err != nil {
		return MirrorProbe{}, fmt.Errorf("statting duckdb mirror %s: %w", path, err)
	}

	conn, err := OpenReadOnly(path)
	if err != nil {
		return MirrorProbe{
			FileExists:   true,
			ShapeIssue:   err.Error(),
			LockConflict: isMirrorHeldError(err),
		}, nil
	}
	defer func() { _ = conn.Close() }()

	return probeOpenMirror(ctx, conn), nil
}

// isMirrorHeldError reports whether err means a live handle blocks opening
// the file: a cross-process lock conflict or duckdb-go's same-process
// double-open rejection. Any other error means the file is simply not an
// openable DuckDB database.
func isMirrorHeldError(err error) bool {
	return isMirrorLockConflictError(err) || isMirrorOpenInSameProcessError(err)
}

// isMirrorLockConflictError reports whether err indicates DuckDB refused to
// open the mirror because another process holds a conflicting lock on it.
// Serve processes hold the mirror read-only and coexist with other
// read-only opens, so a read-only open (probe, serve) only hits this when a
// WRITER holds the file, and a write open (incremental push) hits it while
// any read-only holder exists. Rebuild-into-temp-then-rename still works
// while readers hold the file because their locks bind the inode they
// opened, not the path string: swapMirrorFile's rename replaces what the
// path points at without touching that inode, so a serving process keeps
// running against its (now unlinked but still open) old handle until it
// notices the replacement and reopens.
func isMirrorLockConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Could not set lock") ||
		strings.Contains(msg, "Conflicting lock")
}

// isMirrorOpenInSameProcessError reports whether err is duckdb-go's
// same-process double-open rejection ("Can't open a connection to same
// database file with a different configuration than existing connections"):
// duckdb-go caches instances per literal DSN, so opening the same path with
// a DIFFERENT access mode than an existing in-process handle is rejected. A
// read-only probe hits it when the same process holds the mirror read-write
// (a push in flight), and a push's write open hits it when the same process
// holds the mirror read-only (a serving Store). Same-DSN opens share the
// cached instance instead and never hit this.
func isMirrorOpenInSameProcessError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "same database file")
}

func probeOpenMirror(ctx context.Context, conn *sql.DB) MirrorProbe {
	probe := MirrorProbe{FileExists: true}

	existing, err := loadColumns(ctx, conn)
	if err != nil {
		probe.ShapeIssue, probe.LockConflict = classifyProbeError(err)
		return probe
	}
	// Recognition is looser than shape validity (a mirror from another
	// schema version is still ours and safe to rebuild over) but stricter
	// than generic table names: it requires the agentsview sentinel row.
	// See MirrorProbe.RecognizedMirror and hasMirrorSentinel.
	recognized, err := hasMirrorSentinel(ctx, conn, existing)
	if err != nil {
		probe.ShapeIssue, probe.LockConflict = classifyProbeError(err)
		return probe
	}
	probe.RecognizedMirror = recognized

	if shapeIssue := mirrorShapeIssueFromColumns(existing); shapeIssue != "" {
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
	probe.SourceDatabaseID = meta.SourceDatabaseID
	probe.Scope = meta.Scope
	probe.LastPushCutoff = meta.LastPushCutoff
	probe.LastPushAt = meta.LastPushAt
	probe.LastPushMachine = meta.LastPushMachine
	probe.DeletionRevision = meta.DeletionRevision
	probe.IdentityRevision = meta.IdentityRevision
	return probe
}

// hasMirrorSentinel reports whether the open database carries the
// agentsview mirror sentinel: a sync_metadata table with key and value
// columns that contains the schemaVersionMetadataKey row. createSchema has
// always written that row as part of creating a mirror, so every mirror —
// current or from an older schema generation — carries it, while a foreign
// DuckDB database with generically named tables does not. existing is the
// column map loadColumns already produced, so a database without a
// sync_metadata(key, value) table is rejected without running any query.
func hasMirrorSentinel(
	ctx context.Context, conn *sql.DB, existing map[string]map[string]bool,
) (bool, error) {
	meta := existing["sync_metadata"]
	if !meta["key"] || !meta["value"] {
		return false, nil
	}
	var found bool
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) > 0 FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&found); err != nil {
		return false, fmt.Errorf("checking duckdb mirror sentinel: %w", err)
	}
	return found, nil
}

// classifyProbeError converts an error encountered while querying an
// already-open mirror connection (inside loadColumns or
// readMirrorMetadata) into the ShapeIssue/LockConflict pair MirrorProbe
// reports. duckdb-go opens connections lazily, so a lock conflict often
// does not surface from OpenReadOnly itself but only once the first real
// query runs against the connection; without routing that error through
// isMirrorHeldError here as well, it would degrade to a generic shape issue
// instead of being recognized as a lock conflict.
func classifyProbeError(err error) (shapeIssue string, lockConflict bool) {
	if err == nil {
		return "", false
	}
	return err.Error(), isMirrorHeldError(err)
}

// mirrorShapeIssueFromColumns reports the first missing table/column found
// in an already-loaded column map, or "" when the mirror has every table
// and column mirrorTables declares.
func mirrorShapeIssueFromColumns(existing map[string]map[string]bool) string {
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
		return ""
	}
	sort.Strings(missing)
	return "duckdb mirror shape incompatible: " + missing[0]
}

// NeedsRebuild reports whether the probed mirror can serve scope/
// sourceDataVersion as-is, or must be rebuilt with rebuildMirror. A missing
// file, a shape problem, a schema version mismatch in either direction, a
// stale source data version, or a different push scope all require a
// rebuild; there is no in-place migration path for mirror schema v4.
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
// specific "why", plus three conditions NeedsRebuild does not see: the
// mirror having been last pushed by a different machine name than the one
// pushing now, the mirror having been built from a different SQLite archive
// generation than the one pushing now (see the source-database-id paragraph
// below), and the mirror's deletion journal cursor sitting ahead of the
// local archive's own counter, which happens when the local archive was
// rebuilt or replaced (e.g. by a resync) and its deletion journal no longer
// covers the range the mirror already advanced past — applying a delta in
// that state would otherwise fail with an invalid publication window rather
// than just rebuilding.
//
// The source-database-id check exists because every incremental bookkeeping
// token the mirror stores (push cutoff, deletion and identity revisions) is
// only meaningful relative to the archive that produced it. A mirror built
// from a DIFFERENT archive whose versions, scope, and machine happen to
// coincide — and whose deletion revision is not behind — would otherwise
// take the incremental path: sessions unique to the old archive persist
// forever, and the new archive's sessions with sync_markers below the
// stored cutoff are never copied. A resync deliberately generates a new
// database_id for the fresh archive (internal/db/orphaned.go excludes the
// key from the metadata it carries over), so the first push after ANY
// resync is a full rebuild — paralleling the PostgreSQL push's
// database_id-scoped cursors (the deletion-cursor check below remains as
// defense in depth for same-archive paths). A recorded EMPTY id also
// rebuilds: identity-less mirrors only come from earlier builds of this
// unreleased branch, so they simply rebuild once and record the id.
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
// read, and localDatabaseID the caller's local.GetDatabaseID read; both are
// passed in rather than threaded through NeedsRebuild's pure scope/version
// signature.
func rebuildReason(
	probe MirrorProbe, scope string, sourceDataVersion int, full bool,
	localDeletionRevision int64, machine string, localDatabaseID string,
) string {
	switch {
	case full:
		return "--full requested"
	case !probe.FileExists:
		return "missing file"
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
	case probe.SourceDatabaseID != localDatabaseID:
		return "mirror was built from a different archive (source database id changed)"
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
