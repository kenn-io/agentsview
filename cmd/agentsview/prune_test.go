package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestParsePruneFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(t *testing.T, cfg PruneConfig)
	}{
		{
			name:    "no filters",
			args:    []string{},
			wantErr: "at least one filter",
		},
		{
			name: "project filter",
			args: []string{"--project", "myapp"},
			check: func(t *testing.T, cfg PruneConfig) {
				t.Helper()
				assert.Equal(t, "myapp", cfg.Filter.Project)
				assert.False(t, cfg.DryRun, "DryRun default")
				assert.False(t, cfg.Yes, "Yes default")
			},
		},
		{
			name: "all flags",
			args: []string{
				"--project", "p",
				"--max-messages", "5",
				"--before", "2024-01-01",
				"--first-message", "hello",
				"--dry-run",
				"--yes",
			},
			check: func(t *testing.T, cfg PruneConfig) {
				t.Helper()
				assert.Equal(t, "p", cfg.Filter.Project)
				require.NotNil(t, cfg.Filter.MaxMessages)
				assert.Equal(t, 5, *cfg.Filter.MaxMessages)
				assert.Equal(t, "2024-01-01", cfg.Filter.Before)
				assert.Equal(t, "hello", cfg.Filter.FirstMessage)
				assert.True(t, cfg.DryRun, "DryRun should be true")
				assert.True(t, cfg.Yes, "Yes should be true")
			},
		},
		{
			name:    "unknown flag",
			args:    []string{"--bogus"},
			wantErr: "flag provided but not defined",
		},
		{
			name:    "negative max-messages",
			args:    []string{"--max-messages", "-2"},
			wantErr: "max-messages must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parsePruneFlags(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err,
					"expected error containing %q", tt.wantErr)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestParsePruneFlagsHelp(t *testing.T) {
	_, err := parsePruneFlags([]string{"--help"})
	require.ErrorIs(t, err, flag.ErrHelp)
}

func TestPrunerEmptyFilterReturnsError(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	pruner, _ := newTestPruner(t, d, "")
	cfg := PruneConfig{
		Filter: db.PruneFilter{},
	}

	err := pruner.Prune(cfg)
	require.Error(t, err, "expected error for empty filter")
	assert.Contains(t, err.Error(), "at least one filter",
		"error should mention filter requirement")
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"yes lowercase", "y\n", true},
		{"yes full", "yes\n", true},
		{"YES uppercase", "YES\n", true},
		{"no", "n\n", false},
		{"empty", "\n", false},
		{"other text", "maybe\n", false},
		{"y with spaces", "  y  \n", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.NewReader(tt.input)
			out := &bytes.Buffer{}
			got := confirm(in, out, "Delete?")
			assert.Equal(t, tt.want, got)
			assert.Contains(t, out.String(), "[y/N]",
				"prompt missing [y/N]")
		})
	}
}

func TestWriteSummary(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", Project: "projA", FileSize: new(int64(1024))},
		{ID: "s2", Project: "projA", FileSize: new(int64(2048))},
		{ID: "s3", Project: "projB"},
	}

	var buf bytes.Buffer
	writeSummary(&buf, sessions)
	out := buf.String()

	want := `Found 3 sessions (3.0 KB on disk)

By project:
  projA                                    2
  projB                                    1
`
	assert.Equal(t, want, out, "writeSummary() mismatch")
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%d_bytes", tt.input)
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatBytes(tt.input),
				"formatBytes(%d)", tt.input)
		})
	}
}

func TestPrunerMaxMessagesCountsUserOnly(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	// Session with 1 user message + 49 assistant messages.
	// max-messages=1 should match because only user messages
	// are counted.
	dbtest.SeedSession(t, d, "oneshot", "proj", func(s *db.Session) {
		s.MessageCount = 50
	})
	msgs := []db.Message{dbtest.UserMsg("oneshot", 0, "do it")}
	for i := 1; i < 50; i++ {
		msgs = append(msgs,
			dbtest.AsstMsg("oneshot", i, "working..."))
	}
	dbtest.SeedMessages(t, d, msgs...)

	// Session with 5 user messages + 5 assistant messages.
	// max-messages=1 should NOT match.
	dbtest.SeedSession(t, d, "multi", "proj", func(s *db.Session) {
		s.MessageCount = 10
	})
	dbtest.SeedMessages(t, d,
		dbtest.UserMsg("multi", 0, "step 1"),
		dbtest.AsstMsg("multi", 1, "done 1"),
		dbtest.UserMsg("multi", 2, "step 2"),
		dbtest.AsstMsg("multi", 3, "done 2"),
		dbtest.UserMsg("multi", 4, "step 3"),
		dbtest.AsstMsg("multi", 5, "done 3"),
		dbtest.UserMsg("multi", 6, "step 4"),
		dbtest.AsstMsg("multi", 7, "done 4"),
		dbtest.UserMsg("multi", 8, "step 5"),
		dbtest.AsstMsg("multi", 9, "done 5"),
	)

	pruner, buf := newTestPruner(t, d, "")
	cfg := PruneConfig{
		Filter: db.PruneFilter{MaxMessages: new(1)},
		DryRun: true,
	}

	require.NoError(t, pruner.Prune(cfg), "Prune")

	out := buf.String()
	assert.Contains(t, out, "Found 1 sessions",
		"expected 1 match (oneshot only)")
}

