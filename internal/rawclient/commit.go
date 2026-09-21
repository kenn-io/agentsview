package rawclient

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

// CommitManifest submits one complete manifest and returns its durable
// receipt. Only this response authorizes checkpoint advancement. Head
// conflicts and missing objects surface as typed APIError values so callers
// can distinguish re-capture from retry.
func (c *Client) CommitManifest(
	ctx context.Context,
	manifest rawsync.Manifest,
) (rawsync.CommitResult, error) {
	response, err := c.do(ctx, func(api *apiclient.Client) (*apiclient.PostAPIV1RawSyncManifestsResp, error) {
		return api.PostAPIV1RawSyncManifestsWithResponse(ctx, &apiclient.PostAPIV1RawSyncManifestsRequestOptions{Body: &manifest})
	})
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	wire := response.JSON200
	if wire.Receipt == "" {
		return rawsync.CommitResult{}, errors.New("rawclient: commit response missing receipt")
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
