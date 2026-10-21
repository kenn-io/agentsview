package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveSourceConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, input, wantError string
	}{
		{"explicit snapshot", `[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop"
snapshot = "selected-snapshot"
`, ""},
		{"missing snapshot", `[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop"
`, "snapshot"},
		{"missing repository", `[[archive_sources]]
name = "retired-desktop"
snapshot = "selected-snapshot"
`, "repository"},
		{"missing name", `[[archive_sources]]
repository = "/mnt/archives/desktop"
snapshot = "selected-snapshot"
`, "name"},
		{"duplicate name", `[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop"
snapshot = "selected-snapshot"
[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/other"
snapshot = "other-snapshot"
`, "duplicate"},
		{"misspelled option", `[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop"
snapshot = "selected-snapshot"
snapshop = "other-snapshot"
`, "snapshop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			err := cfg.applyConfigTOML(tc.input)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []ArchiveSource{{
				Name: "retired-desktop", Repository: "/mnt/archives/desktop", Snapshot: "selected-snapshot",
			}}, cfg.ArchiveSources)
		})
	}
}

func TestArchiveSourceExpandsRepositoryHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{ArchiveSources: []ArchiveSource{{Name: "desktop", Repository: "~/archive", Snapshot: "snapshot"}}}
	require.NoError(t, expandLocalPaths(&cfg))
	assert.Equal(t, filepath.Join(os.Getenv("HOME"), "archive"), cfg.ArchiveSources[0].Repository)
}

func TestArchiveSourceDirectoryAndMachineName(t *testing.T) {
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML(`[[archive_sources]]
name = "saved-history"
directory = "/mnt/archives/restored"
machine_name = "Previous workstation"
`))
	assert.Equal(t, "/mnt/archives/restored", cfg.ArchiveSources[0].Directory)
	cfg.ArchiveSources[0].Repository = "/mnt/archives/repository"
	require.ErrorContains(t, cfg.ValidateArchiveSources(), "either")
	require.ErrorContains(t, cfg.applyConfigTOML(`[[archive_sources]]
name = "saved-history"
directory = "/mnt/archives/restored"
machine_name = "   "
`), "machine_name")
}
