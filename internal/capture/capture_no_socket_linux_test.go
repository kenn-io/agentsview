//go:build linux

package capture

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureStartsNoListeningSocket(t *testing.T) {
	baseline, err := processListeningSockets()
	if err != nil {
		t.Skipf("Linux socket inspection is unavailable: %v", err)
	}
	root := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, runErr := Run(context.Background(), RunOptions{
			Provider: ProviderClaude, OccurrenceID: "no-listener",
			CaptureDir: captureDir, ResultPath: resultPath,
			ProviderRoot: root, WorkDir: workDir,
			Command:     []string{producer, "-p", "prompt"},
			Environment: helperEnvironment(root, "claude-final", 0),
			Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:      testLimits(), CustomPricing: testPricing(),
		})
		done <- runErr
	}()

	opened := make(map[string]struct{})
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case runErr := <-done:
			require.NoError(t, runErr)
			assert.Empty(t, opened,
				"one-shot capture must not start a TCP or Unix listener")
			return
		case <-ticker.C:
			current, inspectErr := processListeningSockets()
			require.NoError(t, inspectErr)
			for socket := range current {
				if _, existed := baseline[socket]; !existed {
					opened[socket] = struct{}{}
				}
			}
		}
	}
}

func processListeningSockets() (map[string]struct{}, error) {
	owned := make(map[string]struct{})
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		target, readErr := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			owned[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
		}
	}

	listening := make(map[string]struct{})
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil, openErr
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) > 9 && fields[3] == "0A" {
				listening[fields[9]] = struct{}{}
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			file.Close()
			return nil, scanErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, closeErr
		}
	}
	unix, err := os.Open("/proc/net/unix")
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(unix)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 6 && fields[3] == "00010000" && fields[4] == "0001" {
			listening[fields[6]] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		unix.Close()
		return nil, err
	}
	if err := unix.Close(); err != nil {
		return nil, err
	}

	for socket := range owned {
		if _, ok := listening[socket]; !ok {
			delete(owned, socket)
		}
	}
	return owned, nil
}
