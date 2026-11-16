package duckdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// The usage rollup keeps priced usage summed by session, 15-minute UTC
// bucket, model, and provider, so Usage summary, Top sessions, and session
// counts do not rank and price every usage row on each read.
//
// A push rebuilds the whole rollup when none exists, when its format or
// pricing changed, or when the usage rows of sessions changed since the
// build reach 1/usageRollupRebuildDivisor of the rows it covers. Reads take
// changed, new, and deleted sessions from the live tables; see
// usage_rollup_read.go for how the two halves meet.
//
// A dedup group is summed into the rollup ("finalized") only when no query
// window, filter, or row outside the build can change which of its rows
// wins: all its rows belong to one session and one bucket, a non-Claude
// group has one model (model filters apply before its ranking), no row
// carries a Copilot-reported session cost, and no Cursor account row shares
// its identity. Every other group's rows are copied raw into
// usage_rollup_exceptions and ranked at read time with today's SQL.
const (
	usageRollupFormat         = 2
	usageRollupMetadataKey    = "agentsview_usage_rollup"
	usageRollupRebuildDivisor = 50
	usageRollupBucketSeconds  = 15 * 60
)

func rollupColumns(defs ...string) []columnSpec {
	out := make([]columnSpec, len(defs))
	for i, def := range defs {
		name, _, _ := strings.Cut(def, " ")
		out[i] = columnSpec{name: name, def: def}
	}
	return out
}

func rollupTable(name string, columns []columnSpec) tableSpec {
	defs := make([]string, len(columns))
	for i, column := range columns {
		defs[i] = column.def
	}
	return tableSpec{
		name:    name,
		create:  "CREATE TABLE IF NOT EXISTS " + name + " (\n\t\t\t" + strings.Join(defs, ",\n\t\t\t") + "\n\t\t)",
		columns: columns,
	}
}

var (
	usageRollupColumns = rollupColumns(
		"session_id TEXT NOT NULL", "bucket TIMESTAMP",
		"model TEXT NOT NULL", "provider_id TEXT NOT NULL",
		"input_tokens BIGINT NOT NULL", "output_tokens BIGINT NOT NULL",
		"cache_creation_tokens BIGINT NOT NULL", "cache_read_tokens BIGINT NOT NULL",
		"cost_microdollars BIGINT NOT NULL", "savings_microdollars BIGINT NOT NULL",
	)
	// A row with a NULL band carries a record's flags, base requests, and
	// aggregate rows; a banded row carries that band's requests.
	usageRollupPricingColumns = rollupColumns(
		"session_id TEXT NOT NULL", "bucket TIMESTAMP", "model TEXT NOT NULL",
		"record_id INTEGER NOT NULL", "band BIGINT",
		"requests BIGINT NOT NULL", "aggregates BIGINT NOT NULL",
		"computed BOOLEAN NOT NULL", "reported BOOLEAN NOT NULL",
	)
	usageRollupRecordColumns = rollupColumns(
		"record_id INTEGER NOT NULL", "record BLOB NOT NULL",
	)
)

var usageRollupTables = []tableSpec{
	rollupTable("usage_rollup_sessions", rollupColumns(
		"session_id TEXT NOT NULL", "fingerprint TEXT NOT NULL",
	)),
	rollupTable("usage_rollup", usageRollupColumns),
	rollupTable("usage_rollup_pricing", usageRollupPricingColumns),
	rollupTable("usage_rollup_records", usageRollupRecordColumns),
	// Exception rows keep the row-level columns of duckUsageRawSQL; reads take
	// session columns, and ts for rows without their own time, from sessions.
	rollupTable("usage_rollup_exceptions", rollupColumns(
		"session_id TEXT NOT NULL", "bucket TIMESTAMP",
		"message_ordinal BIGINT", "source TEXT NOT NULL", "pricing_ts TIMESTAMP",
		"model TEXT NOT NULL", "provider_id TEXT NOT NULL",
		"claude_message_id TEXT NOT NULL", "claude_request_id TEXT NOT NULL",
		"source_uuid TEXT NOT NULL", "usage_dedup_key TEXT NOT NULL",
		"input_tokens BIGINT", "output_tokens BIGINT",
		"cache_create BIGINT", "cache_read BIGINT", "reasoning_tokens BIGINT",
		"cache_create_1h BIGINT", "web_search_requests BIGINT",
		"cost_microdollars BIGINT", "cost_source TEXT NOT NULL",
		"recorded_cost_microdollars BIGINT",
	)),
	// One row per finalized group whose identity another session could
	// share. identity is hash(dedup_group); a collision only sends more
	// units to the live path.
	rollupTable("usage_rollup_identities", rollupColumns(
		"identity UBIGINT NOT NULL", "session_id TEXT NOT NULL", "bucket TIMESTAMP",
	)),
}

