//go:build windows

package main

import "testing"

func TestMCPDaemonServiceReusesVerifiedLegacyDaemon(t *testing.T) {
	testMCPDaemonServiceReusesVerifiedDaemon(t, true)
}
