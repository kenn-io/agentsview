package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// ensuredSchemas maps each mirror database, keyed by schemaCacheKey, to the
// schemaState it had after this process last ensured or pushed to it. The
// daemon builds a new Sync for every push request, so the Sync's own memo
// never outlives one push there; this lets those Syncs skip the schema step
// while nothing it depends on has changed since.
var ensuredSchemas sync.Map

// schemaCacheKey names one mirror database on one server.
func schemaCacheKey(t Target) (string, error) {
	database, err := t.DatabaseName()
	if err != nil {
		return "", err
	}
	return t.URL + "\x00" + database, nil
}

// schemaState is what the schema step's outcome depends on besides its own
// code: the mirror's tables, the server that holds them and when it
// started, and the stored table exchange probe. The step probes again after
// a restart, on another server, or when the stored probe changes, as a
// restore can bring in an answer from another server.
type schemaState struct {
	tables  string
	server  string
	started int64
	probe   string
}

// readSchemaState reads the mirror's schemaState. create_table_query
// carries each table's columns, indexes, projections, comment, and view
// query, so a missing table or any change EnsureSchema would repair changes
// tables. A table replaced by an identical one, as a prepared usage refresh
// does, keeps it. The refresh's temporary table is left out.
func readSchemaState(ctx context.Context, conn *sql.DB) (schemaState, error) {
	var state schemaState
	var count, hash uint64
	if err := conn.QueryRowContext(ctx, `
		SELECT count(), sum(cityHash64(name, create_table_query)),
			toString(serverUUID()), toUnixTimestamp(now()) - uptime()
		FROM system.tables
		WHERE database = currentDatabase() AND NOT startsWith(name, '.tmp.')`,
	).Scan(&count, &hash, &state.server, &state.started); err != nil {
		return schemaState{}, fmt.Errorf("reading clickhouse schema state: %w", err)
	}
	state.tables = fmt.Sprintf("%d:%x", count, hash)
	stored, err := readMetadata(ctx, conn, chExchangeProbeKey)
	if err != nil {
		return schemaState{}, err
	}
	state.probe = stored[chExchangeProbeKey]
	return state, nil
}

// sameAs reports whether s describes the same mirror state as recorded.
// now() and uptime() count whole seconds, so the computed start of one
// running server can differ by a second between reads.
func (s schemaState) sameAs(recorded schemaState) bool {
	drift := s.started - recorded.started
	return s.tables == recorded.tables && s.server == recorded.server &&
		s.probe == recorded.probe && drift >= -1 && drift <= 1
}

// schemaUnchangedSinceEnsured reports whether the mirror behind conn still
// has the state this process last recorded for key. A mirror whose state
// cannot be read, such as one whose sync_metadata table is gone, counts as
// changed, so the full schema step runs and reports any real failure.
func schemaUnchangedSinceEnsured(ctx context.Context, conn *sql.DB, key string) bool {
	recorded, ok := ensuredSchemas.Load(key)
	if !ok {
		return false
	}
	current, err := readSchemaState(ctx, conn)
	if err != nil {
		return false
	}
	state, ok := recorded.(schemaState)
	return ok && current.sameAs(state)
}

// recordEnsuredSchema records the mirror's current state as ensured.
func recordEnsuredSchema(ctx context.Context, conn *sql.DB, key string) error {
	state, err := readSchemaState(ctx, conn)
	if err != nil {
		return err
	}
	ensuredSchemas.Store(key, state)
	return nil
}
