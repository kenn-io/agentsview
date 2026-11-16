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
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// The usage rollup holds, per prepared usage stamp, the daily usage groups
// of prepared_usage already ranked, deduplicated, and priced, summed by
// session and 15-minute UTC bucket. A usage read adds these sums to the
// groups it ranks itself from the rows the rollup leaves out, so it ranks a
// few thousand rows instead of every row in its range.
//
// A bucket's sums are only valid when ranking the bucket's rows alone gives
// the same survivors as ranking every row. That holds for a (session,
// bucket) unit when each of its events, its dedup_group, has every row in
// the unit, under one timestamp state, and, unless it is a Claude request,
// under one model: the read's filters and date range then keep or drop the
// whole event, as the full ranking would. Such a unit is summed. Every row
// of any other unit is stored as is, as an exception row, and every read
// ranks those rows with the ones it prepares itself.
//
// Rows can still join a summed unit's events later: a session pushed since
// the refresh, or a cursor usage row. The rollup keeps one identity row per
// summed event, and a read that holds a row naming one of those events ranks
// that event's unit from prepared_usage instead of using its sums.
//
// Rows are partitioned by the stamp of the prepared rows they were built
// from, so a read takes only the sums of the rows its state describes. A
// push builds the partition once prepared_usage holds new rows and drops
// partitions an hour after a newer one replaced them; a read whose partition
// is dropped while it runs reads again without the rollup.
const (
	usageRollupTable = "usage_rollup"
	// chUsageRollupBuildPrefix names the tables a push fills before it moves
	// their rows into usage_rollup.
	chUsageRollupBuildPrefix  = "usage_rollup_build_"
	chUsageRollupBuildPattern = "^" + chUsageRollupBuildPrefix + "[a-z0-9]+$"
	// chUsageRollupBucketSeconds is the bucket width. Every UTC offset in
	// use today is a multiple of 15 minutes, so a bucket falls on one local
	// date in every zone a read is likely to name.
	chUsageRollupBucketSeconds = 900
	// chUsageRollupKeyPlaceholder stands in for the rollup format in the
	// statements chUsageRollupComment hashes, which embed the format.
	chUsageRollupKeyPlaceholder = "{rollup_format}"
)

// chUsageRollupBucketSQL renders the start of the 15-minute UTC bucket of
// the DateTime64 expression col, in Unix seconds. The functions that round
// a time to an interval return DateTime, which clamps times outside 1970
// to 2106.
func chUsageRollupBucketSQL(col string) string {
	micros := "toUnixTimestamp64Micro(" + col + ")"
	const width = "900000000"
	return "((intDiv(" + micros + ", " + width + ") - (" + micros + " % " + width + " < 0)) * " +
		strconv.Itoa(chUsageRollupBucketSeconds) + ")"
}

// chUsageRollupColumns are the rollup's own columns. Rows also carry the
// prepared row columns: an exception row is a prepared row, and a summed
// group keeps the per-row values Go reads from an explicit group in the
// prepared columns of the same name.
var chUsageRollupColumns = []struct{ name, typ string }{
	{"rollup_key", "String"},
	{"row_kind", "Enum8('aggregate' = 1, 'exception' = 2, 'identity' = 3)"},
	// sort_key is an identity row's event hash and zero otherwise.
	{"sort_key", "UInt64"},
	{"bucket_key", "Int64"},
	{"ts_null", "Bool"},
	{"r_explicit_key", "String"},
	{"r_group_kind", "String"},
	{"r_group_context", "String"},
	{"r_group_band", "Int64"},
	{"r_events", "Int64"},
	{"r_input_tokens", "Int64"},
	{"r_output_tokens", "Int64"},
	{"r_cache_create", "Int64"},
	{"r_cache_read", "Int64"},
	{"r_token_cost", "Int128"},
	{"r_savings", "Int128"},
	{"r_explicit_cost", "Int128"},
	{"r_web_search", "Int64"},
	{"r_price_error", "String"},
	{"r_billable_input", "Int64"},
	{"r_billable_output", "Int64"},
	{"r_billable_reasoning", "Int64"},
	{"r_billable_cache_create", "Int64"},
	{"r_billable_cache_create_1h", "Int64"},
	{"r_billable_cache_read", "Int64"},
	{"r_reported_cost_rows", "Int64"},
	{"r_authoritative_cost", "Nullable(Int64)"},
	{"r_authoritative_cost_rows", "Int64"},
}

