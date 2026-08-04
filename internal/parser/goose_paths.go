package parser

// gooseDefaultDirs returns the platform data directories that contain the
// Goose sessions.db store. Paths are relative to the user home so
// nonexistent entries for other platforms are skipped during discovery.
// Goose resolves them via etcetera's choose_app_strategy: the XDG strategy
// on macOS and Linux, and the Windows strategy — which appends a "data"
// subfolder under the author/app directory — on Windows.
func gooseDefaultDirs() []string {
	return []string{
		// macOS and Linux
		".local/share/goose/sessions",
		// Windows
		"AppData/Roaming/Block/goose/data/sessions",
	}
}