// usageRollupMeta describes the rollup a push built.
type usageRollupMeta struct {
	Format        int
	PricingDigest string
	AliasesDigest string
	// SourceRows counts the usage rows the build read.
	SourceRows int64
	// MinBucket and MaxBucket bound the timed buckets; nil when none.
	MinBucket, MaxBucket *time.Time
}

func (s *Store) readUsageRollupMeta(ctx context.Context) (usageRollupMeta, bool, error) {
	var value string
	err := s.queryRowContext(ctx, `SELECT value FROM sync_metadata WHERE key = ?`, usageRollupMetadataKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return usageRollupMeta{}, false, nil
	}
	if err != nil {
		return usageRollupMeta{}, false, fmt.Errorf("reading duckdb usage rollup metadata: %w", err)
	}
	var meta usageRollupMeta
	if err := json.Unmarshal([]byte(value), &meta); err != nil {
		return usageRollupMeta{}, false, fmt.Errorf("decoding duckdb usage rollup metadata: %w", err)
	}
	return meta, true, nil
}

// usageRollupFreshSessionsSQL lists the sessions whose rows the rollup
// still describes: present, not deleted, and pushed with the fingerprint
// the build saw.
const usageRollupFreshSessionsSQL = `SELECT b.session_id FROM usage_rollup_sessions b
			JOIN sessions fs ON fs.id = b.session_id
			WHERE fs.deleted_at IS NULL AND b.fingerprint != ''
				AND COALESCE(fs.agentsview_push_fingerprint, '') = b.fingerprint`

func duckUsageBucketSQL(ts string) string {
	return "time_bucket(INTERVAL '15 minutes', " + ts + ")"
}

// duckUsageUnitKeySQL names a (session, bucket) unit as one string. The
// bucket part is a number or 'n', so the last '|' separates the two.
func duckUsageUnitKeySQL(session, bucket string) string {
	return session + " || '|' || COALESCE(CAST(epoch_us(" + bucket + ") AS VARCHAR), 'n')"
}

// refreshUsageRollup rebuilds the rollup when one is due and reports
// whether it did.
func (s *Sync) refreshUsageRollup(ctx context.Context) (bool, error) {
	store := NewStoreFromDB(s.duck)
	reason, err := usageRollupRebuildReason(ctx, store)
	if err != nil || reason == "" {
		return false, err
	}
	start := time.Now()
	meta, err := s.buildUsageRollup(ctx)
	if err != nil {
		return false, err
	}
	log.Printf("duckdbsync: rebuilt usage rollup (%s) over %d usage rows in %s",
		reason, meta.SourceRows, time.Since(start).Round(time.Millisecond))
	return true, nil
}

