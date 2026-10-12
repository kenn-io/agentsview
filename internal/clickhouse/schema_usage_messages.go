package clickhouse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	"go.kenn.io/agentsview/internal/db"
)

// usageColumns are the token counters usage_messages stores for each message.
// The view computes them from token_usage at insert time, and the startup
// backfill computes them once for rows that predate the view. The messages
// table keeps only the raw JSON.
var usageColumns = []struct{ name, typ, expression string }{
	{"usage_present", "UInt8", "token_usage != ''"},
	{"usage_input", "Int64", "JSONExtractInt(token_usage, 'input_tokens')"},
	{"usage_output", "Int64", "JSONExtractInt(token_usage, 'output_tokens')"},
	{"usage_cache_create", "Int64", "JSONExtractInt(token_usage, 'cache_creation_input_tokens')"},
	{"usage_cache_create_1h", "Int64", "JSONExtractInt(token_usage, 'cache_creation', 'ephemeral_1h_input_tokens')"},
	{"usage_cache_read", "Int64", "JSONExtractInt(token_usage, 'cache_read_input_tokens')"},
	{"usage_reasoning", "Int64", "JSONExtractInt(token_usage, 'reasoning_tokens')"},
	{"usage_web", "Int64", "JSONExtractInt(token_usage, 'server_tool_use', 'web_search_requests')"},
}

// Keep the source key, not the timestamp, as the replacement key: a retry may
// correct a timestamp or remove usage without changing the message ordinal.
const usageMessageFields = `session_id, ordinal, timestamp, model, provider_id,
	claude_message_id, claude_request_id, source_uuid, push_version`

func ensureUsageMessages(ctx context.Context, conn *sql.DB) error {
	const marker = "usage_messages_backfill"
	metadata, err := readMetadata(ctx, conn, marker)
	if err != nil {
		return err
	}
	if metadata[marker] == "1" {
		return nil
	}
	columns := []string{
		"session_id String", "ordinal Int64", "timestamp Nullable(DateTime64(6, 'UTC'))",
		"model String", "provider_id String", "claude_message_id String",
		"claude_request_id String", "source_uuid String", "push_version UInt64",
	}
	fields := []string{usageMessageFields}
	for _, column := range usageColumns {
		columns = append(columns, column.name+" "+column.typ)
		fields = append(fields, column.expression+" AS "+column.name)
	}
	selection := strings.Join(fields, ", ")
	// The low bit makes a live insert beat a concurrent, older backfill of
	// the same push version. UInt128 preserves every possible UInt64 version.
	columns = append(columns, "revision UInt128", "INDEX usage_time timestamp TYPE minmax GRANULARITY 1")
	queries := []string{
		"CREATE TABLE IF NOT EXISTS usage_messages (" + strings.Join(columns, ", ") + `)
		 ENGINE = ReplacingMergeTree(revision) ORDER BY (session_id, ordinal)
		 SETTINGS index_granularity = 64`,
		"CREATE MATERIALIZED VIEW IF NOT EXISTS usage_messages_mv TO usage_messages AS SELECT " +
			selection + ", toUInt128(push_version) * 2 + 1 AS revision FROM messages",
	}
	for _, query := range queries {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("creating stored usage messages: %w", err)
		}
	}
	log.Print("ClickHouse: filling usage messages before serving; source messages remain unchanged")
	if _, err := conn.ExecContext(ctx, "INSERT INTO usage_messages SELECT "+selection+
		", toUInt128(push_version) * 2 AS revision FROM messages"); err != nil {
		return fmt.Errorf("backfilling usage messages (startup can retry): %w", err)
	}
	return writeMetadata(ctx, conn, map[string]string{marker: "1"})
}

const usageSessionSnapshotTable = "usage_session_snapshots"

const usageSnapshotMessageType = `Array(Tuple(
 ordinal Int64, role String, timestamp Nullable(DateTime64(6, 'UTC')),
 model String, provider_id String, token_usage String,
 claude_message_id String, claude_request_id String, source_uuid String))`

func usageSessionSnapshotSpec() (tableSpec, error) {
	sessions, ok := tableByName("sessions")
	if !ok {
		return tableSpec{}, errors.New("missing sessions table specification")
	}
	events, ok := tableByName("usage_events")
	if !ok {
		return tableSpec{}, errors.New("missing usage_events table specification")
	}
	var eventFields []string
	for _, column := range events.columns {
		eventFields = append(eventFields, column.name+" "+column.typ)
	}
	eventFields = append(eventFields, "push_version UInt64")
	return tableSpec{
		name: usageSessionSnapshotTable,
		columns: append(slices.Clone(sessions.columns),
			col("usage_messages", usageSnapshotMessageType),
			col("usage_events", "Array(Tuple("+strings.Join(eventFields, ",")+"))")),
		orderBy: []string{"id"},
	}, nil
}

func ensureUsageSessionSnapshots(ctx context.Context, conn *sql.DB) error {
	spec, err := usageSessionSnapshotSpec()
	if err != nil {
		return err
	}
	var columns []string
	for _, column := range spec.columns {
		columns = append(columns, column.ddl())
	}
	columns = append(columns,
		"push_version UInt64",
		"revision UInt128 DEFAULT toUInt128(push_version) * 2 + 1",
	)
	factTypes := []string{"ordinal Int64", "timestamp Nullable(DateTime64(6, 'UTC'))", "model String", "provider_id String", "claude_message_id String", "claude_request_id String", "source_uuid String"}
	var factValues []string
	for _, field := range []string{"ordinal", "timestamp", "model", "provider_id", "claude_message_id", "claude_request_id", "source_uuid"} {
		factValues = append(factValues, "tupleElement(m,'"+field+"')")
	}
	for _, column := range usageColumns {
		typ := column.typ
		expression := strings.ReplaceAll(column.expression, "token_usage", "tupleElement(m,'token_usage')")
		if column.name != "usage_present" && column.name != "usage_web" {
			typ = "UInt32"
			expression = fmt.Sprintf("least(greatest(%s,toInt64(0)),toInt64(%d))", expression, db.MaxPlausibleTokens)
		}
		factTypes = append(factTypes, column.name+" "+typ)
		factValues = append(factValues, expression)
	}
	columns = append(columns, "usage_facts Array(Tuple("+strings.Join(factTypes, ",")+
		")) MATERIALIZED arrayMap(m -> tuple("+strings.Join(factValues, ",")+"),usage_messages)")
	_, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+spec.name+" ("+
		strings.Join(columns, ",")+") ENGINE=ReplacingMergeTree(revision) ORDER BY id SETTINGS index_granularity=1")
	if err != nil {
		return fmt.Errorf("creating complete usage snapshots: %w", err)
	}
	// Snapshots copy the sessions columns, so a sessions column added later must reach a snapshot table created before it.
	existing, err := readColumns(ctx, conn)
	if err != nil {
		return err
	}
	for _, c := range spec.columns {
		if _, has := existing[spec.name][c.name]; has {
			continue
		}
		if _, err := conn.ExecContext(ctx, spec.addColumnSQL(c)); err != nil {
			return fmt.Errorf("adding clickhouse column %s.%s: %w", spec.name, c.name, err)
		}
	}
	return ensurePreparedUsage(ctx, conn)
}

