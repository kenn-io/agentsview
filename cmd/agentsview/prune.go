package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

// PruneConfig holds parsed CLI options for the prune command.
type PruneConfig struct {
	Filter db.PruneFilter
	DryRun bool
	Yes    bool
}

func parsePruneFlags(args []string) (PruneConfig, error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	project := fs.String(
		"project", "",
		"Sessions whose project contains this substring",
	)
	maxMessages := fs.Int(
		"max-messages", -1,
		"Sessions with at most N user messages",
	)
	before := fs.String(
		"before", "",
		"Sessions that ended before this date (YYYY-MM-DD)",
	)
	firstMessage := fs.String(
		"first-message", "",
		"Sessions whose first message starts with this text",
	)
	dryRun := fs.Bool(
		"dry-run", false,
		"Show what would be pruned without deleting",
	)
	yes := fs.Bool(
		"yes", false,
		"Skip confirmation prompt",
	)

	if err := fs.Parse(args); err != nil {
		return PruneConfig{}, err
	}

	if *maxMessages < 0 && *maxMessages != -1 {
		return PruneConfig{}, fmt.Errorf("max-messages must be >= 0")
	}

	var mm *int
	if *maxMessages != -1 {
		mm = maxMessages
	}

	cfg := PruneConfig{
		Filter: db.PruneFilter{
			Project:      *project,
			MaxMessages:  mm,
			Before:       *before,
			FirstMessage: *firstMessage,
		},
		DryRun: *dryRun,
		Yes:    *yes,
	}

	if !cfg.Filter.HasFilters() {
		return PruneConfig{}, fmt.Errorf(
			"at least one filter is required\n" +
				"use --project, --max-messages, --before," +
				" or --first-message",
		)
	}

	return cfg, nil
}

// Pruner executes the prune workflow against a database.
type Pruner struct {
	DB  *db.DB
	Out io.Writer
	In  io.Reader
}

// Prune finds matching sessions and deletes them.
func (p *Pruner) Prune(cfg PruneConfig) error {
	if !cfg.Filter.HasFilters() {
		return fmt.Errorf(
			"at least one filter is required " +
				"(refusing to prune all sessions)",
		)
	}

	candidates, err := p.DB.FindPruneCandidates(cfg.Filter)
	if err != nil {
		return fmt.Errorf("finding candidates: %w", err)
	}

	if len(candidates) == 0 {
		fmt.Fprintln(p.Out,
			"No sessions match the given filters.")
		return nil
	}

	writeSummary(p.Out, candidates)

	if cfg.DryRun {
		fmt.Fprintln(p.Out, "\nDry run: no changes made.")
		return nil
	}

	if !cfg.Yes {
		msg := fmt.Sprintf(
			"\nDelete %d sessions?", len(candidates),
		)
		if !confirm(p.In, p.Out, msg) {
			fmt.Fprintln(p.Out, "Aborted.")
			return nil
		}
	}

	ids := make([]string, len(candidates))
	for i, s := range candidates {
		ids[i] = s.ID
	}

	deleted, err := p.DB.DeleteSessions(ids)
	if err != nil {
		return fmt.Errorf("deleting sessions: %w", err)
	}

	filesRemoved, bytesReclaimed := deleteFiles(candidates)

	fmt.Fprintf(p.Out,
		"\nDeleted %d sessions, removed %d files"+
			" (%s reclaimed)\n",
		deleted, filesRemoved, formatBytes(bytesReclaimed),
	)
	return nil
}

func confirm(r io.Reader, w io.Writer, msg string) bool {
	fmt.Fprintf(w, "%s [y/N] ", msg)
	scanner := bufio.NewScanner(r)
	scanner.Scan()
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes"
}

func writeSummary(w io.Writer, sessions []db.Session) {
	var totalSize int64
	byProject := map[string]int{}
	var projects []string
	for _, s := range sessions {
		if byProject[s.Project] == 0 {
			projects = append(projects, s.Project)
		}
		byProject[s.Project]++
		if s.FileSize != nil {
			totalSize += *s.FileSize
		}
	}

	sort.Strings(projects)

	fmt.Fprintf(w,
		"Found %d sessions (%s on disk)\n",
		len(sessions), formatBytes(totalSize),
	)
	fmt.Fprintln(w, "\nBy project:")
	for _, proj := range projects {
		count := byProject[proj]
		fmt.Fprintf(w, "  %-40s %d\n", proj, count)
	}
}

func deleteFiles(sessions []db.Session) (int, int64) {
	removed := 0
	var reclaimed int64

	for _, s := range sessions {
		if s.FilePath == nil {
			continue
		}
		path := *s.FilePath

		info, err := os.Stat(path)
		size := int64(0)
		if err == nil {
			size = info.Size()
		}

		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				log.Printf(
					"warning: removing %s: %v", path, err,
				)
			}
			continue
		}
		removed++
		reclaimed += size

		// Remove parent directory if empty (session subdirs).
		dir := filepath.Dir(path)
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed, reclaimed
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func runPrune(cfg PruneConfig) {
	if cfg.Filter.MaxMessages != nil && *cfg.Filter.MaxMessages < 0 {
		fatal("max-messages must be >= 0")
	}
	if !cfg.Filter.HasFilters() {
		fatal("at least one filter is required\nuse --project, --max-messages, --before, or --first-message")
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer database.Close()

	pruner := &Pruner{
		DB:  database,
		Out: os.Stdout,
		In:  os.Stdin,
	}
	if err := pruner.Prune(cfg); err != nil {
		log.Fatalf("prune: %v", err)
	}
}
