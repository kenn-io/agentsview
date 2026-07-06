package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCwdPrefixFilterAllows(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		cwd      string
		want     bool
	}{
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
		{"windows separator boundary", []string{`C:\work`}, `C:\work\repo`, true},
		{"windows sibling", []string{`C:\work`}, `C:\workspace`, false},
		{"blank entries ignored", []string{"  ", ""}, "/anywhere", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCwdPrefixFilter(tt.prefixes)
			assert.Equal(t, tt.want, f.allows(tt.cwd))
		})
	}
}