// chPreparedUsageKeyColumn is the non-null time column prepared_usage sorts
// by. ClickHouse cannot prune granules through a key expression such as
// ifNull(ts, epoch), so a range on the nullable ts column reads the whole
// table; a stored column keyed on the same value lets a day read its slice.
const chPreparedUsageKeyColumn = "ts_key"

const chPreparedUsageSortingKey = chPreparedUsageKeyColumn + ", session_id"

// chPreparedUsageRefresh is the scheduled rebuild. Every push also starts a
// refresh, so the schedule only covers a trigger that failed.
const chPreparedUsageRefresh = "EVERY 15 MINUTE"

// chPreparedUsageRefreshSettings apply to every query that fills
// prepared_usage, whether the view runs it or a push does.
const chPreparedUsageRefreshSettings = "final=1,max_threads=4"

// chPreparedUsageBuildPrefix names the tables a push fills before it moves
// their rows into prepared_usage. Each carries the number of the rebuild
// lock its push holds; see rebuildPreparedUsage.
const chPreparedUsageBuildPrefix = "prepared_usage_build_"

// chPreparedUsageRebuildLockPrefix names the numbered tables that lock
// prepared_usage rebuilds; see acquireRebuildLock.
const chPreparedUsageRebuildLockPrefix = "prepared_usage_rebuild_lock_"

// chPreparedUsageRebuildLockPattern and chPreparedUsageBuildPattern match
// the names of the rebuild lock and build tables; chTableNumberSQL reads
// their numbers.
const (
	chPreparedUsageRebuildLockPattern = "^" + chPreparedUsageRebuildLockPrefix + "[0-9]+$"
	chPreparedUsageBuildPattern       = "^" + chPreparedUsageBuildPrefix + "[0-9]+$"
)

// chTableNumberSQL reads the number at the end of a table's name.
const chTableNumberSQL = "toUInt64OrZero(extract(name, '([0-9]+)$'))"

// chRebuildLockTimeout is how long a rebuild lock lasts after its holder
// last renewed it. After that, another push treats the holder as stopped and
// takes the next lock. A rebuilding push renews its lock every
// chRebuildLockRenewal.
const (
	chRebuildLockTimeout = time.Hour
	chRebuildLockRenewal = 10 * time.Minute
)

// rebuildLockState is the state of one rebuild lock. It lives as JSON in
// the lock table's comment, which ClickHouse changes atomically, including
// on NFS.
type rebuildLockState struct {
	Status rebuildLockStatus `json:"status"`
	// RenewedAt is the ClickHouse server's Unix time when the holder last
	// renewed a held lock, so every client compares it with one clock.
	RenewedAt int64 `json:"renewed_at,omitzero"`
}

type rebuildLockStatus string

const (
	rebuildLockHeld     rebuildLockStatus = "held"
	rebuildLockReleased rebuildLockStatus = "released"
	// rebuildLockRetired marks the newest lock of a database whose view
	// refreshes prepared_usage, so no push takes the lock; see
	// retireRebuildLocks.
	rebuildLockRetired rebuildLockStatus = "retired"
)

// commentSQL returns the state as a quoted SQL string for COMMENT clauses.
func (s rebuildLockState) commentSQL() string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// A struct of a string and an integer always encodes.
		panic(fmt.Sprintf("encoding prepared usage rebuild lock state: %v", err))
	}
	return chSQLString(string(encoded))
}

// parseRebuildLockState reads the state of the lock table of generation
// from its comment. Only a manual edit can make the comment unreadable, so
// the error names the table and the statement that repairs it.
func parseRebuildLockState(generation uint64, comment string) (rebuildLockState, error) {
	var state rebuildLockState
	err := json.Unmarshal([]byte(comment), &state)
	if err == nil {
		switch state.Status {
		case rebuildLockHeld, rebuildLockReleased, rebuildLockRetired:
			return state, nil
		}
		err = fmt.Errorf("unknown status %q", state.Status)
	}
	lock := fmt.Sprintf("%s%d", chPreparedUsageRebuildLockPrefix, generation)
	// The statement comes last so it can be copied from the log as is.
	return rebuildLockState{}, fmt.Errorf("prepared usage rebuild lock %s has comment %q, which is not a lock "+
		"state (%w); repair it in the mirror database with: ALTER TABLE %s MODIFY COMMENT %s",
		lock, comment, err, lock, rebuildLockState{Status: rebuildLockReleased}.commentSQL())
}

// createRebuildLock creates the lock table of generation with state.
// CREATE TABLE fails when the name exists, which is what makes taking a
// lock atomic.
func createRebuildLock(ctx context.Context, conn *sql.DB, generation uint64, state rebuildLockState) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s%d (x UInt8) ENGINE = Memory COMMENT %s",
		chPreparedUsageRebuildLockPrefix, generation, state.commentSQL()))
	return err
}

// setRebuildLock replaces the state of the lock table of generation.
func setRebuildLock(ctx context.Context, conn *sql.DB, generation uint64, state rebuildLockState) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s%d MODIFY COMMENT %s",
		chPreparedUsageRebuildLockPrefix, generation, state.commentSQL()))
	return err
}

// chNoLockSkipMessage explains why a push without the view skips its
// rebuild when the database has no rebuild lock: only the schema step of a
// database that cannot swap tables creates one; see openRebuildLocks.
const chNoLockSkipMessage = "clickhouse push: the prepare_usage view is missing. Usage reads are slower " +
	"until it exists. Another client may be recreating it. If this message repeats, restart agentsview " +
	"to recreate it."

// chRetiredSkipMessage explains why a push skips its rebuild once the
// newest rebuild lock is retired.
const chRetiredSkipMessage = "clickhouse push: skipping the prepared usage rebuild: this database can swap " +
	"tables again, so the prepare_usage view's scheduled refresh keeps prepared usage current"

