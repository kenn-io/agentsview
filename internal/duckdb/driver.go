//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strings"

	// Registers the "duckdb" database/sql driver, which statically links
	// the prebuilt DuckDB library from duckdb-go-bindings.
	duckdbdriver "github.com/duckdb/duckdb-go/v2"
)

func openDuckDB(dsn string) (*sql.DB, error) {
	return sql.Open("duckdb", dsn)
}

func isMirrorOpenInSameProcessError(err error) bool {
	return errors.Is(err, &duckdbdriver.Error{
		Msg: "Connection Error: Can't open a connection to same database file with a different configuration than existing connections",
	})
}

// duckAppendBatch shares one payload budget across all five tables.
// A row larger than the budget is flushed alone, without truncation.
type duckAppendBatch struct {
	conn      driver.QueryerContext
	appenders map[string]*duckdbdriver.Appender
	columns   map[string][]columnSpec
	dirty     map[string]bool
	bytes     int
}

func newDuckAppendBatch(ctx context.Context, raw any, columns map[string][]string) (*duckAppendBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := &duckAppendBatch{dirty: make(map[string]bool), conn: raw.(driver.QueryerContext), appenders: make(map[string]*duckdbdriver.Appender), columns: make(map[string][]columnSpec)}
	for table, names := range columns {
		for _, name := range names {
			column := columnSpec{name: name}
			for _, spec := range mirrorTables {
				if spec.name != table {
					continue
				}
				for _, candidate := range spec.columns {
					if candidate.name == name {
						column = candidate
						break
					}
				}
				break
			}
			b.columns[table] = append(b.columns[table], column)
		}
		a, err := duckdbdriver.NewAppenderWithColumns(raw.(driver.Conn), "", "", table, names)
		if err != nil {
			return nil, errors.Join(err, b.close(ctx))
		}
		b.appenders[table] = a
	}
	return b, nil
}

func (b *duckAppendBatch) add(ctx context.Context, table string, values ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	columns, ok := b.columns[table]
	if !ok {
		return errors.New("missing appender columns for " + table)
	}
	row := make([]driver.Value, len(values))
	payload := 0
	for i, value := range values {
		// The SQL driver accepts list values directly; the stdlib converter doesn't.
		if list, ok := value.([]string); ok {
			row[i] = list
			for _, item := range list {
				payload += len(item)
			}
			continue
		}
		converted, err := driver.DefaultParameterConverter.ConvertValue(value)
		if err != nil {
			return err
		}
		switch v := converted.(type) {
		case int64:
			if strings.Contains(" "+columns[i].def+" ", " INTEGER ") && (v < math.MinInt32 || v > math.MaxInt32) {
				return fmt.Errorf("%s.%s: value %d out of range for INTEGER", table, columns[i].name, v)
			}
		case string:
			payload += len(v)
			switch columns[i].name {
			case "started_at", "ended_at", "local_modified_at", "deleted_at", "created_at", "timestamp", "occurred_at":
				// timeValue leaves DuckDB-only timestamp formats as strings. Preserve
				// the SQL cast on this same transaction connection before appending.
				rows, err := b.conn.QueryContext(ctx, "SELECT CAST(? AS TIMESTAMP)", []driver.NamedValue{{Ordinal: 1, Value: v}})
				if err != nil {
					return err
				}
				result := make([]driver.Value, 1)
				err = rows.Next(result)
				err = errors.Join(err, rows.Close())
				if err != nil {
					return err
				}
				converted = result[0]
			}
		case []byte:
			payload += len(v)
		}
		row[i] = converted
	}
	if b.bytes > 0 && b.bytes+payload > duckInsertBatchBytes {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	if duckAppendHook != nil {
		if err := duckAppendHook(table, false); err != nil {
			return err
		}
	}
	if err := b.appenders[table].AppendRow(row...); err != nil {
		return err
	}
	b.dirty[table] = true
	b.bytes += payload
	if b.bytes >= duckInsertBatchBytes {
		return b.flush(ctx)
	}
	return nil
}

func (b *duckAppendBatch) flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for table, appender := range b.appenders {
		if !b.dirty[table] {
			continue
		}
		if err := appender.FlushWithCancel(ctx); err != nil {
			return err
		}
		if duckAppendHook != nil {
			if err := duckAppendHook(table, true); err != nil {
				return err
			}
		}
	}
	clear(b.dirty)
	b.bytes = 0
	return nil
}

func (b *duckAppendBatch) close(ctx context.Context) error {
	var err error
	for _, appender := range b.appenders {
		err = errors.Join(err, appender.CloseWithCancel(ctx))
	}
	return err
}