func TestPruner_PruneScenarios(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		cfg        PruneConfig
		wantOutput []string
		wantKept   bool
	}{
		{
			name:       "dry run",
			cfg:        PruneConfig{Filter: db.PruneFilter{Project: "test"}, DryRun: true},
			wantOutput: []string{"Dry run", "Found 1 sessions"},
			wantKept:   true,
		},
		{
			name:       "no matches",
			cfg:        PruneConfig{Filter: db.PruneFilter{Project: "nonexistent"}},
			wantOutput: []string{"No sessions match"},
			wantKept:   true,
		},
		{
			name:       "abort",
			input:      "n\n",
			cfg:        PruneConfig{Filter: db.PruneFilter{Project: "test"}},
			wantOutput: []string{"Aborted"},
			wantKept:   true,
		},
		{
			name:       "confirm delete",
			input:      "y\n",
			cfg:        PruneConfig{Filter: db.PruneFilter{Project: "test"}},
			wantOutput: []string{"Deleted 1 sessions"},
			wantKept:   false,
		},
		{
			name:       "yes flag skips prompt",
			cfg:        PruneConfig{Filter: db.PruneFilter{Project: "test"}, Yes: true},
			wantOutput: []string{"Deleted 1 sessions"},
			wantKept:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			dbtest.SeedSession(t, d, "s1", "test", func(s *db.Session) {
				s.EndedAt = new("2024-01-01T00:00:00Z")
				s.MessageCount = 0
			})

			pruner, buf := newTestPruner(t, d, tt.input)
			require.NoError(t, pruner.Prune(tt.cfg), "Prune")

			out := buf.String()
			for _, want := range tt.wantOutput {
				assert.Contains(t, out, want,
					"expected output containing %q", want)
			}
			if tt.cfg.Yes {
				assert.NotContains(t, out, "[y/N]",
					"should not prompt when --yes is set")
			}

			s, _ := d.GetSession(context.Background(), "s1")
			if tt.wantKept {
				assert.NotNil(t, s, "session was deleted unexpectedly")
			} else {
				assert.Nil(t, s, "session still exists")
			}
		})
	}
}

func TestDeleteFilesRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "session1")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	f := filepath.Join(subdir, "data.jsonl")
	require.NoError(t, os.WriteFile(f, []byte("test data"), 0o644))

	sessions := []db.Session{
		{ID: "s1", FilePath: new(f)},
	}

	removed, reclaimed := deleteFiles(sessions)
	assert.Equal(t, 1, removed)
	assert.Equal(t, int64(9), reclaimed)

	// File should be gone.
	_, err := os.Stat(f)
	assert.True(t, os.IsNotExist(err), "file still exists")

	// Empty parent dir should be removed.
	_, err = os.Stat(subdir)
	assert.True(t, os.IsNotExist(err), "empty parent dir still exists")
}

func TestDeleteFilesMissingFile(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", FilePath: new("/nonexistent/path/file.jsonl")},
	}

	removed, reclaimed := deleteFiles(sessions)
	assert.Equal(t, 0, removed)
	assert.Equal(t, int64(0), reclaimed)
}

func TestDeleteFilesNilPath(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", FilePath: nil},
	}

	removed, reclaimed := deleteFiles(sessions)
	assert.Equal(t, 0, removed)
	assert.Equal(t, int64(0), reclaimed)
}

func newTestPruner(t *testing.T, d *db.DB, input string) (*Pruner, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	p := &Pruner{
		DB:  d,
		Out: &buf,
		In:  strings.NewReader(input),
	}
	return p, &buf
}
