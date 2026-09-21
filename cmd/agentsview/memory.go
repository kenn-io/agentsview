// ABOUTME: Fast lifecycle entry points used by conversation-memory packages.
// ABOUTME: SessionStart only queues daemon work and never scans the archive.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
)

const (
	memorySessionStartTimeout = 1900 * time.Millisecond
	memoryRefreshDebounce     = 250 * time.Millisecond
)

var runMemorySessionStart = requestLocalMemoryRefresh

func newMemoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "memory",
		Short:        "Manage conversation-memory integration",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMemorySessionStartCommand())
	return cmd
}

func newMemorySessionStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "session-start",
		Short: "Queue a local archive refresh for an agent session start",
		Long: "Ensure the configured local daemon is available and queue a " +
			"coalesced background refresh. The command returns within two seconds " +
			"without waiting for archive reconciliation.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv("AGENTSVIEW_DISABLE_AUTO_SYNC") == "1" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"agentsview memory: automatic sync disabled by "+
						"AGENTSVIEW_DISABLE_AUTO_SYNC=1")
				return nil
			}
			ctx, cancel := context.WithTimeout(
				cmd.Context(), memorySessionStartTimeout,
			)
			defer cancel()
			return runMemorySessionStart(ctx)
		},
	}
}

func requestLocalMemoryRefresh(ctx context.Context) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("memory session-start: loading config: %w", err)
	}
	tr, err := ensureTransportContext(
		ctx, &cfg, transportIntentArchiveWrite, memorySessionStartTimeout,
	)
	if err != nil {
		return fmt.Errorf("memory session-start: local daemon: %w", err)
	}
	if tr.Mode != transportHTTP || tr.ReadOnly {
		return errors.New(
			"memory session-start: writable local daemon is unavailable",
		)
	}
	if tr.Runtime != nil && tr.Runtime.NoSync {
		return errors.New(
			"memory session-start: local daemon was started with sync disabled",
		)
	}
	return postMemoryRefresh(ctx, tr.URL, cfg.AuthToken)
}

func postMemoryRefresh(ctx context.Context, baseURL, token string) error {
	url := strings.TrimSuffix(baseURL, "/") + "/api/v1/memory/refresh"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, http.NoBody,
	)
	if err != nil {
		return fmt.Errorf("memory session-start: create refresh request: %w", err)
	}
	req.Header.Set("Origin", daemonOriginURL(baseURL))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: memorySessionStartTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("memory session-start: request refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf(
		"memory session-start: refresh request returned HTTP %d: %s",
		resp.StatusCode, strings.TrimSpace(string(body)),
	)
}

type memoryRefreshQueue struct {
	requests chan struct{}
}

func newMemoryRefreshQueue() *memoryRefreshQueue {
	return &memoryRefreshQueue{requests: make(chan struct{}, 1)}
}

func (q *memoryRefreshQueue) Notify() {
	select {
	case q.requests <- struct{}{}:
	default:
	}
}

func runMemoryRefreshScheduler(
	ctx context.Context,
	requests <-chan struct{},
	debounce time.Duration,
	refresh func(),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-requests:
		}

		timer := time.NewTimer(debounce)
	debounceLoop:
		for {
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-requests:
				// The bounded queue already records the pending refresh. Keep
				// the original deadline so a busy burst cannot postpone forever.
			case <-timer.C:
				break debounceLoop
			}
		}

		refresh()
		// A request arriving while refresh ran is covered by that pass. Drain
		// the single pending signal so parallel SessionStart hooks stay one pass.
		select {
		case <-requests:
		default:
		}
	}
}
