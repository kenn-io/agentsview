package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
)

// mirrorView is a materialized view that lives as long as the mirror. The
// schema step keeps its query at the one in the code: CREATE ... IF NOT
// EXISTS leaves an existing view alone, so a view created by an older binary
// would otherwise run its old query for good.
type mirrorView struct {
	name string
	// clauses sit between the name and AS, such as "TO usage_messages".
	clauses string
	// query is the SELECT the view runs, with any SETTINGS clause.
	query string
}

// comment identifies the view's query. The schema step compares it with the
// live view's comment: the query ClickHouse stores is rewritten (qualified
// names, added parentheses), so it cannot be compared with the code's text.
func (v mirrorView) comment() string {
	sum := sha256.Sum256([]byte(v.query))
	return "agentsview view query " + hex.EncodeToString(sum[:])
}

// ensureView creates v, or replaces the query of a live view whose comment
// does not match. A view without the comment predates this check and is
// updated once.
//
// ALTER TABLE ... MODIFY QUERY swaps the query in place without stopping
// inserts, so every insert runs either the old query or the new one, and no
// derived rows are lost. It keeps the view's source table, its column list,
// and its clauses: a definition that changes any of those needs a new view.
func ensureView(ctx context.Context, conn *sql.DB, v mirrorView) error {
	want := v.comment()
	var live string
	err := conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, v.name).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, "CREATE MATERIALIZED VIEW IF NOT EXISTS "+v.name+" "+v.clauses+
			" AS "+v.query+" COMMENT "+chSQLString(want)); err != nil {
			return fmt.Errorf("creating clickhouse view %s: %w", v.name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading clickhouse view %s: %w", v.name, err)
	}
	if live == want {
		return nil
	}
	log.Printf("ClickHouse: updating view %s to the current query; inserts continue during the update", v.name)
	// The comment changes last, so a failure between the two statements
	// leaves a mismatch the next schema step repairs.
	for _, query := range []string{
		"ALTER TABLE " + v.name + " MODIFY QUERY " + v.query,
		"ALTER TABLE " + v.name + " MODIFY COMMENT " + chSQLString(want),
	} {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("updating clickhouse view %s: %w", v.name, err)
		}
	}
	return nil
}
