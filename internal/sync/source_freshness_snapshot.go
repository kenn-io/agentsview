package sync

import (
	"context"
	"log"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type sourceFreshnessSnapshotKey struct{}

// withSourceFreshnessSnapshot loads the pass's per-source freshness state once
// and carries it in ctx to the workers. A load failure leaves ctx unchanged so
// every lookup falls back to its live query.
func (e *Engine) withSourceFreshnessSnapshot(
	ctx context.Context, files []parser.DiscoveredFile,
) context.Context {
	agents := []parser.AgentType{}
	seen := map[parser.AgentType]bool{}
	for _, file := range files {
		if !file.ForceParse && !seen[file.Agent] && e.ProviderStatHasher(file.Agent) != nil {
			seen[file.Agent] = true
			agents = append(agents, file.Agent)
		}
	}
	if len(agents) == 0 {
		return ctx
	}
	snap, err := e.db.LoadSourceFreshnessSnapshot(ctx, agents)
	if err != nil {
		log.Printf("source freshness snapshot: %v", err)
		return ctx
	}
	if snap == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceFreshnessSnapshotKey{}, snap)
}

func sourceFreshnessSnapshotFrom(ctx context.Context) *db.SourceFreshnessSnapshot {
	snap, _ := ctx.Value(sourceFreshnessSnapshotKey{}).(*db.SourceFreshnessSnapshot)
	return snap
}
