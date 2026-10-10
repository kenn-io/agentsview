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
