//go:build windows

package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestRemoteSyncArchiveHTTP_SkipsUnreadableFileOnWindows(t *testing.T) {
	claudeRoot := t.TempDir()
	healthy := filepath.Join(claudeRoot, "-proj-a", "session.jsonl")
	unreadable := filepath.Join(claudeRoot, "-proj-b", "memory", "dysync-troubleshooting.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(healthy), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(unreadable), 0o755))
	require.NoError(t, os.WriteFile(healthy, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-24T12:00:00Z", "healthy remote session").String()), 0o644))
	require.NoError(t, os.WriteFile(unreadable, []byte("restricted note"), 0o644))

	sd, err := windows.SecurityDescriptorFromString("D:P(D;;FR;;;WD)(A;;FA;;;WD)")
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl)
	require.NoError(t, windows.SetNamedSecurityInfo(
		unreadable,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	))
	runtime.KeepAlive(sd)
	t.Cleanup(func() {
		restoreSD, rErr := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
		if rErr == nil {
			if rDACL, _, dErr := restoreSD.DACL(); dErr == nil && rDACL != nil {
				_ = windows.SetNamedSecurityInfo(
					unreadable,
					windows.SE_FILE_OBJECT,
					windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
					nil, nil, rDACL, nil,
				)
			}
			runtime.KeepAlive(restoreSD)
		}
	})

	assertRemoteSyncSkipsUnreadableFileOverHTTP(t, claudeRoot, healthy, unreadable)
}