// chUsageRollupTableSQL creates usage_rollup with the prepared row columns
// prepared_usage has now.
func chUsageRollupTableSQL(comment string) string {
	columns := make([]string, 0, len(chUsageRollupColumns))
	for _, c := range chUsageRollupColumns {
		columns = append(columns, "CAST(defaultValueOfTypeName("+chSQLString(c.typ)+") AS "+c.typ+") AS "+c.name)
	}
	return `CREATE TABLE IF NOT EXISTS ` + usageRollupTable + `
		ENGINE = MergeTree PARTITION BY rollup_key ORDER BY (row_kind, sort_key, bucket_key, session_id)
		COMMENT ` + chSQLString(comment) + `
		AS SELECT ` + strings.Join(columns, ", ") + `, ` + chPreparedUsageRowColumns + `
		FROM prepared_usage LIMIT 0`
}

// chUsageRollupKeySQL renders a prepared row's rollup key from its stamp
// columns, in the form usageRollupKey gives a read.
func chUsageRollupKeySQL(format string) string {
	return "ifNull(concat(" + chSQLString(format) + ", '|', toString(snapshot_count), '|', " +
		"toString(snapshot_hash), '|', pricing_digest, '|', toString(price_count)), '')"
}

// chUsageRollupExceptionsSQL fills build with the rows of every unit the
// rollup cannot sum; see the usage_rollup comment. An event whose identity a
// cursor usage row also names is ranked with that row by every daily read,
// so its unit is not summed either.
func chUsageRollupExceptionsSQL(build, format string) string {
	bucket := chUsageRollupBucketSQL(chPreparedUsageKeyColumn)
	return `INSERT INTO ` + build + ` (rollup_key, row_kind, sort_key, bucket_key, ts_null, ` + chPreparedUsageRowColumns + `)
		SELECT ` + chUsageRollupKeySQL(format) + `, 'exception', 0, ` + bucket + `, ts IS NULL, ` + chPreparedUsageRowColumns + `
		FROM prepared_usage
		WHERE (session_id, ` + bucket + `) IN (
			SELECT unit.1, unit.2 FROM (
				SELECT dedup_group, groupUniqArray((session_id, rollup_bucket)) AS units
				FROM (
					SELECT ` + chUsageDedupGroupSQL + ` AS dedup_group, session_id,
						` + bucket + ` AS rollup_bucket, ts IS NULL AS rollup_ts_null, model,
						claude_message_id != '' AS rollup_claude
					FROM prepared_usage
				)
				GROUP BY dedup_group
				HAVING length(units) > 1 OR uniqExact(rollup_ts_null) > 1
					OR (NOT any(rollup_claude) AND uniqExact(model) > 1)
					OR dedup_group IN (SELECT concat('usage:', dedup_key) FROM cursor_usage_events WHERE dedup_key != '')
			) ARRAY JOIN units AS unit
		)
		SETTINGS ` + chPreparedUsageRefreshSettings
}

// chUsageRollupOpenUnitsSQL selects the units build holds exception rows
// for.
func chUsageRollupOpenUnitsSQL(build string) string {
	return "SELECT DISTINCT session_id, bucket_key FROM " + build + " WHERE row_kind = 'exception'"
}

