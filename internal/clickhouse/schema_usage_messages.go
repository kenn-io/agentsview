package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

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

// chPriceStampSQL identifies the price records under the digest the refresh
// joins, so a mirror whose records were replaced or cleared is not read
// through the stored copies.
const chPriceStampSQL = `(SELECT count() FROM usage_event_prices WHERE pricing_digest = ` +
	chPreparedUsagePricingDigestSQL + `) AS price_count,
	(SELECT sum(sipHash64(price_key)) FROM usage_event_prices WHERE pricing_digest = ` +
	chPreparedUsagePricingDigestSQL + `) AS price_hash`

// chPreparedUsageQuery is the refresh query. Besides the normalized facts it
// stores the price model, the price key, and the price record for the
// current digest, so range reads skip the per-request join against every
// price record. Like the snapshot stamp, the price stamp is taken before the
// refresh scans, so records added during a refresh are also in the join.
func chPreparedUsageQuery() string {
	selection := clickUsageNormalizedQueryFrom("", chUsageStoredMessageEligibility, chUsageEventEligibility,
		"usage_session_snapshots s ARRAY JOIN s.usage_facts AS m",
		"usage_session_snapshots s ARRAY JOIN s.usage_events AS ue", "1")
	stored := make([]string, 0, len(chUsageStoredPriceColumns))
	for _, column := range chUsageStoredPriceColumns {
		stored = append(stored, "p."+column.joined+" AS "+column.stored)
	}
	return `SELECT n.*, ifNull(n.ts, toDateTime64(0,6,'UTC')) AS ` + chPreparedUsageKeyColumn + `,
		` + chSnapshotStampSQL + `,
		` + chPreparedUsagePricingDigestSQL + ` AS pricing_digest,
		` + chPriceStampSQL + `,
		` + strings.Join(stored, ", ") + `
	FROM (SELECT *, ` + chPriceModelCaseSQL() + ` AS price_model, ` + chUsagePriceKeySQL + ` AS price_key
		FROM (` + selection + `)) n
	LEFT JOIN (` + chUsagePriceRowsSQL() + ` WHERE pricing_digest = ` + chPreparedUsagePricingDigestSQL + `) p
		ON p.p_price_key = n.price_key`
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
	var hasView uint8
	if err := conn.QueryRowContext(ctx, `SELECT count() > 0 FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepare_usage'`).Scan(&hasView); err != nil {
		return fmt.Errorf("checking prepared usage view: %w", err)
	}
	queries := missing
	if hasView != 0 {
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
		// until it has. Stop the refresh first so it cannot block the drop.
		// An earlier rebuild may have dropped the view and stopped before
		// the table, so stop it only when it exists.
		log.Print("ClickHouse: rebuilding prepared usage for the current refresh query")
		var hasView uint8
		if err := conn.QueryRowContext(ctx, `SELECT count() > 0 FROM system.tables
			WHERE database = currentDatabase() AND name = 'prepare_usage'`).Scan(&hasView); err != nil {
			return fmt.Errorf("checking prepared usage view: %w", err)
		}
		queries := []string{
			"DROP VIEW IF EXISTS prepare_usage SYNC",
			"DROP TABLE IF EXISTS prepared_usage SYNC",
		}
		if hasView != 0 {
			queries = append([]string{"SYSTEM STOP VIEW prepare_usage"}, queries...)
		}
		for _, query := range queries {
			if _, err := conn.ExecContext(ctx, query); err != nil {
				return fmt.Errorf("replacing prepared usage: %w", err)
			}
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
	if _, err := conn.ExecContext(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS prepare_usage REFRESH `+chPreparedUsageRefresh+`
		TO prepared_usage EMPTY AS `+prepared+" SETTINGS final=1,max_threads=4"); err != nil {
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

func (s *Sync) insertUsageSessionSnapshots(
	ctx context.Context, payloads []sessionPayload, fingerprints map[string]string, version uint64,
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
		row := s.sessionRow(payload, fingerprints[payload.session.ID], version)
		rows = append(rows, append(row[:len(row)-1], messages, events, version))
	}
	return insertRowsSpec(ctx, s.conn, spec, rows)
}