// chExchangeProbePrefix names the scratch tables chExchangeSupported
// exchanges.
const chExchangeProbePrefix = "agentsview_exchange_probe_"

// chSnapshotStampSQL identifies the complete snapshot set as a row count and
// a sum of per-row hashes, so any republished or deleted session changes it.
// The refresh stores it on every prepared row; scalar subqueries are
// evaluated before the refresh scans, so a push that lands mid-refresh can
// only make the stored stamp older than the rows, never newer.
const chSnapshotStampSQL = `(SELECT count() FROM usage_session_snapshots) AS snapshot_count,
	(SELECT sum(sipHash64(id, revision)) FROM usage_session_snapshots) AS snapshot_hash`

// chPreparedUsageColumns lists the prepared columns in the order
// scanActivityUsageRows reads them; ts_key is not part of a usage row.
const chPreparedUsageColumns = `session_id, message_ordinal, ts, pricing_ts, source, model,
	provider_id, agent, claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
	input_tokens_norm, output_tokens_norm, cache_create_norm, cache_create_1h_norm,
	cache_read_norm, reasoning_tokens_norm, web_search_requests_norm, cost_microdollars, cost_source`

// chPreparedUsagePricingDigestSQL reads the digest the mirror was last priced
// under. A push writes every price record for a digest before this key, so a
// refresh that reads the key joins a complete set of records for it.
const chPreparedUsagePricingDigestSQL = "(SELECT ifNull(max(value), '') FROM sync_metadata WHERE key = '" +
	usagePricedDigestKey + "')"

// chPriceStampSQL counts the price records under the digest the refresh
// joins. A push only adds records under a digest, so a live count below the
// stored one means the records were cleared and the stored copies are not
// read.
const chPriceStampSQL = `(SELECT count() FROM usage_event_prices WHERE pricing_digest = ` +
	chPreparedUsagePricingDigestSQL + `) AS price_count`

// chPreparedUsageRowColumns are the columns a read takes from a prepared
// row, in the same order from the table and from rows prepared at read time.
var chPreparedUsageRowColumns = func() string {
	columns := []string{chPreparedUsageColumns, "price_model", "price_key", "snapshot_revision", chPreparedUsageKeyColumn}
	for _, column := range chUsageStoredPriceColumns {
		columns = append(columns, column.stored)
	}
	return strings.Join(columns, ", ")
}()

// chPreparedUsageKeyedSQL renders the normalized rows of the complete
// snapshots that sessionWhere selects, each with its price model and price
// key.
func chPreparedUsageKeyedSQL(sessionWhere string) string {
	messageWhere, eventWhere := chUsageStoredMessageEligibility, chUsageEventEligibility
	if sessionWhere != "" {
		messageWhere += " AND " + sessionWhere
		eventWhere += " AND " + sessionWhere
	}
	selection := clickUsageNormalizedQueryFrom("", messageWhere, eventWhere,
		"usage_session_snapshots s ARRAY JOIN s.usage_facts AS m",
		"usage_session_snapshots s ARRAY JOIN s.usage_events AS ue", "1", "s.revision")
	return `SELECT *, ` + chPriceModelCaseSQL() + ` AS price_model, ` + chUsagePriceKeySQL + ` AS price_key
		FROM (` + selection + `)`
}

// chRawHostUsageRowsSQL renders prepared rows, as a refresh would store
// them, from the raw message and event tables for the sessions in the
// usage_raw_host_sessions external table: the sessions of hosts that have
// not published complete snapshots. Messages follow the raw reads' version
// rule, and the rows join the price records under the digest the one query
// parameter names. A row derived from no snapshot has revision zero.
func chRawHostUsageRowsSQL() string {
	const sessions = " AND s.id IN (SELECT id FROM usage_raw_host_sessions)"
	selection := clickUsageNormalizedQueryFrom("",
		chUsageStoredMessageEligibility+sessions, chUsageEventEligibility+sessions,
		"usage_messages m JOIN sessions s ON s.id = m.session_id",
		"usage_events ue JOIN sessions s ON s.id = ue.session_id", chUsageMessageCurrent, "toUInt128(0)")
	keyed := `SELECT *, ` + chPriceModelCaseSQL() + ` AS price_model, ` + chUsagePriceKeySQL + ` AS price_key
		FROM (` + selection + `)`
	// Exact index filtering also reads overlapping newer parts before FINAL,
	// as the raw activity read does, so a range filter pushed into these
	// tables cannot bring back an older version of a row.
	return chUsageRowsFromKeyedSQL(keyed, chUsagePriceRowsSQL()+" WHERE pricing_digest = ?", false) +
		" SETTINGS optimize_move_to_prewhere_if_final = 0, use_skip_indexes_if_final_exact_mode = 1"
}

// chUsageRowsFromKeyedSQL renders prepared rows from keyed normalized rows,
// joined with the price records priceRowsSQL selects; see
// chPreparedUsageQuery.
func chUsageRowsFromKeyedSQL(keyedSQL, priceRowsSQL string, stamped bool) string {
	stored := make([]string, 0, len(chUsageStoredPriceColumns))
	for _, column := range chUsageStoredPriceColumns {
		stored = append(stored, "p."+column.joined+" AS "+column.stored)
	}
	stamps := ""
	if stamped {
		// Like the snapshot stamp, the price count is taken before the
		// refresh scans, so records added during a refresh are also in the
		// join.
		stamps = chSnapshotStampSQL + `,
		` + chPreparedUsagePricingDigestSQL + ` AS pricing_digest,
		` + chPriceStampSQL + `,
		`
	}
	return `SELECT n.*, ifNull(n.ts, toDateTime64(0,6,'UTC')) AS ` + chPreparedUsageKeyColumn + `,
		` + stamps + strings.Join(stored, ", ") + `
	FROM (` + keyedSQL + `) n
	LEFT JOIN (` + priceRowsSQL + `) p
		ON p.p_price_key = n.price_key`
}

// chPreparedUsageQuery is the refresh query over every complete snapshot.
// It renders prepared rows joined with the price records of the current
// pricing digest. Besides the normalized facts a row stores the snapshot
// revision it was derived from, the price model, the price key, and the
// price record, so range reads skip the per-request join against every
// price record. It also stores the snapshot set, the pricing digest, and
// the price count it read.
func chPreparedUsageQuery() string {
	return chUsageRowsFromKeyedSQL(chPreparedUsageKeyedSQL(""),
		chUsagePriceRowsSQL()+" WHERE pricing_digest = "+chPreparedUsagePricingDigestSQL, true)
}

