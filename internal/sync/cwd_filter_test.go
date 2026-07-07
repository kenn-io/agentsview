package sync

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

type cwdFilterCase struct {
	name     string
	prefixes []string
	cwd      string
	want     bool
}

func TestCwdPrefixFilterAllows(t *testing.T) {
	tests := []cwdFilterCase{
		{"empty filter allows anything", nil, "/anywhere", true},
		{"empty filter allows empty cwd", nil, "", true},
		{"exact match", []string{"/a/b"}, "/a/b", true},
		{"child path", []string{"/a/b"}, "/a/b/c/d", true},
		{"sibling with shared prefix", []string{"/a/b"}, "/a/bc", false},
		{"outside prefix", []string{"/a/b"}, "/x", false},
		{"empty cwd rejected when filter set", []string{"/a/b"}, "", false},
		{"second prefix matches", []string{"/a/b", "/x/y"}, "/x/y/z", true},
		{"trailing separator normalized", []string{"/a/b/"}, "/a/b/c", true},
		{"prefix longer than cwd", []string{"/a/b/c"}, "/a/b", false},
		{"case sensitive", []string{"/a/B"}, "/a/b/c", false},
		{"blank entries ignored", []string{"  ", ""}, "/anywhere", true},
		{"root prefix allows any cwd", []string{"/"}, "/anywhere", true},
		{"dot-dot escaping the prefix rejected", []string{"/a/b"}, "/a/b/../c", false},
		{"dot-dot staying inside allowed", []string{"/a/b"}, "/a/b/c/../d", true},
		{"dot-dot in prefix cleaned", []string{"/a/b/../c"}, "/a/c/d", true},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			cwdFilterCase{"backslash boundary", []string{`C:\work`}, `C:\work\repo`, true},
			cwdFilterCase{"drive sibling", []string{`C:\work`}, `C:\workspace`, false},
			cwdFilterCase{"mixed separators normalized", []string{`C:/work`}, `C:\work\repo`, true},
		)
	} else {
		// On POSIX a backslash is an ordinary filename character:
		// "b\evil" is a sibling of "b" under /a, not a child of /a/b.
		tests = append(tests, cwdFilterCase{
			"backslash is not a separator", []string{"/a/b"}, `/a/b\evil`, false,
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCwdPrefixFilter(tt.prefixes)
			assert.Equal(t, tt.want, f.allows(tt.cwd))
		})
	}
}