func usageRollupRebuildReason(ctx context.Context, store *Store) (string, error) {
	meta, ok, err := store.readUsageRollupMeta(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "no rollup", nil
	}
	if meta.Format != usageRollupFormat {
		return "format changed", nil
	}
	digest, err := usageRollupPricingDigest(ctx, store)
	if err != nil {
		return "", err
	}
	if meta.PricingDigest != digest || meta.AliasesDigest != pricingpkg.ConfiguredAliasesDigest() {
		return "pricing changed", nil
	}
	var changedSessions int64
	if err := store.queryRowContext(ctx, `SELECT COUNT(*) FROM sessions s
		WHERE s.deleted_at IS NULL AND s.id NOT IN (`+usageRollupFreshSessionsSQL+`)`).
		Scan(&changedSessions); err != nil {
		return "", fmt.Errorf("counting duckdb sessions changed since the usage rollup: %w", err)
	}
	if changedSessions == 0 {
		return "", nil
	}
	var changedRows int64
	if err := store.queryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE `+duckUsageMessageEligibility+` AND s.id NOT IN (`+usageRollupFreshSessionsSQL+`))
		+ (SELECT COUNT(*) FROM usage_events ue JOIN sessions s ON s.id = ue.session_id
			WHERE `+duckUsageEventEligibility+` AND s.id NOT IN (`+usageRollupFreshSessionsSQL+`))`).
		Scan(&changedRows); err != nil {
		return "", fmt.Errorf("counting duckdb usage rows changed since the usage rollup: %w", err)
	}
	if changedRows*usageRollupRebuildDivisor >= max(meta.SourceRows, 1) {
		return fmt.Sprintf("%d of %d usage rows changed", changedRows, meta.SourceRows), nil
	}
	return "", nil
}

// usageRollupPricingDigest is the digest of the mirror's own pricing, which
// is what a push prices with; a serving store with custom pricing differs.
func usageRollupPricingDigest(ctx context.Context, store *Store) (string, error) {
	resolver, err := store.loadPricingResolver(ctx)
	if err != nil {
		return "", err
	}
	block, err := resolver.BuildBlock()
	if err != nil {
		return "", err
	}
	return block.Digest, nil
}

// usageRollupRecord is the part of a pricing record that names its lookup.
type usageRollupRecord struct {
	ReportedModel, PricedModel string
	Lookup                     export.PricingLookup
}

// buildUsageRollup replaces the rollup with one built from the mirror's
// current rows, in one transaction.
func (s *Sync) buildUsageRollup(ctx context.Context) (meta usageRollupMeta, err error) {
	store := NewStoreFromDB(s.duck)
	resolver, err := store.loadPricingResolver(ctx)
	if err != nil {
		return meta, err
	}
	block, err := resolver.BuildBlock()
	if err != nil {
		return meta, err
	}
	meta = usageRollupMeta{
		Format: usageRollupFormat, PricingDigest: block.Digest,
		AliasesDigest: pricingpkg.ConfiguredAliasesDigest(),
	}
	conn, err := s.duck.Conn(ctx)
	if err != nil {
		return meta, err
	}
	defer conn.Close()
	defer func() {
		// Drop the scratch tables even when ctx was canceled; the connection
		// returns to the pool afterwards.
		cleanup := context.WithoutCancel(ctx)
		for _, table := range []string{"usage_rollup_raw", "usage_rollup_groups"} {
			_, dropErr := conn.ExecContext(cleanup, "DROP TABLE IF EXISTS temp."+table)
			err = errors.Join(err, dropErr)
		}
	}()
	rawSQL, rawArgs := duckUsageRawSQL(db.UsageFilter{}, nil)
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE usage_rollup_raw AS
		SELECT *, `+duckUsageDedupGroupSQL+` AS dedup_group,
			`+duckUsageBucketSQL("ts")+` AS bucket
		FROM (`+rawSQL+`)`, rawArgs...); err != nil {
		return meta, fmt.Errorf("reading duckdb usage rows for the rollup: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE usage_rollup_groups AS
		SELECT dedup_group,
			COUNT(DISTINCT session_id) = 1
			AND COUNT(DISTINCT COALESCE(epoch_us(bucket), -1)) = 1
			AND (BOOL_AND(claude_message_id != '') OR COUNT(DISTINCT model) = 1)
			AND NOT BOOL_OR(cost_microdollars IS NOT NULL AND cost_source = '`+db.CopilotReportedCostSource+`')
			AND dedup_group NOT IN (
				SELECT 'usage:' || dedup_key FROM cursor_usage_events WHERE dedup_key != ''
			) AS finalized
		FROM usage_rollup_raw
		GROUP BY dedup_group`); err != nil {
		return meta, fmt.Errorf("classifying duckdb usage dedup groups: %w", err)
	}
	var minBucket, maxBucket sql.NullTime
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), MIN(bucket), MAX(bucket) FROM usage_rollup_raw`).
		Scan(&meta.SourceRows, &minBucket, &maxBucket); err != nil {
		return meta, fmt.Errorf("measuring duckdb usage rollup rows: %w", err)
	}
	if minBucket.Valid {
		lo, hi := minBucket.Time.UTC(), maxBucket.Time.UTC()
		meta.MinBucket, meta.MaxBucket = &lo, &hi
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return meta, fmt.Errorf("begin duckdb usage rollup tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range usageRollupTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table.name); err != nil {
			return meta, fmt.Errorf("clearing duckdb %s: %w", table.name, err)
		}
	}
	finalizedRows := `SELECT * EXCLUDE (dedup_group, bucket) FROM usage_rollup_raw
		WHERE dedup_group IN (SELECT dedup_group FROM usage_rollup_groups WHERE finalized)`
	order, sums, pricing, err := collectUsageRollup(ctx, tx, finalizedRows, resolver)
	if err != nil {
		return meta, err
	}
	if err := conn.Raw(func(raw any) (err error) {
		batch, err := newDuckAppendBatch(ctx, raw, map[string][]columnSpec{
			"usage_rollup": usageRollupColumns, "usage_rollup_pricing": usageRollupPricingColumns,
			"usage_rollup_records": usageRollupRecordColumns,
		})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, batch.close(ctx)) }()
		return appendUsageRollup(ctx, batch, order, sums, pricing)
	}); err != nil {
		return meta, fmt.Errorf("writing duckdb usage rollup: %w", err)
	}
	for _, stmt := range []string{
		`INSERT INTO usage_rollup_exceptions
		SELECT session_id, bucket, message_ordinal, source, pricing_ts, model,
			provider_id, claude_message_id, claude_request_id,
			source_uuid, usage_dedup_key, input_tokens, output_tokens,
			cache_create, cache_read, reasoning_tokens,
			cache_create_1h, web_search_requests, cost_microdollars,
			cost_source, recorded_cost_microdollars
		FROM usage_rollup_raw
		WHERE dedup_group IN (SELECT dedup_group FROM usage_rollup_groups WHERE NOT finalized)`,
		`INSERT INTO usage_rollup_identities
		SELECT DISTINCT hash(dedup_group), session_id, bucket
		FROM usage_rollup_raw
		WHERE NOT starts_with(dedup_group, 'row:')
			AND dedup_group IN (SELECT dedup_group FROM usage_rollup_groups WHERE finalized)`,
		`INSERT INTO usage_rollup_sessions
		SELECT id, COALESCE(agentsview_push_fingerprint, '') FROM sessions WHERE deleted_at IS NULL`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return meta, fmt.Errorf("writing duckdb usage rollup: %w", err)
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return meta, fmt.Errorf("encoding duckdb usage rollup metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		usageRollupMetadataKey, string(encoded)); err != nil {
		return meta, fmt.Errorf("recording duckdb usage rollup metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return meta, fmt.Errorf("commit duckdb usage rollup: %w", err)
	}
	return meta, nil
}

type usageRollupKey struct {
	session  string
	bucket   sql.NullTime
	model    string
	provider string
}

type usageRollupSums struct {
	input, output, cacheCr, cacheRd int64
	cost, savings                   money.Money
}

type usageRollupPricingRow struct {
	key     usageRollupKey
	records []export.PricingRecord
}

// collectUsageRollup ranks the finalized groups with the read path's SQL and
// prices every surviving row with the read path's Go. It sums them by
// (session, bucket, model, provider) and keeps their pricing records by
// (session, bucket, model).
func collectUsageRollup(
	ctx context.Context, tx *sql.Tx, rawSQL string, resolver *export.PricingResolver,
) ([]usageRollupKey, map[usageRollupKey]*usageRollupSums, []usageRollupPricingRow, error) {
	cte, args := duckUsageCTEFromRaw(db.UsageFilter{Timezone: "UTC"}, rawSQL, nil, true)
	rows, err := tx.QueryContext(ctx, cte+`
		SELECT session_id, `+duckUsageBucketSQL("ts")+` AS bucket, model, provider_id,
			price_model, source, message_ordinal, pricing_ts,
			`+duckUsageAggregateValueSQL+`
		FROM usage_localized
		ORDER BY session_id, bucket NULLS FIRST, model, provider_id`, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ranking duckdb usage rollup rows: %w", err)
	}
	defer rows.Close()
	var order []usageRollupKey
	sums := map[usageRollupKey]*usageRollupSums{}
	var pricing []usageRollupPricingRow
	var pricingKey usageRollupKey
	var keyResolver *export.PricingResolver
	flush := func() {
		if keyResolver != nil {
			records, _ := keyResolver.RecordedCounts()
			pricing = append(pricing, usageRollupPricingRow{pricingKey, records})
		}
	}
	for rows.Next() {
		var r duckUsageAggregateRow
		var bucket sql.NullTime
		var pricingTS any
		dest := append([]any{
			&r.sessionID, &bucket, &r.model, &r.providerID,
			&r.priceModel, &r.source, &r.messageOrdinal, &pricingTS,
		}, r.aggregateValueDest()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, nil, fmt.Errorf("scanning duckdb usage rollup row: %w", err)
		}
		r.pricingTS = formatDBTime(pricingTS)
		pk := usageRollupKey{session: r.sessionID, bucket: bucket, model: r.model}
		if keyResolver == nil || pk != pricingKey {
			flush()
			pricingKey, keyResolver = pk, resolver.Copy()
		}
		cost, savings, err := r.priceForAggregate(keyResolver)
		if err != nil {
			return nil, nil, nil, err
		}
		k := usageRollupKey{session: r.sessionID, bucket: bucket, model: r.model, provider: r.providerID}
		a := sums[k]
		if a == nil {
			a = &usageRollupSums{}
			sums[k] = a
			order = append(order, k)
		}
		a.input += int64(r.inputTok)
		a.output += int64(r.outputTok)
		a.cacheCr += int64(r.cacheCr)
		a.cacheRd += int64(r.cacheRd)
		if a.cost, err = money.Add(a.cost, cost); err != nil {
			return nil, nil, nil, fmt.Errorf("summing duckdb usage rollup cost: %w", err)
		}
		if a.savings, err = money.Add(a.savings, savings); err != nil {
			return nil, nil, nil, fmt.Errorf("summing duckdb usage rollup savings: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("iterating duckdb usage rollup rows: %w", err)
	}
	flush()
	return order, sums, pricing, nil
}

func appendUsageRollup(
	ctx context.Context, batch *duckAppendBatch, order []usageRollupKey,
	sums map[usageRollupKey]*usageRollupSums, pricing []usageRollupPricingRow,
) error {
	bucketValue := func(b sql.NullTime) any {
		if !b.Valid {
			return nil
		}
		return b.Time
	}
	for _, k := range order {
		a, ok := sums[k]
		if !ok || a == nil {
			return fmt.Errorf("usage rollup key %v has no sums", k)
		}
		if err := batch.add(ctx, "usage_rollup", k.session, bucketValue(k.bucket), k.model, k.provider,
			a.input, a.output, a.cacheCr, a.cacheRd, a.cost.Microdollars, a.savings.Microdollars); err != nil {
			return err
		}
	}
	recordIDs := map[string]int{}
	for _, row := range pricing {
		k := row.key
		for _, rec := range row.records {
			id, err := usageRollupRecordID(ctx, batch, recordIDs, rec)
			if err != nil {
				return err
			}
			if err := batch.add(ctx, "usage_rollup_pricing", k.session, bucketValue(k.bucket), k.model,
				id, nil, rec.BaseRequests, rec.AggregateRows, rec.Computed, rec.Reported); err != nil {
				return err
			}
			for band, count := range rec.BandRequests {
				if err := batch.add(ctx, "usage_rollup_pricing", k.session, bucketValue(k.bucket), k.model,
					id, band, count, 0, false, false); err != nil {
					return err
				}
			}
		}
	}
	return batch.flush(ctx)
}

func usageRollupRecordID(
	ctx context.Context, batch *duckAppendBatch, ids map[string]int, rec export.PricingRecord,
) (int, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(usageRollupRecord{rec.ReportedModel, rec.PricedModel, rec.Lookup}); err != nil {
		return 0, fmt.Errorf("encoding duckdb usage rollup pricing record: %w", err)
	}
	key := buf.String()
	if id, ok := ids[key]; ok {
		return id, nil
	}
	id := len(ids)
	ids[key] = id
	return id, batch.add(ctx, "usage_rollup_records", id, buf.Bytes())
}
