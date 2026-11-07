package main

import agentsync "go.kenn.io/agentsview/internal/sync"

func (r watchRoot) registeredScanScopes() []agentsync.WatchScope {
	scopes := make([]agentsync.WatchScope, 0, len(r.sourceScanScopes))
	for _, scope := range r.sourceScanScopes {
		scopes = append(scopes, agentsync.WatchScope{Agent: string(scope.agent), SyncDir: scope.syncDir})
	}
	return scopes
}