// chUsageRollupIdentitiesSQL fills build with one identity row per summed
// event a later row can name. A row: identity names its own session, so only
// that session's rows can name it, and a read ranks a pushed session whole.
func chUsageRollupIdentitiesSQL(build, format string) string {
	bucket := chUsageRollupBucketSQL(chPreparedUsageKeyColumn)
	return `INSERT INTO ` + build + ` (rollup_key, row_kind, sort_key, bucket_key, session_id)
		SELECT any(rollup_key), 'identity', sipHash64(dedup_group), any(rollup_bucket), any(session_id)
		FROM (
			SELECT ` + chUsageRollupKeySQL(format) + ` AS rollup_key, ` + chUsageDedupGroupSQL + ` AS dedup_group,
				session_id, ` + bucket + ` AS rollup_bucket
			FROM prepared_usage
			WHERE (session_id, ` + bucket + `) NOT IN (` + chUsageRollupOpenUnitsSQL(build) + `)
		)
		WHERE NOT startsWith(dedup_group, 'row:')
		GROUP BY dedup_group
		SETTINGS ` + chPreparedUsageRefreshSettings
}

// chUsageRollupAggregatesSQL fills build with the summed units' daily usage
// groups, split further by bucket, timestamp state, and price model. It runs
// the read's ranking and pricing over the units' prepared rows with no
// filter and no custom rates; a read whose custom rates reach a summed group
// does not use the rollup. The groups keep the session columns out: a read
// joins the live sessions.
func chUsageRollupAggregatesSQL(build, format string) (string, []any) {
	rawSQL := `SELECT ` + chPreparedUsageRawSelect() + `, ` + chUsageRollupKeySQL(format) + ` AS rollup_key
		FROM prepared_usage p
		WHERE (p.session_id, ` + chUsageRollupBucketSQL("p."+chPreparedUsageKeyColumn) + `) NOT IN (` +
		chUsageRollupOpenUnitsSQL(build) + `)`
	cte, args := chUsageCTEFromRawSource(db.UsageFilter{}, rawSQL, nil, true, chPreparedUsageNormSource(), "")
	pricedSQL, pricedArgs := chUsagePricedCTE(chUsageSource{storedPricesDigest: chUsageRollupStoredDigest}, chUsageRollupStoredDigest)
	args = append(args, pricedArgs...)
	bucket := chUsageRollupBucketSQL("ifNull(ts, toDateTime64(0, 6, 'UTC'))")
	return `INSERT INTO ` + build + ` (rollup_key, row_kind, sort_key, bucket_key, ts_null,
			session_id, model, provider_id, price_model,
			r_explicit_key, r_group_kind, r_group_context, r_group_band,
			r_events, r_input_tokens, r_output_tokens, r_cache_create, r_cache_read,
			r_token_cost, r_savings, r_explicit_cost, r_web_search, r_price_error,
			source, message_ordinal, ts, pricing_ts, cache_create_1h_norm, usage_dedup_key,
			recorded_cost_microdollars,
			r_billable_input, r_billable_output, r_billable_reasoning,
			r_billable_cache_create, r_billable_cache_create_1h, r_billable_cache_read,
			r_reported_cost_rows, r_authoritative_cost, r_authoritative_cost_rows)
		` + cte + pricedSQL + chUsageClassifiedCTE("''", "0") + `
		SELECT rollup_key, 'aggregate', 0, ` + bucket + ` AS rollup_bucket, ts IS NULL AS rollup_ts_null,
			session_id, model, provider_id, price_model,
			` + chUsageGroupKeySelect + `,
			toInt64(count()), sum(input_tokens_norm), sum(output_tokens_norm),
			sum(cache_create_norm), sum(cache_read_norm),
			sum(toInt128(p_token_cost)), sum(toInt128(p_savings)), sum(toInt128(explicit_cost)),
			sum(billable_web_search_requests), ` + chUsageGroupPriceErrorSQL + `,
			any(source), any(message_ordinal), any(ts), any(pricing_ts), any(cache_create_1h_norm),
			any(usage_dedup_key), any(recorded_cost_microdollars),
			any(billable_input_tokens), any(billable_output_tokens), any(billable_reasoning_tokens),
			any(billable_cache_creation_tokens), any(billable_cache_creation_1h_tokens),
			any(billable_cache_read_tokens),
			toInt64(any(reported_cost_rows)), any(authoritative_cost), toInt64(any(authoritative_cost_rows))
		FROM usage_classified
		GROUP BY rollup_key, session_id, rollup_bucket, rollup_ts_null, model, provider_id, price_model,
			explicit_key, group_kind, group_context, group_band
		SETTINGS ` + chPreparedUsageRefreshSettings, args
}

