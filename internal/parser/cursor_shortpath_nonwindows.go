//go:build !windows

package parser

import "path/filepath"

type cursorPathAlias struct {
	tokens []string
	path   string
}

func cursorComponentTokenAliases(
	path string, includeShort bool,
) []cursorPathAlias {
	_ = includeShort // Short 8.3 aliases exist only on Windows.
	return []cursorPathAlias{{
		tokens: cursorComponentTokens(filepath.Base(path)),
		path:   path,
	}}
}
