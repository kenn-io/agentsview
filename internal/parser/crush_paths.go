package parser

// crushDefaultDirs returns the platform data directories that contain the
// Crush project registry. Paths are relative to the user home so
// nonexistent entries for other platforms are skipped during discovery.
// Crush resolves its global data dir via XDG on macOS and Linux and
// %LOCALAPPDATA% on Windows (internal/config/load.go upstream).
func crushDefaultDirs() []string {
	return []string{
		// macOS and Linux
		".local/share/crush",
		// Windows
		"AppData/Local/crush",
	}
}