// chUsageRollupStoredDigest stands for the digest a build prices under. The
// prepared rows carry their price records, so the build takes them from the
// rows; the digest only has to match itself.
const chUsageRollupStoredDigest = "stored"

// chUsageRollupComment identifies the table shape and the statements that
// fill it, and the prepared rows they read. A table from another binary is
// replaced, and its rows never serve a read.
func chUsageRollupComment() string {
	const build = "usage_rollup_build"
	aggregates, _ := chUsageRollupAggregatesSQL(build, chUsageRollupKeyPlaceholder)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		chPreparedUsageComment(),
		chUsageRollupTableSQL(""),
		chUsageRollupExceptionsSQL(build, chUsageRollupKeyPlaceholder),
		chUsageRollupIdentitiesSQL(build, chUsageRollupKeyPlaceholder),
		aggregates,
	}, "\n")))
	return "agentsview usage rollup " + hex.EncodeToString(sum[:])
}

// usageRollupKey is the partition of the rollup built from the prepared rows
// a state describes, or "" when the state's rows were prepared by another
// binary.
func usageRollupKey(stamp string) string {
	rest, ok := strings.CutPrefix(stamp, chPreparedUsageComment()+"|")
	if !ok {
		return ""
	}
	return chUsageRollupComment() + "|" + rest
}

// ensureUsageRollup creates usage_rollup, replacing a table another binary
// created. Its rows are derived from prepared_usage, so dropping them loses
// nothing; reads rank every row until the next push builds them again.
func ensureUsageRollup(ctx context.Context, conn *sql.DB) error {
	comment := chUsageRollupComment()
	var current string
	err := conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = ?`, usageRollupTable).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading usage rollup shape: %w", err)
	}
	if err == nil && current != comment {
		log.Print("ClickHouse: rebuilding the usage rollup for the current usage query")
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+usageRollupTable+" SYNC"); err != nil {
			return fmt.Errorf("replacing usage rollup: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, chUsageRollupTableSQL(comment)); err != nil {
		return fmt.Errorf("creating usage rollup: %w", err)
	}
	return nil
}

// readPreparedUsageStamp reads the stamp of the rows prepared_usage holds,
// in the form preparedUsageState.stamp has, or "" when the table is empty or
// was prepared by another binary.
func readPreparedUsageStamp(ctx context.Context, conn *sql.DB) (string, error) {
	var comment string
	err := conn.QueryRowContext(ctx, `SELECT comment FROM system.tables
		WHERE database = currentDatabase() AND name = 'prepared_usage'`).Scan(&comment)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading prepared usage shape: %w", err)
	}
	if comment != chPreparedUsageComment() {
		return "", nil
	}
	var stored usageStamp
	// Every row of one refresh carries the same stamp, so any row will do.
	err = conn.QueryRowContext(ctx, `SELECT snapshot_count, snapshot_hash, pricing_digest, price_count
		FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(
		&stored.snapshotCount, &stored.snapshotHash, &stored.pricingDigest, &stored.priceCount)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading prepared usage stamp: %w", err)
	}
	return stored.format(comment), nil
}

// refreshUsageRollup builds the rollup of the rows prepared_usage holds
// when it has none yet. waitView reports that this push started the view's
// refresh, which the build waits for so it sums the refreshed rows.
func refreshUsageRollup(ctx context.Context, conn *sql.DB, waitView bool) error {
	if waitView {
		if _, err := conn.ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage"); err != nil {
			return fmt.Errorf("waiting for the prepared usage refresh: %w", err)
		}
	}
	// A push killed during a build leaves its table; a build takes seconds.
	if err := dropTables(ctx, conn, "match(name, ?) AND metadata_modification_time <= now() - INTERVAL 1 HOUR",
		chUsageRollupBuildPattern); err != nil {
		return err
	}
	stamp, err := readPreparedUsageStamp(ctx, conn)
	if err != nil || stamp == "" {
		return err
	}
	key := usageRollupKey(stamp)
	present, err := usageRollupPresent(ctx, conn, key)
	if err != nil {
		return err
	}
	if !present {
		if err := buildUsageRollup(ctx, conn, stamp, key); err != nil {
			return err
		}
	}
	return pruneUsageRollup(ctx, conn, key)
}

