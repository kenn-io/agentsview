package parser

// gooseDefaultDirs returns the platform data directories that contain the
// Goose sessions.db store. Paths are relative to the user home so
// nonexistent entries for other platforms are skipped during discovery.
func gooseDefaultDirs() []string {
	return []string{
		// macOS and Linux
		".local/share/goose/sessions",
		// Windows
		"AppData/Roaming/Block/goose/sessions",
	}
}
