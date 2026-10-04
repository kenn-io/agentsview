//go:build fts5 && windows

package db

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thlib/go-timezone-local/tzlocal"
	"go.kenn.io/agentsview/internal/timeutil"
)

// Go ignores TZ on Windows, so every TZ value that names no zone must still
// resolve the system zone instead of leaving the default request unnamed.
func TestUsageDefaultLocationUsesMappedWindowsZone(t *testing.T) {
	mapped, err := tzlocal.LocalTZ()
	require.NoError(t, err)
	for _, tt := range []struct {
		name  string
		tz    string
		unset bool
	}{
		{name: "unset", unset: true},
		{name: "empty"},
		{name: "unknown", tz: "Not/AZone"},
		{name: "Local", tz: "Local"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TZ", tt.tz)
			if tt.unset {
				require.NoError(t, os.Unsetenv("TZ"))
			}
			setUsageLocalLocation(t, timeutil.LocalLocation())

			location := UsageFilter{}.Location()

			assert.Equal(t, mapped, location.String())
			key := usageTimezoneIdentityFor(location, nil).Key
			assert.False(t, strings.HasPrefix(key, "local:"), key)
		})
	}
}
