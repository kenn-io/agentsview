package chromehost

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		return "@echo off\r\n\"" + strings.ReplaceAll(executable, "%", "%%") + "\" chrome-host --socket \"" + strings.ReplaceAll(socket, "%", "%%") + "\"\r\n"
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	return "#!/bin/sh\nexec " + quote(executable) + " chrome-host --socket " + quote(socket) + "\n"
}

func SocketPath(dataDir string) (string, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	socket := filepath.Join(dir, "chrome", "host.sock")
	// macOS has the smallest supported sockaddr_un path, at 104 bytes including NUL.
	if len(socket) >= 104 {
		root, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(dir))
		socket = filepath.Join(root, fmt.Sprintf("av-chrome-%x", digest[:8]), "host.sock")
		if len(socket) >= 104 {
			return "", errors.New("chrome socket path exceeds the Unix socket path limit")
		}
	}
	return socket, nil
}
