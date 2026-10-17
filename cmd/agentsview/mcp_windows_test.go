//go:build windows

package main

import "testing"

func TestMCPDaemonServiceReusesVerifiedLegacyDaemonAtOriginalPath(t *testing.T) {
	testMCPDaemonServiceReusesVerifiedDaemon(t, true)
}
