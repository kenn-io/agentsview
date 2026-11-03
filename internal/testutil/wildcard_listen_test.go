package testutil

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWildcardListenAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		ci   string
		want bool
	}{
		{"forced on", "1", "", true},
		{"forced off", "0", "true", false},
		{"forced off beats CI", "off", "true", false},
		{"unset on CI", "", "true", true},
		{"unset off CI", "", "", runtime.GOOS != "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(WildcardListenEnv, tc.env)
			t.Setenv("CI", tc.ci)
			require.Equal(t, tc.want, WildcardListenAllowed())
		})
	}
}
