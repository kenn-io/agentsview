//go:build !windows && !darwin

package requestsign

import "os"

func hasPrivateFileACL(*os.File) bool { return true }
