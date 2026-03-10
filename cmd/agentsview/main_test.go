package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMustLoadConfig(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantHost      string
		wantPort      int
		wantPublicURL string
		wantProxyMode string
		wantNoBrowser bool
	}{
		{
			name:          "DefaultArgs",
			args:          []string{},
			wantHost:      "127.0.0.1",
			wantPort:      8080,
			wantPublicURL: "",
			wantProxyMode: "",
			wantNoBrowser: false,
		},
		{
			name:          "ExplicitFlags",
			args:          []string{"-host", "0.0.0.0", "-port", "9090", "-public-url", "https://viewer.example.test", "-proxy", "caddy", "-proxy-bind-host", "10.0.60.2", "-public-port", "9443", "-no-browser"},
			wantHost:      "0.0.0.0",
			wantPort:      9090,
			wantPublicURL: "https://viewer.example.test:9443",
			wantProxyMode: "caddy",
			wantNoBrowser: true,
		},
		{
			name:          "PartialFlags",
			args:          []string{"-port", "3000"},
			wantHost:      "127.0.0.1",
			wantPort:      3000,
			wantPublicURL: "",
			wantProxyMode: "",
			wantNoBrowser: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_VIEWER_DATA_DIR", t.TempDir())
			cfg := mustLoadConfig(tt.args)

			if cfg.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", cfg.Host, tt.wantHost)
			}
			if cfg.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", cfg.Port, tt.wantPort)
			}
			if cfg.PublicURL != tt.wantPublicURL {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, tt.wantPublicURL)
			}
			if cfg.Proxy.Mode != tt.wantProxyMode {
				t.Errorf("Proxy.Mode = %q, want %q", cfg.Proxy.Mode, tt.wantProxyMode)
			}
			if cfg.NoBrowser != tt.wantNoBrowser {
				t.Errorf("NoBrowser = %v, want %v", cfg.NoBrowser, tt.wantNoBrowser)
			}

			if cfg.DataDir == "" {
				t.Error("DataDir should be set")
			}
			wantDBPath := filepath.Join(cfg.DataDir, "sessions.db")
			if cfg.DBPath != wantDBPath {
				t.Errorf("DBPath = %q, want %q", cfg.DBPath, wantDBPath)
			}
		})
	}
}

func TestSetupLogFile(t *testing.T) {
	origOutput := log.Writer()

	dir := t.TempDir()
	setupLogFile(dir)

	// Close the log file before TempDir cleanup removes the
	// directory. On Windows, open files can't be deleted.
	// Registered after TempDir so LIFO ordering runs this first.
	t.Cleanup(func() {
		if c, ok := log.Writer().(io.Closer); ok {
			c.Close()
		}
		log.SetOutput(origOutput)
	})

	// Log something and verify it reaches the file.
	log.Print("test-log-message")

	logPath := filepath.Join(dir, "debug.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if !strings.Contains(string(data), "test-log-message") {
		t.Errorf(
			"log file missing message, got: %q", data,
		)
	}
}

func TestSetupLogFileOpenFailure(t *testing.T) {
	origOutput := log.Writer()
	t.Cleanup(func() { log.SetOutput(origOutput) })

	// Capture log output to verify warning is emitted.
	var buf bytes.Buffer
	log.SetOutput(io.MultiWriter(origOutput, &buf))

	// Pass a path that can't be opened (dir doesn't exist
	// and we use a file as the "dir").
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	os.WriteFile(tmpFile, []byte("x"), 0o644)

	setupLogFile(tmpFile)

	if !strings.Contains(buf.String(), "cannot open log file") {
		t.Errorf(
			"expected warning about log file, got: %q",
			buf.String(),
		)
	}
}

func TestTruncateLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write a file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	os.WriteFile(path, big, 0o644)

	// Truncate with limit smaller than file size.
	truncateLogFile(path, 512)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after truncate: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size after truncate = %d, want 0", info.Size())
	}
}

func TestTruncateLogFileUnderLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	content := []byte("small log content")
	os.WriteFile(path, content, 0o644)

	// File is under limit: should not be truncated.
	truncateLogFile(path, 1024)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after truncate: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content changed: got %q", data)
	}
}

func TestTruncateLogFileMissing(t *testing.T) {
	// Non-existent file: should not panic.
	missing := filepath.Join(t.TempDir(), "missing", "log.txt")
	truncateLogFile(missing, 1024)
}

func TestTruncateLogFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "link.log")

	// Write a target file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	if err := os.WriteFile(target, big, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, syscall.EPERM) ||
			errors.Is(err, syscall.EACCES) ||
			errors.Is(err, os.ErrPermission) ||
			errors.Is(err, syscall.ENOSYS) ||
			errors.Is(err, syscall.ENOTSUP) {
			t.Skip("symlinks not supported:", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	// Truncate via symlink: should be a no-op.
	truncateLogFile(link, 512)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(data) != 1024 {
		t.Errorf(
			"symlink target was truncated: size=%d, want 1024",
			len(data),
		)
	}
}
