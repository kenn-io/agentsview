package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// PruneConfig holds parsed CLI options for the prune command.
type PruneConfig struct {
	Filter db.PruneFilter
	DryRun bool
	Yes    bool
}

// Pruner executes the prune workflow against a database.
type Pruner struct {
	DB  *db.DB
	Out io.Writer
	In  io.Reader
}

// Prune finds matching sessions and deletes them.
func (p *Pruner) Prune(ctx context.Context, cfg PruneConfig) error {
	if !cfg.Filter.HasFilters() {
		return errors.New("at least one filter is required " +
			"(refusing to prune all sessions)",
		)
	}

	candidates, err := p.DB.FindPruneCandidates(ctx, cfg.Filter)
	if err != nil {
		return fmt.Errorf("finding candidates: %w", err)
	}

	if len(candidates) == 0 {
		fmt.Fprintln(p.Out,
			"No sessions match the given filters.")
		return nil
	}

	filesKept := writeSummary(p.Out, candidates)

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

	deleted, err := p.DB.DeleteSessions(ctx, ids)
	if err != nil {
		return fmt.Errorf("deleting sessions: %w", err)
	}

	filesRemoved, bytesReclaimed := deleteFiles(candidates)

	fmt.Fprintf(p.Out,
		"\nDeleted %d sessions, removed %d files"+
			" (%s reclaimed)",
		deleted, filesRemoved, formatBytes(bytesReclaimed),
	)
	if filesKept > 0 {
		writeKeptFiles(p.Out, "left", filesKept)
	}
	fmt.Fprintln(p.Out)
	return nil
}

func confirm(r io.Reader, w io.Writer, msg string) bool {
	fmt.Fprintf(w, "%s [y/N] ", msg)
	scanner := bufio.NewScanner(r)
	scanner.Scan()
	ans := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return ans == "y" || ans == "yes"
}

func writeSummary(w io.Writer, sessions []db.Session) int {
	var totalSize int64
	trashed := 0
	byProject := map[string]int{}
	var projects []string
	for _, s := range sessions {
		if byProject[s.Project] == 0 {
			projects = append(projects, s.Project)
		}
		byProject[s.Project]++
		if s.DeletedAt != nil {
			trashed++
		}
		if s.FilePath != nil && parser.OwnsAppStore(parser.AgentType(s.Agent), *s.FilePath) {
			continue
		}
		if s.FileSize != nil {
			totalSize += *s.FileSize
		}
	}

	sort.Strings(projects)

	fmt.Fprintf(w,
		"Found %d sessions (%s on disk)",
		len(sessions), formatBytes(totalSize),
	)
	if trashed > 0 {
		fmt.Fprintf(w, ", %d in trash", trashed)
	}
	kept := keptSourceFiles(sessions)
	if kept > 0 {
		writeKeptFiles(w, "will leave", kept)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\nBy project:")
	for _, proj := range projects {
		count := byProject[proj]
		fmt.Fprintf(w, "  %-40s %d\n", proj, count)
	}
	return kept
}

func writeKeptFiles(w io.Writer, verb string, count int) {
	fmt.Fprintf(w, ", %s %d source file%s in place", verb, count, pluralSuffix(count))
}

func keptSourceFiles(sessions []db.Session) int {
	kept := map[string]struct{}{}
	for _, s := range sessions {
		if s.FilePath == nil || !parser.OwnsAppStore(parser.AgentType(s.Agent), *s.FilePath) {
			continue
		}
		path := parser.ResolveSourceFilePath(*s.FilePath)
		kept[path] = struct{}{}
	}
	count := 0
	for path := range kept {
		if _, err := os.Stat(path); err == nil {
			count++
		}
	}
	return count
}

func deleteFiles(sessions []db.Session) (int, int64) {
	removed := 0
	var reclaimed int64

	for _, s := range sessions {
		if s.FilePath == nil || parser.OwnsAppStore(parser.AgentType(s.Agent), *s.FilePath) {
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

func runPrune(ctx context.Context, cfg PruneConfig) {
	if cfg.Filter.MaxMessages != nil && *cfg.Filter.MaxMessages < 0 {
		fatal("max-messages must be >= 0")
	}
	if !cfg.Filter.HasFilters() {
		fatal("at least one filter is required\nuse --agent, --project, --max-messages, --before, or --first-message")
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	database, writeLock, err := openWriteDB(ctx, appCfg)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}

	pruner := &Pruner{
		DB:  database,
		Out: os.Stdout,
		In:  os.Stdin,
	}
	err = pruner.Prune(ctx, cfg)
	closeWriteDB(database, writeLock)
	if err != nil {
		log.Fatalf("prune: %v", err)
	}
}
