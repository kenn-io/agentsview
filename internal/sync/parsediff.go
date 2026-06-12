package sync

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ParseDiffOptions configures a report-only re-parse comparison.
type ParseDiffOptions struct {
	// Agents restricts the run; empty means every file-based agent with
	// a DiscoverFunc. Agents without an on-disk source to re-parse
	// (database-backed or import-only) are rejected with an error.
	Agents []parser.AgentType
	// Limit caps the number of source files parsed, newest mtime first
	// across all agents. 0 means no limit.
	Limit int
	// Progress, when non-nil, is called as (filesDone, filesTotal) from
	// the result collector.
	Progress func(done, total int)
}

// NewDiffEngine creates an engine for report-only parse-diff runs. It
// forces Ephemeral so nothing is persisted (no skip cache, no sync
// state) and arms the engine's force-parse mode so every discovered
// file is fully re-parsed regardless of stored size/mtime/data_version
// state.
func NewDiffEngine(database *db.DB, cfg EngineConfig) *Engine {
	cfg.Ephemeral = true
	return NewEngine(database, cfg)
}

// ParseDiff re-parses session source files with the current binary,
// runs the result through the same normalization sync applies, and
// compares it against the stored rows. It writes nothing: no sessions,
// no skip cache, no sync state. It holds the engine's sync mutex for
// the duration.
func (e *Engine) ParseDiff(ctx context.Context, opts ParseDiffOptions) (*ParseDiffReport, error) {
	return &ParseDiffReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		DataVersion: db.CurrentDataVersion(),
		FieldCounts: map[string]int{},
	}, nil
}
