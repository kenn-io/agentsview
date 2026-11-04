package testutil

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireWildcardListen(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		ci   string
		want bool
	}{
		{"forced on", "1", "", true},
		{"forced off", "0", "true", false},
		{"unset on CI", "", "true", true},
		{"unset off CI", "", "", runtime.GOOS != "windows"},
		{"word override ignored", "off", "true", true},
		{"padded override ignored", " 0 ", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(WildcardListenEnv, tc.env)
			t.Setenv("CI", tc.ci)
			ran := false
			t.Run("listener", func(t *testing.T) {
				RequireWildcardListen(t)
				ran = true
			})
			require.Equal(t, tc.want, ran, "whether code after RequireWildcardListen runs")
		})
	}
}
