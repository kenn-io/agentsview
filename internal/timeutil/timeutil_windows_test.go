//go:build windows

package timeutil

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/thlib/go-timezone-local/tzlocal"
)

func TestBestEffortLocalTimezoneRuntime(t *testing.T) {
	t.Setenv("TZ", os.Getenv("TZ"))
	require.NoError(t, os.Unsetenv("TZ"))

	nativeMapped, err := tzlocal.LocalTZ()
	require.NoError(t, err)
	require.NotEmpty(t, nativeMapped)
	mapped := BestEffortLocalTimezone()
	require.Equal(t, nativeMapped, mapped)
	_, err = time.LoadLocation(mapped)
	require.NoError(t, err)
	t.Logf("Windows local identity %q maps to loadable IANA zone %q", time.Local.String(), mapped) //nolint:forbidigo // Exercise the operating system timezone used for local date input and display.
}