// chUsageChangedSessionsWhere selects the snapshots of the sessions in the
// usage_changed_sessions query parameter: sessions pushed since the
// refresh, whose stored rows are missing or from an older snapshot. The
// ids are a parameter rather than an external table so the snapshot key
// narrows the read to their granules; through an external table the read
// took most of the snapshot index. A parameter is not query text, so the
// list is not limited by the query size.
const chUsageChangedSessionsWhere = "has({usage_changed_sessions:Array(String)}, s.id)"

// chPreparedUsageDeltaColumns are the columns of a prepared row a read
// takes, with their types, in the order chPreparedUsageDeltaRowsSQL selects
// them and prepareUsageDelta fills them. The stored price columns come last,
// filled in Go.
var chPreparedUsageDeltaColumns = []struct{ name, typ string }{
	{"session_id", "String"},
	{"message_ordinal", "Nullable(Int64)"},
	{"ts", "Nullable(DateTime64(6, 'UTC'))"},
	{"pricing_ts", "Nullable(DateTime64(6, 'UTC'))"},
	{"source", "String"},
	{"model", "String"},
	{"provider_id", "String"},
	{"agent", "String"},
	{"claude_message_id", "String"},
	{"claude_request_id", "String"},
	{"source_uuid", "String"},
	{"usage_dedup_key", "String"},
	{"input_tokens_norm", "Int64"},
	{"output_tokens_norm", "Int64"},
	{"cache_create_norm", "Int64"},
	{"cache_create_1h_norm", "Int64"},
	{"cache_read_norm", "Int64"},
	{"reasoning_tokens_norm", "Int64"},
	{"web_search_requests_norm", "Int64"},
	{"cost_microdollars", "Nullable(Int64)"},
	{"cost_source", "String"},
	{"price_model", "String"},
	{"price_key", "String"},
	{"snapshot_revision", "UInt128"},
	{chPreparedUsageKeyColumn, "DateTime64(6, 'UTC')"},
	{"stored_priced", "Int64"},
	{"stored_token_cost", "Int64"},
	{"stored_savings", "Int64"},
	{"stored_billed_context_id", "String"},
	{"stored_unbilled_context_id", "String"},
	{"stored_request_scoped", "Bool"},
	{"stored_band", "Int64"},
	{"stored_price_error", "String"},
}

// chPreparedUsageDeltaRowsSQL selects, for the changed sessions, the
// prepared columns a refresh would store except the price record, which
// prepareUsageDelta prices in Go from the same inputs a push prices: a join
// against the mirror's records would scan every record for a few keys.
func chPreparedUsageDeltaRowsSQL() string {
	return `SELECT ` + chPreparedUsageColumns + `, price_model, price_key, snapshot_revision,
			ifNull(ts, toDateTime64(0,6,'UTC')) AS ` + chPreparedUsageKeyColumn + `
		FROM (` + chPreparedUsageKeyedSQL(chUsageChangedSessionsWhere) + `)`
}

// chPreparedUsageComment identifies the table shape and refresh query, so an
// installation prepared by a binary with a different query is rebuilt and
// never read.
func chPreparedUsageComment() string {
	sum := sha256.Sum256([]byte(chPreparedUsageSortingKey + "\n" + chPreparedUsageRefresh + "\n" + chPreparedUsageQuery()))
	return "agentsview prepared usage " + hex.EncodeToString(sum[:])
}

// chPreparedUsageProjections are the projections of prepared_usage, by
// name.
var chPreparedUsageProjections = []struct{ name, query string }{
	{"usage_content_fingerprint", `SELECT count(),sumWithOverflow(
			reinterpretAsUInt128(sipHash128Reference(tuple(*)))
		)`},
	// The sessions the rows cover, read after every push to find the
	// sessions pushed since the refresh without scanning the rows.
	{"covered_sessions", `SELECT session_id, snapshot_revision GROUP BY session_id, snapshot_revision`},
}

