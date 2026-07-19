package duckdb

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// mirrorMarkerSuffix is appended to the mirror path to form the sidecar
// ownership marker's path. The marker deliberately stays a SIBLING of the
// mirror rather than living in the work directory (mirrorWorkDirSuffix):
// it must remain readable while the mirror is locked, and the stale-file
// sweeps only ever delete inside the work directory, so neither sweep can
// touch it.
const mirrorMarkerSuffix = ".agentsview-mirror"

// MirrorMarkerPath returns the path of the sidecar ownership marker for
// the mirror at path. The marker is what lets a probe recognize a mirror it
// cannot open because a DuckDB process holds the file locked: the DuckDB
// lock binds the inode itself, so the locked file's CONTENT cannot be
// inspected at all (even a hardlink alias hits the same inode), but the
// sidecar next to it can always be read, and the locked file's attributes
// can still be statted. The marker is one JSON line recording the schema
// version, machine, write time, and — the part recognition verifies — the
// filesystem identity of the exact mirror file the marker was written for
// (see fileIdentity), so a marker left behind after the mirror is manually
// replaced by a different file no longer recognizes the replacement.
func MirrorMarkerPath(path string) string {
	return path + mirrorMarkerSuffix
}

type mirrorMarkerContent struct {
	SchemaVersion int    `json:"schema_version"`
	Machine       string `json:"machine"`
	WrittenAt     string `json:"written_at"`
	// FileIdentity is the filesystem identity of the mirror file this
	// marker was written for, captured by statting the FINAL mirror path
	// right after the write that produced it. Locked-file recognition
	// requires it to match the file currently at the path (see
	// verifyMirrorMarker).
	FileIdentity fileIdentity `json:"file_identity"`
}

// writeMirrorMarker (re)writes the sidecar ownership marker for the mirror
// at path, binding it to the current file's filesystem identity. A failure
// here is a real push error, not best-effort cleanup: the marker is what
// future probes rely on to recognize this mirror while a serve process
// holds it locked, so a silently missing marker would make the next
// push-under-serve fail closed (see recognizeUninspectableMirror).
func writeMirrorMarker(path, machine string) error {
	identity, err := fileIdentityForPath(path)
	if err != nil {
		return fmt.Errorf("binding duckdb mirror marker to %s: %w", path, err)
	}
	content, err := json.Marshal(mirrorMarkerContent{
		SchemaVersion: SchemaVersion,
		Machine:       machine,
		WrittenAt:     time.Now().UTC().Format(time.RFC3339),
		FileIdentity:  identity,
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

// ensureMirrorMarker writes the marker when it is missing, unparseable, or
// no longer bound to the file currently at path. This heals mirrors created
// by agentsview versions that predate the marker (their next unlocked push
// adds it, so later locked probes recognize the mirror again) and keeps the
// recorded identity self-correcting: a successful push proves the file at
// path is the mirror this push just wrote, so an identity that disagrees —
// which should not normally happen — is rewritten rather than left to fail
// the next locked probe.
func ensureMirrorMarker(path, machine string) error {
	if verifyMirrorMarker(path) == markerVerified {
		return nil
	}
	return writeMirrorMarker(path, machine)
}

// markerVerdict is verifyMirrorMarker's result: how the sidecar marker for
// a mirror path relates to the file currently at that path.
type markerVerdict int

const (
	// markerUnusable: the marker is missing, unreadable, unparseable, or
	// records no identity. A marker without identity fields is simply
	// unverified — this branch is unreleased, so there is deliberately no
	// backward-compatibility shim for identity-less markers; they take the
	// same stop-serve-and-push upgrade path as a missing marker.
	markerUnusable markerVerdict = iota
	// markerIdentityMismatch: the marker parses and records an identity,
	// but the file currently at the path is not the file the marker was
	// written for (for example, the mirror was manually replaced by an
	// unrelated DuckDB database).
	markerIdentityMismatch
	// markerVerified: the marker parses and its recorded identity matches
	// the file currently at the path.
	markerVerified
)

// verifyMirrorMarker reads the sidecar marker for path and checks that its
// recorded file identity matches the file currently at path. It never
// opens the mirror file itself — only the sidecar's content and the mirror
// file's attributes — so it works while a live DuckDB process holds the
// mirror locked.
func verifyMirrorMarker(path string) markerVerdict {
	data, err := os.ReadFile(MirrorMarkerPath(path))
	if err != nil {
		return markerUnusable
	}
	var content mirrorMarkerContent
	if err := json.Unmarshal(data, &content); err != nil {
		return markerUnusable
	}
	if content.FileIdentity.isZero() {
		return markerUnusable
	}
	current, err := fileIdentityForPath(path)
	if err != nil {
		return markerUnusable
	}
	if current != content.FileIdentity {
		return markerIdentityMismatch
	}
	return markerVerified
}
