package duckdb

// fileIdentity is a serializable, comparable filesystem identity for a
// mirror file, used to bind the sidecar ownership marker to the exact file
// it was written for (see writeMirrorMarker and
// recognizeUninspectableMirror). The fields are platform-specific:
//
//   - Unix: A = device, B = inode, C = 0.
//   - Windows: A = volume serial number, B = file index high, C = file
//     index low.
//
// Identities are only ever compared against identities produced on the same
// machine, so the per-platform field meanings never mix. The zero value is
// treated as "no identity recorded" and never matches a real file: real
// Unix identities always carry a non-zero inode and real Windows identities
// a non-zero file index.
type fileIdentity struct {
	A uint64 `json:"a"`
	B uint64 `json:"b"`
	C uint64 `json:"c"`
}

// isZero reports whether no identity was recorded (for example a marker
// written by a pre-identity build of this branch).
func (id fileIdentity) isZero() bool {
	return id == fileIdentity{}
}
