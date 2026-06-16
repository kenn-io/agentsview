package parser

// coworkDefaultDirs returns platform-specific default directories that
// hold Claude Desktop "cowork" (local agent mode) session data. The
// Claude desktop app is an Electron app, so its user-data directory
// follows the standard Electron convention per platform:
//
//	macOS:   ~/Library/Application Support/Claude
//	Linux:   ~/.config/Claude
//	Windows: %LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\Claude
//	         or %APPDATA%\Claude on non-MSIX/older installs
//
// Cowork sessions live under <userData>/local-agent-mode-sessions.
// Paths that don't exist on a given platform are skipped silently by
// discovery, so listing all three unconditionally is safe. In Docker
// the host directory is mounted and COWORK_DIR points at the mount.
func coworkDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Application Support/Claude/local-agent-mode-sessions",
		// Linux
		".config/Claude/local-agent-mode-sessions",
		// Windows MSIX package install
		"AppData/Local/Packages/Claude_pzs8sxrjxfjjc/" +
			"LocalCache/Roaming/Claude/local-agent-mode-sessions",
		// Windows non-MSIX / older installs
		"AppData/Roaming/Claude/local-agent-mode-sessions",
	}
}
