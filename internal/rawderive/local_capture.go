package rawderive

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// PlanLocalCapture groups only continuations verified by the native parser.
// This may parse transcripts, so hosted capture and watchers keep using their
// ordinary raw-capture plans. Callers supply the complete immutable discovery.
func PlanLocalCapture(ctx context.Context, provider parser.Provider, source parser.SourceRef, sources []parser.SourceRef) (parser.RawCapturePlan, []parser.SourceRef, error) {
	ctx = parser.WithoutFilesystemProjectDiscovery(ctx)
	planner, ok := provider.(parser.RawCaptureProvider)
	if !ok {
		return parser.RawCapturePlan{}, nil, errors.New("provider does not support raw capture")
	}
	plan, err := planner.PlanRawCapture(ctx, source)
	if err != nil {
		return plan, nil, err
	}
	members := []parser.SourceRef{source}
	if source.Provider != parser.AgentClaude || !strings.HasPrefix(filepath.Base(source.DisplayPath), "agent-") {
		return plan, members, nil
	}
	outcome, err := provider.Parse(ctx, parser.ParseRequest{Source: source, ForceParse: true})
	if err != nil {
		return plan, nil, err
	}
	var paths []string
	for _, result := range outcome.Results {
		if result.Result.Session.File.Path == source.DisplayPath && len(result.Result.Session.ClaudeSubagentSources) > 1 {
			paths = result.Result.Session.ClaudeSubagentSources
			break
		}
	}
	if len(paths) < 2 {
		return plan, members, nil
	}
	byPath := make(map[string]parser.SourceRef, len(sources))
	for _, candidate := range sources {
		byPath[candidate.DisplayPath] = candidate
	}
	seen := make(map[string]bool, len(plan.Entries))
	for _, entry := range plan.Entries {
		seen[entry.Path] = true
	}
	for _, path := range paths {
		if path == source.DisplayPath {
			continue
		}
		member, ok := byPath[path]
		if !ok {
			return plan, nil, errors.New("claude continuation is outside captured source discovery")
		}
		companion, err := planner.PlanRawCapture(ctx, member)
		if err != nil {
			return plan, nil, err
		}
		if companion.CaptureRoot != plan.CaptureRoot {
			return plan, nil, errors.New("claude continuation has a different capture root")
		}
		members = append(members, member)
		for _, entry := range companion.Entries {
			if !seen[entry.Path] {
				plan.Entries = append(plan.Entries, entry)
				seen[entry.Path] = true
			}
		}
	}
	slices.SortFunc(plan.Entries, func(a, b parser.RawCaptureEntry) int { return strings.Compare(a.Path, b.Path) })
	return plan, members, nil
}
