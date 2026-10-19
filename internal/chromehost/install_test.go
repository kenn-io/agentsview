package chromehost

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLauncher(t *testing.T) {
	for _, tt := range []struct{ platform, executable, socket, body string }{
		{"windows", `C:\bin 100%\agentsview.exe`, `C:\data 100%\host.sock`, "@echo off\r\n\"C:\\bin 100%%\\agentsview.exe\" chrome-host --socket \"C:\\data 100%%\\host.sock\"\r\n"},
		{"linux", "/bin's/agentsview", "/data's/host.sock", "#!/bin/sh\nexec '/bin'\"'\"'s/agentsview' chrome-host --socket '/data'\"'\"'s/host.sock'\n"},
	} {
		t.Run(tt.platform, func(t *testing.T) {
			assert.Equal(t, tt.body, BuildLauncher(tt.executable, tt.socket, tt.platform))
		})
	}
}

func TestLauncherTargets(t *testing.T) {
	unix := "#!/bin/sh\nexec '/bin/agentsview' chrome-host --socket '/home/u/av-chrome-0123456789abcdef/host.sock'\n"
	windows := "@echo off\r\n\"C:\\bin\\agentsview.exe\" chrome-host --socket \"C:\\data 100%%\\host.sock\"\r\n"
	for _, tt := range []struct {
		name, launcher, socket, platform string
		want                             bool
	}{
		{"same socket", unix, "/home/u/av-chrome-0123456789abcdef/host.sock", "linux", true},
		{"moved socket", unix, "/var/folders/T/av-chrome-0123456789abcdef/host.sock", "darwin", false},
		{"socket prefix", unix, "/home/u/av-chrome-0123456789abcdef/host", "linux", false},
		{"windows escaping", windows, `C:\data 100%\host.sock`, "windows", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LauncherTargets(tt.launcher, tt.socket, tt.platform))
		})
	}
}

func TestChromeConfigRoot(t *testing.T) {
	for _, tt := range []struct{ name, chrome, xdg, want string }{
		{"Chrome overrides XDG", "chrome-config", "xdg-config", "chrome-config/google-chrome"},
		{"XDG overrides home", "", "xdg-config", "xdg-config/google-chrome"},
		{"home fallback", "", "", "home/.config/google-chrome"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHROME_CONFIG_HOME", tt.chrome)
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			assert.Equal(t, filepath.FromSlash(tt.want), ConfigRoot("home"))
		})
	}
}
