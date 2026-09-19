package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportProfileFlagsAreHidden(t *testing.T) {
	commands := []*cobra.Command{
		newExportSessionsCommand(),
		newExportHourCommand(defaultExportReportingDeps()),
		newExportDayCommand(defaultExportReportingDeps()),
		newExportDigestCommand(defaultExportReportingDeps()),
	}
	for _, command := range commands {
		for _, name := range []string{"cpuprofile", "memprofile", "trace"} {
			flag := command.Flags().Lookup(name)
			require.NotNil(t, flag)
			assert.True(t, flag.Hidden)
		}
	}
}

func TestExportProfileInvalidPath(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T)
		root func() *cobra.Command
		args []string
	}{
		{"sessions", func(t *testing.T) { t.Helper(); seedExportSessionsArchive(t) }, newRootCommand, []string{"export", "sessions"}},
		{"hour", seedExportReportingArchive, func() *cobra.Command {
			return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
		}, []string{"export", "hour", "2026-07-28-10"}},
		{"day", seedExportReportingArchive, func() *cobra.Command {
			return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
		}, []string{"export", "day", "2026-07-28"}},
		{"digest", seedExportReportingArchive, func() *cobra.Command {
			return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
		}, []string{"export", "digest", "--from", "2026-07-28", "--to", "2026-07-28"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.seed(t)
			dir := t.TempDir()
			badParent := filepath.Join(dir, "parent")
			require.NoError(t, os.WriteFile(badParent, []byte("file"), 0o600))
			args := append([]string{}, tt.args...)
			args = append(args, "--cpuprofile", filepath.Join(badParent, "cpu"), "--memprofile", filepath.Join(badParent, "mem"), "--trace", filepath.Join(badParent, "trace"))
			stdout, stderr, err := executeExportSessionsCommand(tt.root(), args...)
			require.NoError(t, err)
			assert.NotEmpty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

func TestExportProfileSuccess(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T)
		root func() *cobra.Command
		args []string
	}{
		{
			"sessions", func(t *testing.T) { t.Helper(); seedExportSessionsArchive(t) }, newRootCommand,
			[]string{"export", "sessions"},
		},
		{
			"hour", seedExportReportingArchive,
			func() *cobra.Command {
				return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
			},
			[]string{"export", "hour", "2026-07-28-10"},
		},
		{
			"day", seedExportReportingArchive,
			func() *cobra.Command {
				return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
			},
			[]string{"export", "day", "2026-07-28"},
		},
		{
			"digest", seedExportReportingArchive,
			func() *cobra.Command {
				return newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
			},
			[]string{"export", "digest", "--from", "2026-07-28", "--to", "2026-07-28"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.seed(t)
			dir := t.TempDir()
			args := append([]string{}, tt.args...)
			paths := []string{filepath.Join(dir, "cpu"), filepath.Join(dir, "mem"), filepath.Join(dir, "trace")}
			args = append(args, "--cpuprofile", paths[0], "--memprofile", paths[1], "--trace", paths[2])
			stdout, stderr, err := executeExportSessionsCommand(tt.root(), args...)
			require.NoError(t, err)
			assert.NotEmpty(t, stdout)
			assert.Empty(t, stderr)
			for _, path := range paths {
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Positive(t, info.Size())
			}
		})
	}
}

func TestExportProfileFailureCleanup(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"sessions", []string{"export", "sessions", "--json", "--format", "ndjson"}, "--json cannot be combined"},
		{"hour", []string{"export", "hour", "--schema-version", "99", "2026-07-28-10"}, "unsupported reporting schema version"},
		{"day", []string{"export", "day", "--schema-version", "99", "2026-07-28"}, "unsupported reporting schema version"},
		{"digest", []string{"export", "digest", "--from", "2026-07-29", "--to", "2026-07-28"}, "--from must not be after --to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := []string{filepath.Join(dir, "cpu"), filepath.Join(dir, "mem"), filepath.Join(dir, "trace")}
			args := append([]string{}, tt.args...)
			args = append(args, "--cpuprofile", paths[0], "--memprofile", paths[1], "--trace", paths[2])
			root := newRootCommand()
			if tt.name != "sessions" {
				root = newExportReportingTestRoot(time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC))
			}
			_, _, err := executeExportSessionsCommand(root, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			for _, path := range paths {
				info, statErr := os.Stat(path)
				require.NoError(t, statErr)
				assert.Positive(t, info.Size())
			}
		})
	}
}
