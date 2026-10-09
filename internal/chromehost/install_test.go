package chromehost

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLauncher(t *testing.T) {
	for _, tt := range []struct{ platform, executable, socket, body string }{
		{"windows", `C:\bin 100%\agentsview.exe`, `C:\data 100%\host.sock`, "@echo off\r\n\"C:\\bin 100%%\\agentsview.exe\" chrome-host --socket \"C:\\data 100%%\\host.sock\"\r\n"},
		{"linux", "/bin's/agentsview", "/data's/host.sock", "#!/bin/sh\nexec '/bin'\"'\"'s/agentsview' chrome-host --socket '/data'\"'\"'s/host.sock'\n"},
	} {
		t.Run(tt.platform, func(t *testing.T) {
			assert.Equal(t, tt.body, BuildLauncher(tt.executable, tt.socket, tt.platform))
			executable, socket, ok := ParseLauncher(tt.body, tt.platform)
			require.True(t, ok)
			assert.Equal(t, tt.executable, executable)
			assert.Equal(t, tt.socket, socket)
			_, _, ok = ParseLauncher(tt.body+"extra command\n", tt.platform)
			assert.False(t, ok)
		})
	}
}

func TestInstalledRegistration(t *testing.T) {
	for _, state := range []string{"installed", "missing registration", "other manifest", "other data directory", "other launcher", "other socket", "malformed launcher", "missing executable"} {
		t.Run(state, func(t *testing.T) {
			dir, home := t.TempDir(), t.TempDir()
			t.Setenv("CHROME_CONFIG_HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			executable := filepath.Join(t.TempDir(), "agentsview")
			if state != "missing executable" {
				require.NoError(t, os.WriteFile(executable, []byte("program"), 0o700))
			}
			launcher := LauncherPath(dir)
			socket, err := SocketPath(dir)
			require.NoError(t, err)
			if state == "other socket" {
				socket = "other.sock"
			}
			command := BuildLauncher(executable, socket, runtime.GOOS)
			if state == "malformed launcher" {
				command = "invalid"
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
			require.NoError(t, os.WriteFile(launcher, []byte(command), 0o700))
			registered := ManifestPath(dir, home)
			if state == "other manifest" {
				registered = filepath.Join(t.TempDir(), NativeHost+".json")
			}
			if state == "other data directory" {
				launcher = LauncherPath(t.TempDir())
			}
			if state == "other launcher" {
				launcher = filepath.Join(dir, "chrome", "other")
			}
			body, err := json.Marshal(map[string]string{"path": launcher})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(registered), 0o700))
			if state != "missing registration" {
				require.NoError(t, os.WriteFile(registered, body, 0o600))
			}
			assert.Equal(t, state == "installed", Installed(dir, home, registered))
		})
	}
}
