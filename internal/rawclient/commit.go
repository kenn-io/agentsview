package rawclient

import (
	"context"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/rawsync"
)

// commitResultWire mirrors the manifest-commit response body. rawsync's
// CommitResult is the custody-domain type and carries no JSON tags, so the
// four wire fields decode here and convert.
type commitResultWire struct {
	ManifestID string `json:"manifest_id"`
	Receipt    string `json:"receipt"`
	Generation int64  `json:"generation"`
	Created    bool   `json:"created"`
}

// CommitManifest submits one complete manifest and returns its durable
// receipt. Only this response authorizes checkpoint advancement. Head
// conflicts and missing objects surface as typed APIError values so callers
// can distinguish re-capture from retry.
func (c *Client) CommitManifest(
	ctx context.Context,
	manifest rawsync.Manifest,
) (rawsync.CommitResult, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/raw-sync/manifests", nil, manifest)
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	defer resp.Body.Close()
	var wire commitResultWire
	if err := jsonDecode(resp.Body, &wire); err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("rawclient: decode commit result: %w", err)
	}
	if wire.Receipt == "" {
		return rawsync.CommitResult{}, fmt.Errorf("rawclient: commit response missing receipt")
	}
	result := rawsync.CommitResult{
		ManifestID: wire.ManifestID,
		Receipt:    wire.Receipt,
		Generation: wire.Generation,
		Created:    wire.Created,
	}
	if err := rawsync.ValidateCommitResult(result); err != nil {
		return rawsync.CommitResult{}, fmt.Errorf("rawclient: invalid commit response: %w", err)
	}
	return result, nil
}