// ensurePreparedUsageProjections adds the projections prepared_usage lacks.
// Every refresh swaps in a new table that keeps them, so a server or push
// that starts during one finds them in place and alters nothing: an ALTER
// that meets the swap fails on the replaced table. A missing projection is
// added with the refresh stopped and any running one finished first.
func ensurePreparedUsageProjections(ctx context.Context, conn *sql.DB) (err error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM system.projections
		WHERE database = currentDatabase() AND table = 'prepared_usage'`)
	if err != nil {
		return fmt.Errorf("reading prepared usage projections: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning prepared usage projection: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating prepared usage projections: %w", err)
	}
	var missing []string
	for _, p := range chPreparedUsageProjections {
		if !present[p.name] {
			missing = append(missing, "ALTER TABLE prepared_usage ADD PROJECTION IF NOT EXISTS "+p.name+" ("+p.query+")")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	hasView, err := preparedUsageViewExists(ctx, conn)
	if err != nil {
		return err
	}
	queries := missing
	if hasView {
		if _, err := conn.ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage"); err != nil {
			return fmt.Errorf("stopping prepared usage refresh: %w", err)
		}
		// Once stopped, the refresh must start again even when a step below
		// fails: a later start that finds the projections present would
		// leave it stopped for good.
		defer func() {
			if _, startErr := conn.ExecContext(context.WithoutCancel(ctx), "SYSTEM START VIEW prepare_usage"); startErr != nil {
				err = errors.Join(err, fmt.Errorf("restarting prepared usage refresh: %w", startErr))
			}
		}()
		queries = append([]string{"SYSTEM WAIT VIEW prepare_usage"}, missing...)
	}
	for _, query := range queries {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("adding prepared usage projections: %w", err)
		}
	}
	return nil
}

func ensurePreparedUsage(ctx context.Context, conn *sql.DB) error {
	prepared := chPreparedUsageQuery()
	var comment string
	err := conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepared_usage'`).Scan(&comment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading prepared usage shape: %w", err)
	}
	rebuild := errors.Is(err, sql.ErrNoRows) || comment != chPreparedUsageComment()
	if err == nil && rebuild {
		// The table is derived from the snapshots, so replacing it loses
		// nothing; the next refresh fills it, and reads use the raw rows
		// until it has. Drop the view first so a refresh cannot block the
		// drop of the table.
		log.Print("ClickHouse: rebuilding prepared usage for the current refresh query")
		if err := dropPreparedUsageView(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS prepared_usage SYNC"); err != nil {
			return fmt.Errorf("replacing prepared usage: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prepared_usage ENGINE=MergeTree
		ORDER BY (`+chPreparedUsageSortingKey+`) COMMENT `+chSQLString(chPreparedUsageComment())+
		` AS `+prepared+" LIMIT 0"); err != nil {
		return fmt.Errorf("creating prepared usage: %w", err)
	}
	if err := ensurePreparedUsageProjections(ctx, conn); err != nil {
		return err
	}
	exchange, err := chExchangeSupported(ctx, conn)
	if err != nil {
		return err
	}
	if !exchange {
		// Every refresh of the view would fail at its exchange, so each
		// push rebuilds the table instead, including the push that runs
		// this schema step.
		log.Print("ClickHouse: this database cannot swap tables (EXCHANGE TABLES), usually because its data " +
			"directory is on NFS; each push rebuilds prepared usage itself instead of the prepare_usage view's " +
			"scheduled refresh")
		// Release a retired lock before dropping the view, so a push that
		// finds no view rebuilds; see refreshPreparedUsage for the gap a
		// replaced prepared_usage leaves.
		if err := openRebuildLocks(ctx, conn); err != nil {
			return err
		}
		return dropPreparedUsageView(ctx, conn)
	}
	// Fence off rebuilds before the view exists, so none can publish after
	// a view refresh: a rebuild that already took a lock finds the retired
	// lock newer than its own, and dropping its build table waits for a
	// publish already running and fails a later one. A push that tries to
	// take the lock from here on finds it retired; see acquireRebuildLock.
	// A push rebuilds only while a lock exists, so a database with no lock
	// has no rebuild to fence.
	if err := retireRebuildLocks(ctx, conn); err != nil {
		return fmt.Errorf("switching prepared usage to scheduled refresh: %w", err)
	}
	if err := createPreparedUsageView(ctx, conn); err != nil {
		return err
	}
	// A new table serves reads only once filled; start that now rather
	// than after the schedule or the next push.
	if rebuild {
		if _, err := conn.ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage"); err != nil {
			return fmt.Errorf("filling prepared usage: %w", err)
		}
	}
	return nil
}

// createPreparedUsageView creates the view that refreshes prepared_usage
// on the server. It leaves the table empty until the first refresh.
func createPreparedUsageView(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS prepare_usage REFRESH `+chPreparedUsageRefresh+`
		TO prepared_usage EMPTY AS `+chPreparedUsageQuery()+" SETTINGS "+chPreparedUsageRefreshSettings); err != nil {
		return fmt.Errorf("creating prepared usage: %w", err)
	}
	return nil
}

func preparedUsageViewExists(ctx context.Context, conn *sql.DB) (bool, error) {
	var hasView uint8
	if err := conn.QueryRowContext(ctx, `SELECT count() > 0 FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepare_usage'`).Scan(&hasView); err != nil {
		return false, fmt.Errorf("checking prepared usage view: %w", err)
	}
	return hasView != 0, nil
}

// dropPreparedUsageView drops the view that refreshes prepared_usage. It
// stops the refresh first so a running one cannot block the drop; an earlier
// rebuild may have dropped the view already, so it is stopped only when it
// exists.
func dropPreparedUsageView(ctx context.Context, conn *sql.DB) error {
	hasView, err := preparedUsageViewExists(ctx, conn)
	if err != nil {
		return err
	}
	queries := []string{"DROP VIEW IF EXISTS prepare_usage SYNC"}
	if hasView {
		queries = append([]string{"SYSTEM STOP VIEW prepare_usage"}, queries...)
	}
	for _, query := range queries {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("dropping prepared usage view: %w", err)
		}
	}
	return nil
}

