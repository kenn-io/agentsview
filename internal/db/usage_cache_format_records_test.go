//go:build fts5

package db

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestUsageCacheFormatRecordGuidance(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	files := func(names ...string) fstest.MapFS {
		f := fstest.MapFS{}
		for _, name := range names {
			f[name] = &fstest.MapFile{}
		}
		return f
	}
	for _, tc := range []struct {
		name         string
		records      fstest.MapFS
		version      int
		digest, want string
	}{
		{"missing", files(fmt.Sprintf("22-%s.txt", a)), 23, b, "record digest " + b + " for usage cache format 23"},
		{"changed", files(fmt.Sprintf("22-%s.txt", a)), 22, b, "if this PR added the record, replace it; otherwise bump to format 23"},
		{"duplicate", files(fmt.Sprintf("23-%s.txt", a), fmt.Sprintf("23-%s.txt", b)), 23, a, "bump to format 24"},
		{"newer gap", files(fmt.Sprintf("22-%s.txt", a), fmt.Sprintf("25-%s.txt", b)), 22, a, "format 26"},
		{"same output after bump", files(fmt.Sprintf("22-%s.txt", a), fmt.Sprintf("23-%s.txt", a)), 23, a, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkUsageCacheFormatRecords(tc.records, tc.version, tc.digest)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
