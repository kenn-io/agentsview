package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/web"
	"go.kenn.io/kit/safefileio"
)

const chromeNativeHost = "io.kenn.agentsview"

func newChromeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "chrome", Short: "Set up Claude.ai Sync in Chrome", GroupID: groupData}
	cmd.AddCommand(&cobra.Command{Use: "setup", Short: "Install the Chrome native host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadMinimal()
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		assets, err := web.Assets()
		if err != nil {
			return err
		}
		folder, err := setupChrome(cfg.DataDir, home, executable, assets, registerChromeHost)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Open chrome://extensions, enable Developer mode, and Load unpacked:\n%s\n", folder)
		return nil
	}})
	return cmd
}

func newChromeHostCommand() *cobra.Command {
	var socket string
	cmd := &cobra.Command{Use: "chrome-host", Hidden: true, Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return relayChromeHost(cmd.Context(), socket, os.Stdin, os.Stdout)
	}}
	cmd.Flags().StringVar(&socket, "socket", "", "Private server socket")
	_ = cmd.MarkFlagRequired("socket")
	return cmd
}

func relayChromeHost(ctx context.Context, socket string, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := make(chan []byte)
	inputErr := make(chan error, 1)
	go func() {
		for {
			frame, err := chromehost.ReadFrame(input)
			if err != nil {
				inputErr <- err
				cancel()
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err == nil {
			disconnected := make(chan error, 1)
			go func() {
				for {
					frame, err := chromehost.ReadFrame(conn)
					if err == nil {
						err = chromehost.WriteFrame(output, frame)
					}
					if err != nil {
						disconnected <- err
						return
					}
				}
			}()
		connected:
			for {
				select {
				case <-ctx.Done():
					break connected
				case <-disconnected:
					_ = conn.Close()
					goto retry
				case frame := <-frames:
					if err := chromehost.WriteFrame(conn, frame); err != nil {
						break connected
					}
				}
			}
			_ = conn.Close()
			<-disconnected
		}
	retry:
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	select {
	case err := <-inputErr:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	default:
		return ctx.Err()
	}
}

func setupChrome(dataDir, home, executable string, assets fs.FS, register func(string) error) (string, error) {
	manifest, err := fs.ReadFile(assets, "chrome-extension/manifest.json")
	if err != nil {
		return "", fmt.Errorf("extension assets: %w; build the frontend first", err)
	}
	var extension struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(manifest, &extension); err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(extension.Key)
	if err != nil || len(key) == 0 {
		return "", errors.New("extension public key missing or invalid")
	}
	digest := sha256.Sum256(key)
	var id strings.Builder
	for _, b := range digest[:16] {
		id.WriteByte('a' + (b >> 4))
		id.WriteByte('a' + (b & 15))
	}
	dir, err := filepath.Abs(filepath.Join(dataDir, "chrome"))
	if err != nil {
		return "", err
	}
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	folder := filepath.Join(dir, "extension")
	if err := fs.WalkDir(assets, "chrome-extension", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative := strings.TrimPrefix(path, "chrome-extension/")
		if path == "chrome-extension" {
			relative = ""
		}
		target := filepath.Join(folder, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		return "", fmt.Errorf("extension assets: %w; build the frontend first", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	socket, err := chromeSocketPath(dataDir)
	if err != nil {
		return "", err
	}
	launcher := filepath.Join(dir, "host")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := "#!/bin/sh\nexec " + quote(executable) + " chrome-host --socket " + quote(socket) + "\n"
	if runtime.GOOS == "windows" {
		launcher += ".cmd"
		command = "@echo off\r\n\"" + strings.ReplaceAll(executable, "%", "%%") + "\" chrome-host --socket \"" + strings.ReplaceAll(socket, "%", "%%") + "\"\r\n"
	}
	if err := os.WriteFile(launcher, []byte(command), 0700); err != nil {
		return "", err
	}
	manifestDir := dir
	if runtime.GOOS == "darwin" {
		manifestDir = filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")
	}
	if runtime.GOOS == "linux" {
		manifestDir = filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts")
	}
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{"name": chromeNativeHost, "description": "AgentsView Claude.ai Sync", "path": launcher, "type": "stdio", "allowed_origins": []string{"chrome-extension://" + id.String() + "/"}})
	if err != nil {
		return "", err
	}
	path := filepath.Join(manifestDir, chromeNativeHost+".json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		return "", err
	}
	if err := register(path); err != nil {
		return "", err
	}
	return folder, nil
}

func chromeSocketPath(dataDir string) (string, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	socket := filepath.Join(dir, "chrome", "host.sock")
	// macOS has the smallest supported sockaddr_un path, at 104 bytes including NUL.
	if len(socket) >= 104 {
		root := "/tmp"
		if runtime.GOOS == "windows" {
			root = os.TempDir()
		}
		digest := sha256.Sum256([]byte(dir))
		socket = filepath.Join(root, fmt.Sprintf("av-chrome-%x", digest[:8]), "host.sock")
		if len(socket) >= 104 {
			return "", errors.New("Chrome socket path exceeds the Unix socket path limit")
		}
	}
	return socket, nil
}
