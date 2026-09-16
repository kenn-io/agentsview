package rawclient

import (
	"context"
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
	body := apiclient.RawsyncManifest{
		SchemaVersion: int64(manifest.SchemaVersion), Provider: string(manifest.Provider), ConfiguredRootID: manifest.ConfiguredRootID,
		SourceKey: manifest.SourceKey, CaptureID: manifest.CaptureID, CapturedAt: manifest.CapturedAt, Kind: string(manifest.Kind),
	}
	if manifest.ExpectedParentReceipt != "" {
		body.ExpectedParentReceipt = new(manifest.ExpectedParentReceipt)
	}
	for _, entry := range manifest.Entries {
		wire := apiclient.RawsyncEntry{Path: entry.Path, Type: entry.Type, Length: entry.Length, Objects: make([]apiclient.RawsyncObjectRef, 0, len(entry.Objects))}
		if entry.ModTimeNS != 0 {
			wire.ModTimeNs = new(entry.ModTimeNS)
		}
		for _, object := range entry.Objects {
			wire.Objects = append(wire.Objects, apiclient.RawsyncObjectRef{Sha256: object.SHA256, Length: object.Length})
		}
		body.Entries = append(body.Entries, wire)
	}
	resp, err := c.do(ctx, func(api *apiclient.Client) error {
		_, err := api.PostAPIV1RawSyncManifestsWithResponse(ctx, &apiclient.PostAPIV1RawSyncManifestsRequestOptions{Body: &body})
		return err
	})
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	defer resp.Body.Close()
	var wire apiclient.RawSyncManifestResponse
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
