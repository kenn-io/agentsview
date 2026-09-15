package duckdb

import "time"

// duckMaxSQLVars bounds the IN-list size per query to stay well under
// driver bind-variable limits; larger ID sets are split into chunks.
const duckMaxSQLVars = 900

func duckQueryChunked(ids []string, fn func(chunk []string) error) error {
	for i := 0; i < len(ids); i += duckMaxSQLVars {
		end := min(i+duckMaxSQLVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func stringInArgs(values []string) ([]any, []string) {
	args := make([]any, len(values))
	placeholders := make([]string, len(values))
	for i, value := range values {
		args[i] = value
		placeholders[i] = "?"
	}
	return args, placeholders
}

func parseDuckTime(ts string) (time.Time, bool) {
	if t, ok := parseTimestamp(ts); ok {
		return t, true
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
