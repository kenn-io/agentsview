package duckdb

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
