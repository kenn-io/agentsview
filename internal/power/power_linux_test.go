package power

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnBattery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   map[string]string
		battery bool
	}{
		{name: "desktop"},
		{name: "mouse", files: map[string]string{"mouse/type": "Battery", "mouse/scope": "Device", "mouse/status": "Discharging"}},
		{name: "discharging", files: map[string]string{"AC/type": "Mains", "AC/online": "0", "BAT0/type": "Battery", "BAT0/status": "Discharging"}, battery: true},
		{name: "AC overrides discharging", files: map[string]string{"ZAC/type": "USB_PD", "ZAC/online": "1", "BAT0/type": "Battery", "BAT0/status": "Discharging"}},
		{name: "peripheral USB", files: map[string]string{"USB/type": "USB", "USB/scope": "Device", "USB/online": "1", "BAT0/type": "Battery", "BAT0/status": "Discharging"}, battery: true},
		{name: "programmable USB", files: map[string]string{"USB/type": "USB_C", "USB/scope": "System", "USB/online": "2", "BAT0/type": "Battery", "BAT0/status": "Discharging"}},
		{name: "adjustable USB", files: map[string]string{"USB/type": "USB", "USB/scope": "System", "USB/online": "3", "BAT0/type": "Battery", "BAT0/status": "Discharging"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for path, value := range tc.files {
				path = filepath.Join(root, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(value+"\n"), 0o600))
			}
			t.Cleanup(func() { powerSupplyRoot = "/sys/class/power_supply" })
			powerSupplyRoot = root
			assert.Equal(t, tc.battery, OnBattery())
		})
	}
}
