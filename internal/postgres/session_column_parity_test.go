package postgres

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	localdb "go.kenn.io/agentsview/internal/db"
)

// sqliteOnlySessionColumns names every sessions column the SQLite
// archive carries and the PostgreSQL mirror deliberately does not,
// with the reason it stays local. Each reason comes from the
// column's own comment in internal/db/schema.sql.
//
// The mirror is a read-side store. Parsers, the file watcher, the
// sync engine, and the notification hub never run against it, so a
// column that only serves those paths has nothing to mirror: adding
// it to coreDDL, the push upsert, and sessionPushFingerprint would
// be inert plumbing that still has to be kept in step.
var sqliteOnlySessionColumns = map[string]string{
	// Machine-local parse state. The incremental parser reads these
	// to decide how to treat the file it is about to parse.
	"next_ordinal":        "local parse resume position",
	"last_entry_uuid":     "local incremental-append resume key",
	"claude_linear_parse": "whether the Claude parser fell back to linear processing for this file",
	// Consumed only by parse-diff, to suppress benign
	// incremental-versus-full skew.
	"last_write_incremental": "whether the last write used the incremental-append path",
	// Local push-eligibility watermark. The incremental push reads
	// it as a predicate; there is nothing to store on the far side.
	"local_modified_at": "local push-eligibility watermark",
	// A missing source must not hide or trash the archived session;
	// this only stops freshness checks from skipping a reparse when
	// the source returns.
	"source_missing_at": "when the local source file went missing",
	// Index-backed cache of the max-of-signals marker that the PG
	// push recomputes in Go, so the mirror would store a duplicate.
	"sync_marker": "materialized local sync marker",
	// The notification hub's candidate watermark: stamped only by
	// the archive's transcript-write path, read by exactly one
	// SQLite query.
	"transcript_modified_at": "notification candidate watermark",
	// Local file checkpoint state, used to detect rewrites and
	// replacements without re-reading the transcript.
	"file_size":   "local file stat",
	"file_mtime":  "local file stat",
	"file_hash":   "local content hash",
	"file_inode":  "local file identity",
	"file_device": "local file identity",
}

// TestPostgresSessionColumnsMatchSQLiteExceptDocumentedExceptions
// pins the sessions-column divergence between the archive and the
// mirror.
//
// The divergence is a deliberate exception, not drift, and this test
// is what keeps it deliberate: a new SQLite-only column fails here
// until it is named in the map above with its reason, and a column
// that starts being mirrored fails until it is removed from the map.
// The PostgreSQL side is read from coreDDL plus the columnMigration
// list EnsureSchema applies on top of it; the SQLite side is read
// from a freshly initialised archive, so it follows schema.sql and
// every schemaColumnMigration instead of a copy of them.
func TestPostgresSessionColumnsMatchSQLiteExceptDocumentedExceptions(t *testing.T) {
	pgColumns := postgresSessionsColumns(t)
	sqliteColumns := sqliteSessionsColumns(t)

	var undocumented []string
	for _, col := range sqliteColumns {
		if slices.Contains(pgColumns, col) {
			continue
		}
		if _, ok := sqliteOnlySessionColumns[col]; ok {
			continue
		}
		undocumented = append(undocumented, col)
	}
	assert.Empty(t, undocumented,
		"these sessions columns exist only in SQLite: mirror them "+
			"through the PostgreSQL schema, push SQL, and "+
			"sessionPushFingerprint, or name them in "+
			"sqliteOnlySessionColumns with the reason they stay local")

	var stale []string
	for col := range sqliteOnlySessionColumns {
		if slices.Contains(pgColumns, col) || !slices.Contains(sqliteColumns, col) {
			stale = append(stale, col)
		}
	}
	slices.Sort(stale)
	assert.Empty(t, stale,
		"sqliteOnlySessionColumns names columns that are now mirrored "+
			"or no longer exist in SQLite; drop them from the map")
}

// postgresSessionsColumns returns the sessions columns the mirror
// ends up with: the coreDDL baseline plus every columnMigration the
// schema step adds to the same table.
func postgresSessionsColumns(t *testing.T) []string {
	t.Helper()
	file := parseSchemaSource(t)

	columns := coreDDLSessionsColumns(t, file)
	columns = append(columns, migrationSessionsColumns(t, file)...)
	require.NotEmpty(t, columns, "parsed no sessions columns from schema.go")
	return columns
}

// coreDDLSessionsColumns reads the sessions column names out of the
// coreDDL string literal.
func coreDDLSessionsColumns(t *testing.T, file *ast.File) []string {
	t.Helper()
	var ddl string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "coreDDL" {
			return true
		}
		for _, value := range spec.Values {
			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			require.NoError(t, err, "unquoting coreDDL")
			ddl = unquoted
		}
		return false
	})
	require.NotEmpty(t, ddl, "coreDDL is no longer a string literal in schema.go")

	const header = "CREATE TABLE IF NOT EXISTS sessions ("
	_, rest, ok := strings.Cut(ddl, header)
	require.True(t, ok, "coreDDL no longer declares the sessions table")
	body, _, ok := strings.Cut(rest, "\n);")
	require.True(t, ok, "coreDDL's sessions table has no closing paren")

	var columns []string
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		switch strings.ToUpper(name) {
		case "PRIMARY", "UNIQUE", "CHECK", "CONSTRAINT", "FOREIGN":
			// Table-level constraints, not columns.
			continue
		}
		columns = append(columns, name)
	}
	return columns
}

// migrationSessionsColumns reads the sessions entries out of every
// []columnMigration literal in schema.go. Walking the AST rather than
// matching text keeps this working if the list moves or is split.
func migrationSessionsColumns(t *testing.T, file *ast.File) []string {
	t.Helper()
	var columns []string
	lists := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		array, ok := lit.Type.(*ast.ArrayType)
		if !ok {
			return true
		}
		elt, ok := array.Elt.(*ast.Ident)
		if !ok || elt.Name != "columnMigration" {
			return true
		}
		lists++
		for _, entry := range lit.Elts {
			fields, ok := entry.(*ast.CompositeLit)
			if !ok || len(fields.Elts) < 2 {
				continue
			}
			table, ok := stringLiteral(fields.Elts[0])
			if !ok || table != "sessions" {
				continue
			}
			column, ok := stringLiteral(fields.Elts[1])
			if !ok {
				continue
			}
			columns = append(columns, column)
		}
		return true
	})
	require.NotZero(t, lists,
		"no []columnMigration literal found in schema.go; if the "+
			"migrations moved, teach this test where to look")
	return columns
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func parseSchemaSource(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(
		token.NewFileSet(), "schema.go", nil, parser.SkipObjectResolution,
	)
	require.NoError(t, err, "parsing schema.go")
	return file
}

// sqliteSessionsColumns reads the archive's own column list, so the
// expectation follows the real schema rather than a hand-kept list.
func sqliteSessionsColumns(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	archive, err := localdb.Open(path)
	require.NoError(t, err)
	path = archive.Path()
	require.NoError(t, archive.Close())

	conn, err := sql.Open("agentsview_archive_sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	rows, err := conn.Query("PRAGMA table_info(sessions)")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var columns []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, columns, "PRAGMA table_info(sessions) returned nothing")
	return columns
}
