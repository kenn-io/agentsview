//go:build !windows

package parser

import "testing"

func waitForCodexThreadDirectoryChange(_ *testing.T, _ *codexThreadDirectory) {}
