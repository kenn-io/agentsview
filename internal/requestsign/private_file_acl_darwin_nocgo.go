//go:build darwin && !cgo

package requestsign

import "os"

// Darwin ACL inspection requires the CGO-enabled build used by AgentsView.
func hasPrivateFileACL(*os.File) bool { return false }
