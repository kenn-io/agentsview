package duckdb

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// mirrorMarkerSuffix is appended to the mirror path to form the sidecar
// ownership marker's path. The name deliberately does not match the prefix
// patterns the stale-file sweeps remove (path+".tmp-", path+".reopen-"),
// so neither sweep can ever delete the marker.
const mirrorMarkerSuffix = ".agentsview-mirror"

// MirrorMarkerPath returns the path of the sidecar ownership marker for
// the mirror at path. The marker's EXISTENCE — not its content — is what
// lets a probe recognize a mirror it cannot open because a DuckDB process
// holds the file locked: the DuckDB lock binds the inode itself, so the
// locked file cannot be inspected at all (even a hardlink alias hits the
// same inode), but the sidecar next to it can always be checked. The
// content is informational only: one JSON line recording the schema
// version, machine, and write time of the push that wrote it.
func MirrorMarkerPath(path string) string {
	return path + mirrorMarkerSuffix
}

type mirrorMarkerContent struct {
	SchemaVersion int    `json:"schema_version"`
	Machine       string `json:"machine"`
	WrittenAt     string `json:"written_at"`
}

// writeMirrorMarker (re)writes the sidecar ownership marker for the mirror
// at path. A failure here is a real push error, not best-effort cleanup:
// the marker is what future probes rely on to recognize this mirror while
// a serve process holds it locked, so a silently missing marker would make
// the next push-under-serve fail closed (see recognizeUninspectableMirror).
func writeMirrorMarker(path, machine string) error {
	content, err := json.Marshal(mirrorMarkerContent{
		SchemaVersion: SchemaVersion,
		Machine:       machine,
		WrittenAt:     time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		// json.Marshal of a struct of scalars cannot fail; kept as an
		// error path only so a future content change stays safe.
		return fmt.Errorf("encoding duckdb mirror marker: %w", err)
	}
	content = append(content, '\n')
	markerPath := MirrorMarkerPath(path)
	if err := os.WriteFile(markerPath, content, 0o644); err != nil {
		return fmt.Errorf("writing duckdb mirror marker %s: %w", markerPath, err)
	}
	return nil
}

// ensureMirrorMarker writes the marker only when it is missing. This heals
// mirrors created by agentsview versions that predate the marker: their
// next unlocked push adds it, so later locked probes recognize the mirror
// again.
func ensureMirrorMarker(path, machine string) error {
	if mirrorMarkerExists(path) {
		return nil
	}
	return writeMirrorMarker(path, machine)
}

func mirrorMarkerExists(path string) bool {
	info, err := os.Stat(MirrorMarkerPath(path))
	return err == nil && info.Mode().IsRegular()
}
