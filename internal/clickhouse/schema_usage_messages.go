package clickhouse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
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
// their rows into prepared_usage; see rebuildPreparedUsage.
const chPreparedUsageBuildPrefix = "prepared_usage_build_"

// chPreparedUsageRebuildLock names the table a push holds while it rebuilds
// prepared_usage; see rebuildPreparedUsage.
const chPreparedUsageRebuildLock = "prepared_usage_rebuild_lock"

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
		log.Print("ClickHouse: this database cannot exchange tables, which happens when its data directory " +
			"is on NFS; each push rebuilds prepared usage before it returns")
		return dropPreparedUsageView(ctx, conn)
	}
	if _, err := conn.ExecContext(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS prepare_usage REFRESH `+chPreparedUsageRefresh+`
		TO prepared_usage EMPTY AS `+prepared+" SETTINGS "+chPreparedUsageRefreshSettings); err != nil {
		return fmt.Errorf("creating prepared usage: %w", err)
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
	if err := dropAbandonedTables(ctx, conn, "^"+chExchangeProbePrefix+"[a-z0-9]+_[ab]$"); err != nil {
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
// Without the view, the database cannot exchange tables and the push
// rebuilds the table before it returns.
func refreshPreparedUsage(ctx context.Context, conn *sql.DB) error {
	hasView, err := preparedUsageViewExists(ctx, conn)
	if err != nil {
		return err
	}
	if !hasView {
		return rebuildPreparedUsage(ctx, conn)
	}
	if _, err := conn.ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage"); err != nil {
		return fmt.Errorf("starting prepared usage refresh: %w", err)
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
// skips its own; reads prepare its sessions from their snapshots until the
// next rebuild.
func rebuildPreparedUsage(ctx context.Context, conn *sql.DB) (err error) {
	// A lock or build table older than an hour is taken to belong to a push
	// that was killed. A rebuild that runs longer loses its lock, and the
	// next one may overlap it.
	if err := dropAbandonedTables(ctx, conn, "^("+chPreparedUsageRebuildLock+"|"+chPreparedUsageBuildPrefix+"[a-z0-9]+)$"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "CREATE TABLE "+chPreparedUsageRebuildLock+" (x UInt8) ENGINE = Memory")
	if ex, ok := errors.AsType[*chdriver.Exception](err); ok && ex.Code == codeTableAlreadyExists {
		log.Print("clickhouse push: another push is rebuilding prepared usage; skipping this rebuild")
		return nil
	}
	if err != nil {
		return fmt.Errorf("locking prepared usage rebuild: %w", err)
	}
	defer func() {
		if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+chPreparedUsageRebuildLock+" SYNC"); dropErr != nil {
			err = errors.Join(err, fmt.Errorf("unlocking prepared usage rebuild: %w", dropErr))
		}
	}()
	current, err := preparedUsageCurrent(ctx, conn)
	if err != nil || current {
		return err
	}
	log.Print("clickhouse push: rebuilding prepared usage")
	started := time.Now()
	build := chPreparedUsageBuildPrefix + strings.ToLower(rand.Text())
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
		return fmt.Errorf("filling prepared usage build table: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE prepared_usage REPLACE PARTITION tuple() FROM "+build); err != nil {
		return fmt.Errorf("replacing prepared usage rows: %w", err)
	}
	log.Printf("clickhouse push: rebuilt prepared usage in %s", time.Since(started).Round(time.Millisecond))
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

// dropAbandonedTables drops the tables whose names match pattern and whose
// metadata is more than an hour old: tables a push stopped by a crash or a
// kill left behind. Callers pass patterns that match only names they make,
// which need no quoting.
func dropAbandonedTables(ctx context.Context, conn *sql.DB, pattern string) error {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM system.tables
		WHERE database = currentDatabase() AND match(name, ?)
		AND metadata_modification_time < now() - INTERVAL 1 HOUR`, pattern)
	if err != nil {
		return fmt.Errorf("listing abandoned tables: %w", err)
	}
	defer rows.Close()
	var abandoned []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning abandoned table: %w", err)
		}
		abandoned = append(abandoned, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing abandoned tables: %w", err)
	}
	for _, name := range abandoned {
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+name+" SYNC"); err != nil {
			return fmt.Errorf("dropping abandoned table %s: %w", name, err)
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
