package parser

// warpDefaultDirs returns the platform-specific default
// directories for the Warp SQLite database. Each path is
// relative to $HOME and should contain warp.sqlite.
func warpDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Group Containers/2BBY89MBSN.dev.warp/Library/Application Support/dev.warp.Warp-Stable",
		// Linux
		".local/state/warp-terminal",
		// Windows
		"AppData/Local/warp/Warp/data",
	}
}
