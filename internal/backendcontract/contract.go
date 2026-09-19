// Package backendcontract centralizes compile-time checks that every storage
// backend implements the full server-facing db.Store capability surface.
package backendcontract

import (
	clickhousestore "go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/db"
	duckdbstore "go.kenn.io/agentsview/internal/duckdb"
	postgresstore "go.kenn.io/agentsview/internal/postgres"
)

var (
	_ db.Store = (*db.DB)(nil)
	_ db.Store = (*postgresstore.Store)(nil)
	_ db.Store = (*duckdbstore.Store)(nil)
	_ db.Store = (*clickhousestore.Store)(nil)
)