// usageRollupPresent reports whether usage_rollup holds the partition key.
func usageRollupPresent(ctx context.Context, conn *sql.DB, key string) (bool, error) {
	var present uint64
	if err := conn.QueryRowContext(ctx, `SELECT count() FROM (SELECT 1 FROM `+usageRollupTable+`
		WHERE rollup_key = ? LIMIT 1)`, key).Scan(&present); err != nil {
		return false, fmt.Errorf("checking usage rollup: %w", err)
	}
	return present != 0, nil
}

// buildUsageRollup fills a build table from prepared_usage and moves it into
// the partition key. The build reads prepared_usage in several statements,
// so it publishes only when the table's stamp is the same after them as
// before and every row it built names key.
func buildUsageRollup(ctx context.Context, conn *sql.DB, stamp, key string) (err error) {
	started := time.Now()
	build := chUsageRollupBuildPrefix + strings.ToLower(rand.Text())
	if _, err := conn.ExecContext(ctx, "CREATE TABLE "+build+" AS "+usageRollupTable); err != nil {
		return fmt.Errorf("creating usage rollup build table: %w", err)
	}
	defer func() {
		if _, dropErr := conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+build+" SYNC"); dropErr != nil {
			err = errors.Join(err, fmt.Errorf("dropping usage rollup build table: %w", dropErr))
		}
	}()
	format := chUsageRollupComment()
	aggregates, aggregateArgs := chUsageRollupAggregatesSQL(build, format)
	for _, step := range []struct {
		name  string
		query string
		args  []any
	}{
		{"exception rows", chUsageRollupExceptionsSQL(build, format), nil},
		{"event identities", chUsageRollupIdentitiesSQL(build, format), nil},
		{"usage sums", aggregates, aggregateArgs},
	} {
		if _, err := conn.ExecContext(ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("building usage rollup %s: %w", step.name, err)
		}
	}
	var keys []string
	if err := conn.QueryRowContext(ctx, "SELECT groupUniqArray(rollup_key) FROM "+build).Scan(&keys); err != nil {
		return fmt.Errorf("checking usage rollup build: %w", err)
	}
	after, err := readPreparedUsageStamp(ctx, conn)
	if err != nil {
		return err
	}
	if after != stamp || len(keys) != 1 || keys[0] != key {
		log.Print("clickhouse push: prepared usage changed while the usage rollup was built; the next push builds it")
		return nil
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+usageRollupTable+" REPLACE PARTITION "+
		chSQLString(key)+" FROM "+build); err != nil {
		return fmt.Errorf("publishing usage rollup: %w", err)
	}
	log.Printf("clickhouse push: built the usage rollup in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

// pruneUsageRollup drops the partitions other than key that no push has
// written for an hour. A read that started before a newer partition
// replaced one still finds it; a read that outlasts the hour sees it gone
// afterwards and reads again without the rollup.
func pruneUsageRollup(ctx context.Context, conn *sql.DB, key string) error {
	rows, err := conn.QueryContext(ctx, `SELECT partition_id FROM system.parts
		WHERE database = currentDatabase() AND table = ? AND active
		GROUP BY partition_id
		HAVING max(modification_time) <= now() - INTERVAL 1 HOUR
			AND partition_id NOT IN (SELECT _partition_id FROM `+usageRollupTable+` WHERE rollup_key = ? LIMIT 1)`,
		usageRollupTable, key)
	if err != nil {
		return fmt.Errorf("listing usage rollup partitions: %w", err)
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning usage rollup partition: %w", err)
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing usage rollup partitions: %w", err)
	}
	for _, id := range stale {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE "+usageRollupTable+" DROP PARTITION ID "+chSQLString(id)); err != nil {
			return fmt.Errorf("dropping usage rollup partition: %w", err)
		}
	}
	return nil
}
