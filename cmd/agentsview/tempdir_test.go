package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setTempDirEnv points every temp-dir variable os.TempDir reads on any
// platform at dir, restoring them when the test ends.
func setTempDirEnv(t *testing.T, dir string) {
	t.Helper()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "SystemTemp"} {
		t.Setenv(key, dir)
	}
}

func TestEnsureUsableTempDir(t *testing.T) {
	tests := []struct {
		name       string
		tempDir    func(root string) string
		dataDir    func(t *testing.T, root string) string
		wantTemp   func(root, inherited, dataDir string) string
		wantOutput []string
	}{
		{
			name:    "writable temp dir is kept",
			tempDir: func(root string) string { return root },
			dataDir: func(_ *testing.T, root string) string {
				return filepath.Join(root, "data")
			},
			wantTemp: func(_, inherited, _ string) string { return inherited },
		},
		{
			name: "missing temp dir falls back to data dir",
			tempDir: func(root string) string {
				return filepath.Join(root, "missing")
			},
			dataDir: func(_ *testing.T, root string) string {
				return filepath.Join(root, "data")
			},
			wantTemp: func(_, _, dataDir string) string {
				return filepath.Join(dataDir, "tmp")
			},
			wantOutput: []string{"missing", "not writable", filepath.Join("data", "tmp")},
		},
		{
			name: "unusable fallback keeps inherited temp dir",
			tempDir: func(root string) string {
				return filepath.Join(root, "missing")
			},
			dataDir: func(t *testing.T, root string) string {
				t.Helper()
				// A regular file where the data directory should be makes
				// creating <data>/tmp fail.
				path := filepath.Join(root, "data")
				require.NoError(t, os.WriteFile(path, nil, 0o600))
				return path
			},
			wantTemp: func(_, inherited, _ string) string { return inherited },
			wantOutput: []string{
				"missing", "fallback", filepath.Join("data", "tmp"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			inherited := tt.tempDir(root)
			dataDir := tt.dataDir(t, root)
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			setTempDirEnv(t, inherited)

			var stderr bytes.Buffer
			ensureUsableTempDir(&stderr)

			want := tt.wantTemp(root, inherited, dataDir)
			// The redirect is what is under test, so read the process temp
			// directory instead of the test's own.
			assert.Equal(t, want, os.TempDir()) //nolint:usetesting // asserts the redirect
			if len(tt.wantOutput) == 0 {
				assert.Empty(t, stderr.String())
			}
			for _, fragment := range tt.wantOutput {
				assert.Contains(t, stderr.String(), fragment)
			}
			if want != inherited {
				f, err := os.CreateTemp("", "after-redirect-*") //nolint:usetesting // asserts the redirect
				require.NoError(t, err)
				require.NoError(t, f.Close())
				assert.Equal(t, want, filepath.Dir(f.Name()))
			}
		})
	}
}

func TestEnsureUsableTempDirResolvesRelativeDataDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("AGENTSVIEW_DATA_DIR", "data")
	setTempDirEnv(t, filepath.Join(root, "missing"))

	var stderr bytes.Buffer
	ensureUsableTempDir(&stderr)

	got := os.TempDir() //nolint:usetesting // asserts the redirect
	require.True(t, filepath.IsAbs(got), "temp dir %q is relative", got)
	gotInfo, err := os.Stat(got)
	require.NoError(t, err)
	wantInfo, err := os.Stat(filepath.Join(root, "data", "tmp"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(wantInfo, gotInfo),
		"temp dir %q is not <cwd>/data/tmp", got)
}
