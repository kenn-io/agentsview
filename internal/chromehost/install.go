package chromehost

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/safefileio"
)

const NativeHost = "io.kenn.agentsview"

func ConfigRoot(home string) string {
	if root := os.Getenv("CHROME_CONFIG_HOME"); root != "" {
		return filepath.Join(root, "google-chrome")
	}
	if root := os.Getenv("XDG_CONFIG_HOME"); root != "" {
		return filepath.Join(root, "google-chrome")
	}
	return filepath.Join(home, ".config", "google-chrome")
}

func ManifestPath(dataDir, home string) string {
	dir := filepath.Join(dataDir, "chrome")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")
	}
	if runtime.GOOS == "linux" {
		dir = filepath.Join(ConfigRoot(home), "NativeMessagingHosts")
	}
	return filepath.Join(dir, NativeHost+".json")
}

func LauncherPath(dataDir string) string {
	path := filepath.Join(dataDir, "chrome", "host")
	if runtime.GOOS == "windows" {
		path += ".cmd"
	}
	return path
}

func BuildLauncher(executable, socket, platform string) string {
	if platform == "windows" {
		return "@echo off\r\n" + launcherArg(executable, platform) + launcherSocketArg(socket, platform) + "\r\n"
	}
	return "#!/bin/sh\nexec " + launcherArg(executable, platform) + launcherSocketArg(socket, platform) + "\n"
}

// LauncherTargets reports whether a launcher written by BuildLauncher relays
// to socket. Setup embeds the socket path, so a launcher from an older setup
// can point at a path the server no longer uses.
func LauncherTargets(launcher, socket, platform string) bool {
	return strings.Contains(launcher, launcherSocketArg(socket, platform))
}

func launcherSocketArg(socket, platform string) string {
	return " chrome-host --socket " + launcherArg(socket, platform)
}

func launcherArg(s, platform string) string {
	if platform == "windows" {
		return "\"" + strings.ReplaceAll(s, "%", "%%") + "\""
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// maxSocketPath is macOS's sockaddr_un path limit, the smallest supported,
// at 104 bytes including NUL.
const maxSocketPath = 104

// SocketPath returns <dataDir>/chrome/host.sock. When that path is too long
// for a Unix socket, it returns av-chrome-<hash>/host.sock under the first
// usable fallback root: $XDG_RUNTIME_DIR, then the temp directory. On Unix the
// root must be private to the current user, so a shared /tmp never qualifies.
// Windows skips that check: a per-user %TEMP% inherits its access list and
// never passes it, and ServeChromeHost still locks down av-chrome-<hash>.
func SocketPath(dataDir string) (string, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	socket := filepath.Join(dir, "chrome", "host.sock")
	if len(socket) < maxSocketPath {
		return socket, nil
	}
	digest := sha256.Sum256([]byte(dir))
	name := fmt.Sprintf("av-chrome-%x", digest[:8])
	var rejected []string
	for _, root := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir()} {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		fallback := filepath.Join(root, name, "host.sock")
		if len(fallback) >= maxSocketPath {
			rejected = append(rejected, root+" is too long for a socket path")
			continue
		}
		if runtime.GOOS != "windows" {
			if err := safefileio.ValidatePrivateDir(root); err != nil {
				rejected = append(rejected, err.Error())
				continue
			}
		}
		return fallback, nil
	}
	advice := "Use a shorter data directory"
	if runtime.GOOS != "windows" {
		advice += ", or set XDG_RUNTIME_DIR to a directory you own with mode 0700"
	}
	return "", fmt.Errorf(
		"the data directory path is too long for the Chrome socket %s. %s. "+
			"Fallback directories that did not qualify: %s",
		socket, advice, strings.Join(rejected, "; "),
	)
}
