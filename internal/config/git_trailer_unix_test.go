//go:build unix

package config

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadGitTrailerRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "config.toml"), 0o600))
	_, err := LoadGitTrailer()
	require.Error(t, err)
}