// chExchangeSupported reports whether the mirror database can exchange two
// tables. A refreshable view that replaces its target ends every refresh
// with EXCHANGE TABLES, which ClickHouse runs as a renameat2 call with the
// RENAME_EXCHANGE flag on the database's metadata files and no fallback. A
// filesystem without that flag, such as NFS, fails it with
// UNSUPPORTED_METHOD. The probe exchanges two empty tables in the same
// database, so it renames files in the same metadata directory a refresh
// does; where the tables keep their data does not matter.
func chExchangeSupported(ctx context.Context, conn *sql.DB) (supported bool, err error) {
	// A probe takes milliseconds, so probe tables an hour old belong to a
	// push that was killed during one.
	if err := dropTables(ctx, conn, "match(name, ?) AND metadata_modification_time <= now() - INTERVAL 1 HOUR",
		"^"+chExchangeProbePrefix+"[a-z0-9]+_[ab]$"); err != nil {
		return false, err
	}
	probe := chExchangeProbePrefix + strings.ToLower(rand.Text())
	tables := []string{probe + "_a", probe + "_b"}
	defer func() {
		for _, table := range tables {
			if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+table+" SYNC"); dropErr != nil {
				err = errors.Join(err, fmt.Errorf("dropping table exchange probe: %w", dropErr))
			}
		}
	}()
	for _, table := range tables {
		if _, err := conn.ExecContext(ctx, "CREATE TABLE "+table+" (x UInt8) ENGINE = Memory"); err != nil {
			return false, fmt.Errorf("creating table exchange probe: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, "EXCHANGE TABLES "+tables[0]+" AND "+tables[1])
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeUnsupportedMethod {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probing table exchange: %w", err)
	}
	return true, nil
}

// refreshPreparedUsage brings prepared_usage up to date after a push. Where
// the view exists, it starts the view's refresh, which runs on the server.
// Where the schema step found that the database cannot swap tables, it
// dropped the view and left a rebuild lock, and the push rebuilds the table
// before it returns.
func refreshPreparedUsage(ctx context.Context, conn *sql.DB) error {
	hasView, err := preparedUsageViewExists(ctx, conn)
	if err != nil {
		return err
	}
	if !hasView {
		locks, _, state, err := newestRebuildLock(ctx, conn)
		if err != nil {
			return err
		}
		switch {
		case locks == 0:
			log.Print(chNoLockSkipMessage)
			return nil
		case state.Status == rebuildLockRetired:
			// A schema step that replaces prepared_usage drops the view
			// first. If the database also lost its swap support, the step
			// releases the retired lock only afterwards, so this push may
			// see that gap rather than a switch that stopped halfway.
			// Creating the view then would leave every refresh failing
			// until the next schema step.
			exchange, err := chExchangeSupported(ctx, conn)
			if err != nil {
				return err
			}
			if exchange {
				if err := finishScheduledRefreshSwitch(ctx, conn); err != nil {
					return fmt.Errorf("finishing the switch to the prepare_usage view (the next push retries): %w", err)
				}
				return nil
			}
			if err := openRebuildLocks(ctx, conn); err != nil {
				return err
			}
		}
		if err := rebuildPreparedUsage(ctx, conn); err != nil {
			return fmt.Errorf("rebuilding prepared usage (the next push retries): %w", err)
		}
		return nil
	}
	if _, err := conn.ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage"); err != nil {
		return fmt.Errorf("starting prepared usage refresh (the next scheduled refresh retries): %w", err)
	}
	return nil
}

// finishScheduledRefreshSwitch completes a switch to the view that a schema
// step started but did not finish: it retired the rebuild locks, then
// stopped before it created the view, so no push rebuilds and nothing
// refreshes. Retiring again drops any build table the schema step left
// behind before the view can refresh.
func finishScheduledRefreshSwitch(ctx context.Context, conn *sql.DB) error {
	log.Print("clickhouse push: finishing the switch of prepared usage to the prepare_usage view's scheduled refresh")
	if err := retireRebuildLocks(ctx, conn); err != nil {
		return err
	}
	if err := createPreparedUsageView(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage"); err != nil {
		return fmt.Errorf("filling prepared usage: %w", err)
	}
	return nil
}

// rebuildPreparedUsage refreshes prepared_usage without exchanging tables.
// It runs the view's query into a new build table, then moves the build
// table's parts into prepared_usage with REPLACE PARTITION, which swaps all
// of the table's parts at once. Readers therefore see either the previous
// rows or the new ones, each with the stamp of the snapshots they were
// derived from.
//
// Rebuilds take turns, as the view's refreshes do. A read checks freshness
// and fetches rows in separate queries, so a build that read older snapshots
// must not replace a newer one between them: the read would trust the newer
// stamp and serve the older rows. A push that finds another rebuild running
// skips its own. The running rebuild checks once more after it publishes and
// rebuilds again for snapshots that arrived meanwhile, as the view queues one
// more refresh for a request that arrives during a refresh.
func rebuildPreparedUsage(ctx context.Context, conn *sql.DB) (err error) {
	current, err := preparedUsageCurrent(ctx, conn)
	if err != nil || current {
		return err
	}
	generation, acquired, err := acquireRebuildLock(ctx, conn)
	if acquired {
		defer func() {
			err = errors.Join(err, releaseRebuildLock(context.WithoutCancel(ctx), conn, generation))
		}()
	}
	if err != nil || !acquired {
		return err
	}
	// Deferred after the release, so this runs first: a renewal must not
	// mark the lock held again after its release.
	renewCtx, stopRenewing := context.WithCancel(ctx)
	var renewing sync.WaitGroup
	renewing.Go(func() { keepRebuildLock(renewCtx, conn, generation) })
	defer func() {
		stopRenewing()
		renewing.Wait()
	}()
	// Only a lock holder creates a build table, so a build table with a
	// lower number belongs to a push that was stopped or lost its lock.
	// Dropping it fences that push off: a publish already reading the table
	// holds it, so DROP ... SYNC waits for the publish to finish before this
	// rebuild starts, and a later publish fails for lack of its table. Either
	// way the older rows never land after this rebuild's rows. A build table
	// with a higher number belongs to a successor and is left alone.
	if err := dropTables(ctx, conn, "match(name, ?) AND "+chTableNumberSQL+" < ?",
		chPreparedUsageBuildPattern, generation); err != nil {
		return err
	}
	// The first pass checks again because the previous holder may have
	// published these snapshots while this push waited for the lock.
	for pass := range 3 {
		current, err := preparedUsageCurrent(ctx, conn)
		if err != nil || current {
			return err
		}
		switch pass {
		case 1:
			log.Print("clickhouse push: another push changed sessions or prices during the rebuild; rebuilding prepared usage once more")
		case 2:
			log.Print("clickhouse push: another push changed sessions or prices during the second rebuild; " +
				"usage reads compute those sessions directly, which is slower, until the next push rebuilds prepared usage")
			return nil
		}
		if err := publishPreparedUsageBuild(ctx, conn, generation); err != nil {
			return err
		}
	}
	return nil
}

// publishPreparedUsageBuild fills the build table of the lock generation
// with the view's query and moves its parts into prepared_usage.
func publishPreparedUsageBuild(ctx context.Context, conn *sql.DB, generation uint64) (err error) {
	log.Print("clickhouse push: rebuilding prepared usage")
	started := time.Now()
	build := fmt.Sprintf("%s%d", chPreparedUsageBuildPrefix, generation)
	if _, err := conn.ExecContext(ctx, "CREATE TABLE "+build+" AS prepared_usage"); err != nil {
		return fmt.Errorf("creating prepared usage build table: %w", err)
	}
	defer func() {
		if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+build+" SYNC"); dropErr != nil {
			err = errors.Join(err, fmt.Errorf("dropping prepared usage build table: %w", dropErr))
		}
	}()
	// The build table copies prepared_usage, whose columns were created
	// from this query, so the columns match by position.
	if _, err := conn.ExecContext(ctx, "INSERT INTO "+build+" "+chPreparedUsageQuery()+
		" SETTINGS "+chPreparedUsageRefreshSettings); err != nil {
		return supersededBuildError(ctx, conn, build, fmt.Errorf("filling prepared usage build table: %w", err))
	}
	// A push whose lock expired may have lost it to a newer rebuild before
	// it created its build table, which that rebuild's drop then missed. If
	// the takeover comes after this check instead, the new holder drops the
	// build table and fences the publish off.
	superseded, err := rebuildLockSuperseded(ctx, conn, generation)
	if err != nil {
		return err
	}
	if superseded {
		return errRebuildSuperseded
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE prepared_usage REPLACE PARTITION tuple() FROM "+build); err != nil {
		return supersededBuildError(ctx, conn, build, fmt.Errorf("replacing prepared usage rows: %w", err))
	}
	log.Printf("clickhouse push: rebuilt prepared usage in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

// errRebuildSuperseded reports a rebuild that a newer rebuild took over
// from after this one outlasted its lock.
var errRebuildSuperseded = errors.New("a newer rebuild took over; discarding this build")

// supersededBuildError returns errRebuildSuperseded when err comes from a
// missing table and the build table is gone, which means a newer rebuild
// took over and dropped it. Otherwise it returns err.
func supersededBuildError(ctx context.Context, conn *sql.DB, build string, err error) error {
	if !isMissingTableError(err) {
		return err
	}
	var exists uint8
	if checkErr := conn.QueryRowContext(context.WithoutCancel(ctx), `SELECT count() > 0 FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, build).Scan(&exists); checkErr != nil || exists != 0 {
		return errors.Join(err, checkErr)
	}
	return errRebuildSuperseded
}

// keepRebuildLock renews the lock generation every chRebuildLockRenewal
// until ctx ends, so a rebuild that runs longer than chRebuildLockTimeout
// keeps its lock. It stops when the lock is gone, which means a newer
// holder took over.
func keepRebuildLock(ctx context.Context, conn *sql.DB, generation uint64) {
	ticker := time.NewTicker(chRebuildLockRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := renewRebuildLock(ctx, conn, generation)
		if isMissingTableError(err) {
			return
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("clickhouse push: renewing prepared usage rebuild lock: %v", err)
		}
	}
}

// renewRebuildLock marks the lock generation held as of the server's
// current time.
func renewRebuildLock(ctx context.Context, conn *sql.DB, generation uint64) error {
	var now int64
	if err := conn.QueryRowContext(ctx, "SELECT toUnixTimestamp(now())").Scan(&now); err != nil {
		return fmt.Errorf("reading server time: %w", err)
	}
	return setRebuildLock(ctx, conn, generation, rebuildLockState{Status: rebuildLockHeld, RenewedAt: now})
}

// acquireRebuildLock takes the next prepared usage rebuild lock. The locks
// are tables numbered in the order pushes took them, and the newest one
// decides: it is held until its holder marks it released in its comment, or
// until its holder has not renewed it for chRebuildLockTimeout. A database
// with no lock, or whose newest lock is retired, refreshes prepared_usage
// with its view, and no push rebuilds it. To take the lock, a push creates
// the table with the next number. CREATE TABLE fails when the name
// exists, so of two pushes that read the same newest lock only one gets the
// next number. Numbers never repeat, so a push that lost its lock cannot
// release or outrank its successor. The new holder drops every older lock,
// and the newest lock stays after release to keep the numbers rising, so one
// lock table remains between rebuilds.
func acquireRebuildLock(ctx context.Context, conn *sql.DB) (generation uint64, acquired bool, err error) {
	var now int64
	if err := conn.QueryRowContext(ctx, "SELECT toUnixTimestamp(now())").Scan(&now); err != nil {
		return 0, false, fmt.Errorf("reading server time: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT `+chTableNumberSQL+` AS generation, comment
		FROM system.tables WHERE database = currentDatabase() AND match(name, ?)
		ORDER BY generation`, chPreparedUsageRebuildLockPattern)
	if err != nil {
		return 0, false, fmt.Errorf("reading prepared usage rebuild locks: %w", err)
	}
	defer rows.Close()
	var generations []uint64
	var comment string
	for rows.Next() {
		var g uint64
		if err := rows.Scan(&g, &comment); err != nil {
			return 0, false, fmt.Errorf("scanning prepared usage rebuild lock: %w", err)
		}
		generations = append(generations, g)
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("reading prepared usage rebuild locks: %w", err)
	}
	if len(generations) == 0 {
		log.Print(chNoLockSkipMessage)
		return 0, false, nil
	}
	newest, err := parseRebuildLockState(generations[len(generations)-1], comment)
	if err != nil {
		return 0, false, err
	}
	if newest.Status == rebuildLockRetired {
		log.Print(chRetiredSkipMessage)
		return 0, false, nil
	}
	if newest.Status == rebuildLockHeld && now-newest.RenewedAt < int64(chRebuildLockTimeout/time.Second) {
		log.Printf("clickhouse push: skipping the prepared usage rebuild: another push holds %s%d "+
			"(last renewed %s); usage reads are slower until that push finishes, or for an hour after "+
			"its last renewal if it stopped; see \"Lock of a stopped push\" in the ClickHouse sync guide",
			chPreparedUsageRebuildLockPrefix, generations[len(generations)-1],
			time.Unix(newest.RenewedAt, 0).Format(time.RFC3339))
		return 0, false, nil
	}
	generation = generations[len(generations)-1]
	generation++
	acquired, err = takeRebuildLock(ctx, conn, generation, now)
	if err != nil || !acquired {
		return 0, false, err
	}
	for _, g := range generations {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s%d SYNC", chPreparedUsageRebuildLockPrefix, g)); err != nil {
			return generation, true, fmt.Errorf("dropping old prepared usage rebuild lock: %w", err)
		}
	}
	return generation, true, nil
}

// takeRebuildLock creates the lock table of generation, which succeeds for
// only one push. A push that read the locks before a long pause can create a
// generation a newer holder already dropped; it finds the newer lock and
// gives the generation up, so a retired generation and its build table name
// are never used again.
func takeRebuildLock(ctx context.Context, conn *sql.DB, generation uint64, now int64) (bool, error) {
	lock := fmt.Sprintf("%s%d", chPreparedUsageRebuildLockPrefix, generation)
	err := createRebuildLock(ctx, conn, generation, rebuildLockState{Status: rebuildLockHeld, RenewedAt: now})
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
		log.Print("clickhouse push: skipping the prepared usage rebuild: another push took the rebuild lock first")
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("locking prepared usage rebuild: %w", err)
	}
	superseded, err := rebuildLockSuperseded(ctx, conn, generation)
	if err == nil && !superseded {
		return true, nil
	}
	if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+lock+" SYNC"); dropErr != nil {
		err = errors.Join(err, fmt.Errorf("dropping prepared usage rebuild lock: %w", dropErr))
	}
	if err == nil {
		log.Print("clickhouse push: skipping the prepared usage rebuild: a newer push already took a later rebuild lock")
	}
	return false, err
}

