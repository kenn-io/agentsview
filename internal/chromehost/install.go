package chromehost

import (
	"crypto/sha256"
	"encoding/json/v2"
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

func ParseLauncher(body, platform string) (executable, socket string, ok bool) {
	var prefix, separator, suffix string
	if platform == "windows" {
		prefix, separator, suffix = "@echo off\r\n\"", "\" chrome-host --socket \"", "\"\r\n"
	} else {
		prefix, separator, suffix = "#!/bin/sh\nexec '", "' chrome-host --socket '", "'\n"
	}
	command, ok := strings.CutPrefix(body, prefix)
	if !ok {
		return "", "", false
	}
	executable, socket, ok = strings.Cut(command, separator)
	if !ok {
		return "", "", false
	}
	socket, ok = strings.CutSuffix(socket, suffix)
	if !ok {
		return "", "", false
	}
	if platform == "windows" {
		executable = strings.ReplaceAll(executable, "%%", "%")
		socket = strings.ReplaceAll(socket, "%%", "%")
	} else {
		executable = strings.ReplaceAll(executable, "'\"'\"'", "'")
		socket = strings.ReplaceAll(socket, "'\"'\"'", "'")
	}
	return executable, socket, executable != "" && socket != "" && BuildLauncher(executable, socket, platform) == body
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

func RegistrationDataDir(manifestPath string) string {
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	var manifest struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(body, &manifest) != nil || !filepath.IsAbs(manifest.Path) {
		return ""
	}
	return filepath.Dir(filepath.Dir(manifest.Path))
}

func Installed(dataDir, home, registered string) bool {
	dir, err := filepath.Abs(dataDir)
	if err != nil || registered != ManifestPath(dir, home) {
		return false
	}
	body, err := os.ReadFile(registered)
	if err != nil {
		return false
	}
	var manifest struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.Path != LauncherPath(dir) {
		return false
	}
	body, err = os.ReadFile(manifest.Path)
	if err != nil {
		return false
	}
	executable, socket, ok := ParseLauncher(string(body), runtime.GOOS)
	expectedSocket, err := SocketPath(dir)
	if !ok || err != nil || socket != expectedSocket {
		return false
	}
	info, err := os.Stat(executable)
	return err == nil && info.Mode().IsRegular()
}
