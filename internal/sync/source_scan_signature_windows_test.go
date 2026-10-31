//go:build windows

package sync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSourceScanLongPath(t *testing.T) {
	long := strings.Repeat("d", 260)
	tests := []struct {
		name, path, want string
	}{
		{"short drive path", `C:\sessions\a.jsonl`, `C:\sessions\a.jsonl`},
		{"long drive path", `C:\sessions\` + long, `\\?\C:\sessions\` + long},
		{"long UNC path", `\\server\share\` + long, `\\?\UNC\server\share\` + long},
		{"already extended", `\\?\C:\` + long, `\\?\C:\` + long},
		{"device path", `\\.\pipe\` + long, `\\.\pipe\` + long},
		{"relative path", long, long},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sourceScanLongPath(tt.path))
		})
	}
}
