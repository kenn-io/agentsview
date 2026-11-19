package kata

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	locateTimeout   = 5 * time.Second
	locateWaitDelay = 250 * time.Millisecond
)

// locateCommand uses Kata's supported local discovery contract. It can start
// a stopped daemon; argv is fixed and no shell is involved.
var locateCommand = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kata", "daemon", "locate", "--json")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = locateWaitDelay
	return cmd.Output()
}

type locateResult struct {
	Network        string `json:"network"`
	Address        string `json:"address"`
	RequestBaseURL string `json:"request_base_url"`
}

func resolveEndpoint(ctx context.Context, configured string) (string, error) {
	if endpoint := strings.TrimSpace(configured); endpoint != "" {
		return endpoint, nil
	}
	// Reserve pipe cleanup time inside the discovery budget.
	locateCtx, cancel := context.WithTimeout(ctx, locateTimeout-locateWaitDelay)
	defer cancel()
	out, err := locateCommand(locateCtx)
	if ctxErr := locateCtx.Err(); ctxErr != nil {
		return "", fmt.Errorf("kata daemon locate failed: %w", ctxErr)
	}
	// A daemon child may keep stdout open after locate exits. WaitDelay bounds
	// that wait; complete output is still usable after it expires.
	if err != nil && (!errors.Is(err, exec.ErrWaitDelay) || len(out) == 0) {
		return "", fmt.Errorf("kata daemon locate failed: %w", err)
	}
	var result locateResult
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("kata daemon locate: %w", ErrInvalidResponse)
	}
	switch result.Network {
	case "unix":
		if !strings.HasPrefix(result.Address, "unix:///") {
			return "", fmt.Errorf("kata daemon locate: %w: invalid unix address", ErrInvalidResponse)
		}
		return result.Address, nil
	case "tcp":
		if result.RequestBaseURL == "" {
			return "", fmt.Errorf("kata daemon locate: %w: request_base_url missing", ErrInvalidResponse)
		}
		return result.RequestBaseURL, nil
	default:
		return "", errors.New("kata daemon locate: unsupported network")
	}
}
