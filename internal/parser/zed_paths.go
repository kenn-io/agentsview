package parser

// zedDefaultDirs returns platform-specific default data directories
// that contain threads/threads.db.
func zedDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Application Support/Zed",
		// Linux
		".local/share/zed",
		// Windows
		"AppData/Local/Zed",
	}
}
