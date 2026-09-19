package main

import (
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

// replicaBackends lists every remote replica compiled into this binary. A new
// backend is added here; newReplicaCommand gives it its CLI verb and
// pushBackendOptions gives the daemon its push route.
var replicaBackends = []storage.Replica{pgReplica{}}

// mirrorBackend is the derived local mirror the daemon can rebuild.
var mirrorBackend storage.Mirror = duckdb.Mirror{}

// pushBackendOptions registers every push backend with a server so the
// daemon and the committed OpenAPI document expose the same push routes.
func pushBackendOptions() []server.Option {
	return []server.Option{
		server.WithReplicas(replicaBackends...),
		server.WithMirror(mirrorBackend),
	}
}
