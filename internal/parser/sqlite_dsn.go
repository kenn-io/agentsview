package parser

import "strings"

// sqliteURIPath escapes a filesystem path for use in a SQLite file: URI.
// SQLite percent-decodes URI paths and stops parsing them at '?' or '#',
// and go-sqlite3 splits its driver parameters at the first '?', so these
// characters in a real path would open the wrong file or silently drop
// options such as mode=ro.
func sqliteURIPath(path string) string {
	return strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
	).Replace(path)
}
