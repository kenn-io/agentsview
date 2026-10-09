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
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const chromeNativeHost = chromehost.NativeHost

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
		registered, _ := chromehost.RegisteredManifest(home)
		previousDir := chromehost.RegistrationDataDir(registered)
		folder, err := setupChrome(cfg.DataDir, home, executable, assets, registerChromeHost)
		if err != nil {
			return err
		}
		if previousDir != "" && previousDir != filepath.Dir(filepath.Dir(folder)) {
			fmt.Fprintf(cmd.OutOrStdout(), "Replaced Chrome native host registration for data directory %s\n", previousDir)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Open chrome://extensions, enable Developer mode, and Load unpacked:\n%s\n", folder)
		return nil
	}})
	return cmd
}

func newChromeHostCommand() *cobra.Command {
	var socket string
	cmd := &cobra.Command{Use: "chrome-host", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
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
		var conn net.Conn
		err := safefileio.ValidatePrivateDir(filepath.Dir(socket))
		if errors.Is(err, os.ErrNotExist) {
			goto retry
		}
		if err != nil {
			err = fmt.Errorf("refusing chrome host socket: %w", err)
			fmt.Fprintln(os.Stderr, err)
			return err
		}
		conn, err = (&net.Dialer{}).DialContext(ctx, "unix", socket)
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
	return setupChromeWithWriter(dataDir, home, executable, assets, register, (*os.File).Write)
}

func setupChromeWithWriter(dataDir, home, executable string, assets fs.FS, register func(string) error, write func(*os.File, []byte) (int, error)) (string, error) {
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
	type installFile struct {
		path string
		data []byte
		mode fs.FileMode
	}
	var files []installFile
	folder := filepath.Join(dir, "extension")
	extensionPaths := make(map[string]bool)
	if err := fs.WalkDir(assets, "chrome-extension", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative := strings.TrimPrefix(path, "chrome-extension/")
		if path == "chrome-extension" {
			relative = ""
		}
		target := filepath.Join(folder, relative)
		extensionPaths[target] = true
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		files = append(files, installFile{target, data, 0o600})
		return nil
	}); err != nil {
		return "", fmt.Errorf("extension assets: %w; build the frontend first", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	socket, err := chromehost.SocketPath(dataDir)
	if err != nil {
		return "", err
	}
	launcher := chromehost.LauncherPath(filepath.Dir(dir))
	command := chromehost.BuildLauncher(executable, socket, runtime.GOOS)
	files = append(files, installFile{launcher, []byte(command), 0o700})
	body, err := json.Marshal(map[string]any{"name": chromeNativeHost, "description": "AgentsView Claude.ai Sync", "path": launcher, "type": "stdio", "allowed_origins": []string{"chrome-extension://" + id.String() + "/"}})
	if err != nil {
		return "", err
	}
	path := chromehost.ManifestPath(filepath.Dir(dir), home)
	files = append(files, installFile{path, body, 0o600})
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	var staged []string
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}()
	// Finish every write before replacing any part of the registered install.
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file.path), 0o700); err != nil {
			return "", err
		}
		temp, err := os.CreateTemp(filepath.Dir(file.path), ".chrome-*")
		if err != nil {
			return "", err
		}
		staged = append(staged, temp.Name())
		err = temp.Chmod(file.mode)
		if err == nil {
			var n int
			n, err = write(temp, file.data)
			if err == nil && n != len(file.data) {
				err = io.ErrShortWrite
			}
		}
		if err := errors.Join(err, temp.Close()); err != nil {
			return "", err
		}
	}
	for i, file := range files {
		if err := atomicfile.Replace(staged[i], file.path); err != nil {
			return "", err
		}
	}
	if err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || extensionPaths[path] {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := register(path); err != nil {
		return "", err
	}
	return folder, nil
}