// rebuildLockSuperseded reports whether a push took a newer rebuild lock
// than generation.
func rebuildLockSuperseded(ctx context.Context, conn *sql.DB, generation uint64) (bool, error) {
	var superseded uint8
	if err := conn.QueryRowContext(ctx, `SELECT count() > 0 FROM system.tables
		WHERE database = currentDatabase() AND match(name, ?) AND `+chTableNumberSQL+` > ?`,
		chPreparedUsageRebuildLockPattern, generation).Scan(&superseded); err != nil {
		return false, fmt.Errorf("checking prepared usage rebuild lock: %w", err)
	}
	return superseded != 0, nil
}

// releaseRebuildLock marks the lock generation released. A newer holder may
// already have dropped it, which leaves nothing to release.
func releaseRebuildLock(ctx context.Context, conn *sql.DB, generation uint64) error {
	err := setRebuildLock(ctx, conn, generation, rebuildLockState{Status: rebuildLockReleased})
	if err != nil && !isMissingTableError(err) {
		return fmt.Errorf("releasing prepared usage rebuild lock: %w", err)
	}
	return nil
}

// preparedUsageCurrent reports whether the prepared rows were derived from
// the current snapshots and price records. The refresh query reads nothing
// else, so a rebuild would store the same rows. An empty table has no stamp
// and is never current.
func preparedUsageCurrent(ctx context.Context, conn *sql.DB) (bool, error) {
	var stored, live usageStamp
	err := conn.QueryRowContext(ctx, `SELECT snapshot_count, snapshot_hash, pricing_digest, price_count
		FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(
		&stored.snapshotCount, &stored.snapshotHash, &stored.pricingDigest, &stored.priceCount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading prepared usage stamp: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT `+chSnapshotStampSQL+`,
		`+chPreparedUsagePricingDigestSQL+` AS pricing_digest, `+chPriceStampSQL).Scan(
		&live.snapshotCount, &live.snapshotHash, &live.pricingDigest, &live.priceCount); err != nil {
		return false, fmt.Errorf("reading current usage stamp: %w", err)
	}
	return stored == live, nil
}

