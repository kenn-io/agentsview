//go:build windows

package timeutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/thlib/go-timezone-local/tzlocal"
)

func TestBestEffortLocalTimezoneRuntime(t *testing.T) {
	mapped, err := tzlocal.LocalTZ()
	require.NoError(t, err)
	require.NotEmpty(t, mapped)
	_, err = time.LoadLocation(mapped)
	require.NoError(t, err)
	t.Logf("Windows local identity %q maps to loadable IANA zone %q", time.Local.String(), mapped)
}
