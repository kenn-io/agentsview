package rawclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

// Status reads the authenticated tenant's hosted raw-sync status.
func (c *Client) Status(ctx context.Context) (rawsync.Status, error) {
	response, err := c.do(ctx, func(
		api *apiclient.Client,
	) (*apiclient.GetAPIV1RawSyncStatusResp, error) {
		return api.GetAPIV1RawSyncStatusWithResponse(
			ctx, &apiclient.GetAPIV1RawSyncStatusRequestOptions{},
		)
	})
	if err != nil {
		return rawsync.Status{}, err
	}
	if response.StatusCode != http.StatusOK || response.JSON200 == nil {
		return rawsync.Status{}, errors.New("rawclient: invalid status response")
	}
	var status *rawsync.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return rawsync.Status{}, fmt.Errorf("rawclient: decode status response: %w", err)
	}
	if status == nil {
		return rawsync.Status{}, errors.New("rawclient: status response is null")
	}
	return *status, nil
}
