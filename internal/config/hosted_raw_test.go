package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestHostedRawConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", "")
	hegel.Test(t, func(ht *hegel.T) {
		fileValue := hegel.Draw(ht, hegel.Booleans())
		env := hegel.Draw(ht, hegel.SampledFrom([]string{"", "true", "1", "false", "0", "TRUE", "True", "t", "T", "FALSE", "False", "f", "F"}))
		t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", env)
		require.NoError(ht, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[raw_sync]\nhosted = %t\n", fileValue)), 0o600))
		cfg, err := LoadReadOnlyFor(ScopeLocal)
		require.NoError(ht, err)
		want := fileValue
		if env != "" {
			want, _ = strconv.ParseBool(env)
		}
		require.Equal(ht, want, cfg.HostedRawMode())
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHostedRawConfigDefault(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", "")
	cfg, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err)
	require.False(t, cfg.HostedRawMode())
}

func TestHostedRawConfigRejectsInvalidEnvironment(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", "")
	t.Run("invalid-2-regression", func(t *testing.T) {
		t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", "2")
		_, err := LoadReadOnlyFor(ScopeLocal)
		require.ErrorContains(t, err, "AGENTSVIEW_RAW_SYNC_HOSTED")
	})
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		// NUL cannot be represented in an operating-system environment value.
		if strings.ContainsRune(value, 0) {
			return
		}
		if _, err := strconv.ParseBool(value); err == nil || value == "" {
			return
		}
		t.Setenv("AGENTSVIEW_RAW_SYNC_HOSTED", value)
		_, err := LoadReadOnlyFor(ScopeLocal)
		require.ErrorContains(ht, err, "AGENTSVIEW_RAW_SYNC_HOSTED")
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}
