package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/dbtest"
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
				if cfg.Filter.Project != "myapp" {
					t.Errorf(
						"Project = %q, want %q",
						cfg.Filter.Project, "myapp",
					)
				}
				if cfg.DryRun || cfg.Yes {
					t.Error("unexpected flag defaults")
				}
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
				if cfg.Filter.Project != "p" {
					t.Errorf("Project = %q", cfg.Filter.Project)
				}
				if cfg.Filter.MaxMessages == nil || *cfg.Filter.MaxMessages != 5 {
					t.Errorf(
						"MaxMessages = %v", cfg.Filter.MaxMessages,
					)
				}
				if cfg.Filter.Before != "2024-01-01" {
					t.Errorf("Before = %q", cfg.Filter.Before)
				}
				if cfg.Filter.FirstMessage != "hello" {
					t.Errorf(
						"FirstMessage = %q",
						cfg.Filter.FirstMessage,
					)
				}
				if !cfg.DryRun {
					t.Error("DryRun should be true")
				}
				if !cfg.Yes {
					t.Error("Yes should be true")
				}
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
				if err == nil {
					t.Fatalf("expected error containing %q",
						tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q missing %q",
						err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestParsePruneFlagsHelp(t *testing.T) {
	_, err := parsePruneFlags([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf(
			"expected flag.ErrHelp, got %v", err,
		)
	}
}

func TestPrunerEmptyFilterReturnsError(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	pruner, _ := newTestPruner(t, d, "")
	cfg := PruneConfig{
		Filter: db.PruneFilter{},
	}

	err := pruner.Prune(cfg)
	if err == nil {
		t.Fatal("expected error for empty filter")
	}
	if !strings.Contains(err.Error(), "at least one filter") {
		t.Errorf(
			"error %q should mention filter requirement",
			err,
		)
	}
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
			if got != tt.want {
				t.Errorf("confirm() = %v, want %v", got, tt.want)
			}
			if !strings.Contains(out.String(), "[y/N]") {
				t.Error("prompt missing [y/N]")
			}
		})
	}
}

func TestWriteSummary(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", Project: "projA", FileSize: dbtest.Ptr(int64(1024))},
		{ID: "s2", Project: "projA", FileSize: dbtest.Ptr(int64(2048))},
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
	if out != want {
		t.Errorf("writeSummary() mismatch\nwant:\n%s\ngot:\n%s", want, out)
	}
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
			got := formatBytes(tt.input)
			if got != tt.want {
				t.Errorf(
					"formatBytes(%d) = %q, want %q",
					tt.input, got, tt.want,
				)
			}
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
		Filter: db.PruneFilter{MaxMessages: dbtest.Ptr(1)},
		DryRun: true,
	}

	if err := pruner.Prune(cfg); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Found 1 sessions") {
		t.Errorf(
			"expected 1 match (oneshot only), got: %s", out,
		)
	}
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
				s.EndedAt = dbtest.Ptr("2024-01-01T00:00:00Z")
				s.MessageCount = 0
			})

			pruner, buf := newTestPruner(t, d, tt.input)
			if err := pruner.Prune(tt.cfg); err != nil {
				t.Fatalf("Prune: %v", err)
			}

			out := buf.String()
			for _, want := range tt.wantOutput {
				if !strings.Contains(out, want) {
					t.Errorf("expected output containing %q, got: %s", want, out)
				}
			}
			if tt.cfg.Yes && strings.Contains(out, "[y/N]") {
				t.Error("should not prompt when --yes is set")
			}

			s, _ := d.GetSession(context.Background(), "s1")
			if tt.wantKept && s == nil {
				t.Error("session was deleted unexpectedly")
			} else if !tt.wantKept && s != nil {
				t.Error("session still exists")
			}
		})
	}
}

func TestDeleteFilesRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "session1")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(subdir, "data.jsonl")
	if err := os.WriteFile(f, []byte("test data"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := []db.Session{
		{ID: "s1", FilePath: dbtest.Ptr(f)},
	}

	removed, reclaimed := deleteFiles(sessions)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if reclaimed != 9 {
		t.Errorf("reclaimed = %d, want 9", reclaimed)
	}

	// File should be gone.
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file still exists")
	}

	// Empty parent dir should be removed.
	if _, err := os.Stat(subdir); !os.IsNotExist(err) {
		t.Error("empty parent dir still exists")
	}
}

func TestDeleteFilesMissingFile(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", FilePath: dbtest.Ptr("/nonexistent/path/file.jsonl")},
	}

	removed, reclaimed := deleteFiles(sessions)
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0", reclaimed)
	}
}

func TestDeleteFilesNilPath(t *testing.T) {
	sessions := []db.Session{
		{ID: "s1", FilePath: nil},
	}

	removed, reclaimed := deleteFiles(sessions)
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0", reclaimed)
	}
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
