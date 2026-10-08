package clickhouse

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/readbase"
)

type catalogSQL struct{ store *Store }

func (s *Store) catalog() *readbase.Catalog {
	return readbase.NewCatalog(catalogSQL{s}, "clickhouse")
}

func (s catalogSQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.store.queryContext(ctx, query, args...)
}

func (s catalogSQL) InventoryAggregate(ctx context.Context, filter db.ProjectDateFilter) (map[string]db.ProjectInventoryAgg, error) {
	return s.store.projectInventoryAggregate(ctx, filter)
}

func (s catalogSQL) WorktreeCandidateSessions(ctx context.Context, ids []string) ([]db.WorktreeCandidateSession, error) {
	return s.store.loadWorktreeCandidateSessions(ctx, ids)
}

func (s catalogSQL) VisibleArchivesSQL() string {
	return readbase.VisibleArchivesSQL()
}

func (s catalogSQL) MachinesSQL() string {
	return readbase.MachinesSQL()
}

func (s catalogSQL) SourceArchivesSQL() string {
	return readbase.SourceArchivesSQL()
}

func (s catalogSQL) MappingsSQL(machine *string) (string, []any) {
	return readbase.MappingsSQL(machine, db.ClickHouseQueryDialect())
}

func (s catalogSQL) CandidateRowsSQL(machine *string) (string, []any) {
	return readbase.CandidateRowsSQL(machine, db.ClickHouseQueryDialect())
}

func (s catalogSQL) ArchiveSessionsSQL(filter db.ProjectDateFilter) (string, []any) {
	return readbase.ArchiveSessionsSQL(filter, db.ClickHouseQueryDialect())
}

func (s catalogSQL) ListProjectIdentityObservations(ctx context.Context, labels []string) ([]export.ProjectIdentityObservation, error) {
	return s.store.ListProjectIdentityObservations(ctx, labels)
}

func (s catalogSQL) BuildProjectIdentityMap(ctx context.Context, labels []string) (map[string]export.ProjectMapEntry, error) {
	return s.store.BuildProjectIdentityMap(ctx, labels)
}
