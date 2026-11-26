package sync

import (
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

func antigravityTitleRoots(agent parser.AgentType, configured []string) []string {
	if agent != parser.AgentAntigravityCLI {
		return configured
	}
	var roots []string
	seen := make(map[string]struct{}, len(configured))
	for _, root := range configured {
		expanded, _ := parser.ExpandAntigravityCLIRoot(root)
		for _, candidate := range expanded {
			if _, dup := seen[candidate]; dup {
				continue
			}
			seen[candidate] = struct{}{}
			roots = append(roots, candidate)
		}
	}
	return roots
}

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		for _, root := range antigravityTitleRoots(agent, e.sources().agentDirs[agent]) {
			path := filepath.Join(root, parser.AntigravitySummariesDBName)
			if applied, ok := e.appliedNativeTitles[path]; ok {
				states[path] = applied.state
			}
		}
	}
	return states
}

// Mirror snapshots change only on extraction, so persisted states can seed a delta engine.
func (e *Engine) InjectAntigravityTitleStates(states map[string]parser.SQLiteContainerState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	now := time.Now()
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI} {
		for _, root := range antigravityTitleRoots(agent, e.sources().agentDirs[agent]) {
			path := filepath.Join(root, parser.AntigravitySummariesDBName)
			delete(e.appliedNativeTitles, path)
			if state, ok := states[path]; ok {
				e.appliedNativeTitles[path] = appliedNativeTitleStore{state: state, readAt: now}
			}
		}
	}
}