// retireRebuildLocks fences off rebuilds that pushes started while the
// database could not exchange tables. The view refreshes prepared_usage
// from here on, as when the data directory moved off NFS. It takes one more
// lock number marked retired, which stays until openRebuildLocks: a push
// that took an older lock finds the newer one and discards its build, and a
// push that tries to take the lock later finds it retired and skips. It then
// drops the older locks and every build table. A database that never
// rebuilt this way has no lock tables and gets none.
func retireRebuildLocks(ctx context.Context, conn *sql.DB) error {
	for range 3 {
		locks, newest, state, err := newestRebuildLock(ctx, conn)
		if err != nil || locks == 0 {
			return err
		}
		retired := newest
		if state.Status != rebuildLockRetired {
			retired++
			newestRetired, err := createRetiredLock(ctx, conn, retired)
			if err != nil {
				return err
			}
			if !newestRetired {
				continue
			}
		}
		return dropTables(ctx, conn, "match(name, ?) OR (match(name, ?) AND "+chTableNumberSQL+" < ?)",
			chPreparedUsageBuildPattern, chPreparedUsageRebuildLockPattern, retired)
	}
	return errors.New("retiring prepared usage rebuild locks: other pushes kept taking newer locks")
}

// createRetiredLock creates the retired lock of generation and reports
// whether it is the newest lock. While retirement paused after reading the
// locks, pushes may have taken newer numbers and dropped this one, so the
// create can succeed below a newer lock; that lock's holder would stay
// unfenced, so the caller retires above it instead. The lower retired lock
// is dropped with the other old locks.
func createRetiredLock(ctx context.Context, conn *sql.DB, generation uint64) (bool, error) {
	err := createRebuildLock(ctx, conn, generation, rebuildLockState{Status: rebuildLockRetired})
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("retiring prepared usage rebuild lock: %w", err)
	}
	superseded, err := rebuildLockSuperseded(ctx, conn, generation)
	return err == nil && !superseded, err
}

// openRebuildLocks lets pushes rebuild prepared_usage in a database that
// cannot swap tables. A push rebuilds only while a rebuild lock exists and
// the newest one is not retired, so this creates the first lock, or
// releases the lock an earlier switch to the view retired.
func openRebuildLocks(ctx context.Context, conn *sql.DB) error {
	locks, newest, state, err := newestRebuildLock(ctx, conn)
	if err != nil {
		return err
	}
	if locks == 0 {
		err := createRebuildLock(ctx, conn, 1, rebuildLockState{Status: rebuildLockReleased})
		if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
			return nil
		}
		if err != nil {
			return fmt.Errorf("creating prepared usage rebuild lock: %w", err)
		}
		return nil
	}
	if state.Status != rebuildLockRetired {
		return nil
	}
	if err := setRebuildLock(ctx, conn, newest, rebuildLockState{Status: rebuildLockReleased}); err != nil {
		return fmt.Errorf("reopening prepared usage rebuild lock: %w", err)
	}
	return nil
}

// newestRebuildLock returns how many rebuild lock tables exist and the
// number and state of the newest one.
func newestRebuildLock(ctx context.Context, conn *sql.DB) (locks, newest uint64, state rebuildLockState, err error) {
	var comment string
	if err := conn.QueryRowContext(ctx, `SELECT count(), max(`+chTableNumberSQL+`), argMax(comment, `+chTableNumberSQL+`)
		FROM system.tables WHERE database = currentDatabase() AND match(name, ?)`,
		chPreparedUsageRebuildLockPattern).Scan(&locks, &newest, &comment); err != nil {
		return 0, 0, rebuildLockState{}, fmt.Errorf("reading prepared usage rebuild locks: %w", err)
	}
	if locks == 0 {
		return 0, 0, rebuildLockState{}, nil
	}
	state, err = parseRebuildLockState(newest, comment)
	return locks, newest, state, err
}

// dropTables drops the tables of the current database that the condition
// on system.tables selects. Callers select only names they make, which need
// no quoting.
func dropTables(ctx context.Context, conn *sql.DB, condition string, args ...any) error {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM system.tables
		WHERE database = currentDatabase() AND (`+condition+`)`, args...)
	if err != nil {
		return fmt.Errorf("listing tables to drop: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning table to drop: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing tables to drop: %w", err)
	}
	for _, name := range names {
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+name+" SYNC"); err != nil {
			return fmt.Errorf("dropping table %s: %w", name, err)
		}
	}
	return nil
}

func (s *Sync) insertUsageSessionSnapshots(
	ctx context.Context, payloads []sessionPayload, version uint64,
) error {
	spec, err := usageSessionSnapshotSpec()
	if err != nil {
		return err
	}
	rows := make([][]any, 0, len(payloads))
	for _, payload := range payloads {
		messages := make([][]any, 0, len(payload.messages))
		for _, message := range payload.messages {
			messages = append(messages, []any{
				int64(message.Ordinal), message.Role, timeValue(message.Timestamp),
				message.Model, message.ProviderID, string(message.TokenUsage),
				message.ClaudeMessageID, message.ClaudeRequestID, message.SourceUUID,
			})
		}
		events := make([][]any, 0, len(payload.usage))
		for _, event := range payload.usage {
			events = append(events, usageEventRow(event, version))
		}
		row := s.sessionRow(payload, payload.fingerprint, version)
		rows = append(rows, append(row[:len(row)-1], messages, events, version))
	}
	return insertRowsSpec(ctx, s.conn, spec, rows)
}
