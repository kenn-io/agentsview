package rawclient

import (
	"context"
	"encoding/json/jsontext"
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
	if err := validateStatusResponse(response.Body); err != nil {
		return rawsync.Status{}, fmt.Errorf("rawclient: invalid status response: %w", err)
	}
	return *status, nil
}

// Typed decoding accepts missing members as zero values. Check presence separately
// so incomplete responses cannot look like a tenant with no work to process.
func validateStatusResponse(body []byte) error {
	root, err := statusObject(body, "", "source_heads", "parse_jobs", "active_device_count", "devices", "uploads")
	if err != nil {
		return err
	}
	if _, err := statusObject(root["parse_jobs"], "", "ready", "leased", "retrying", "complete", "failed", "superseded"); err != nil {
		return err
	}
	uploads, err := statusObject(root["uploads"], "oldest_open_session", "open_count", "pending_bytes", "oldest_open_session")
	if err != nil {
		return err
	}
	if oldest := uploads["oldest_open_session"]; oldest.Kind() != 'n' {
		if _, err := statusObject(oldest, "", "upload_id", "created_at"); err != nil {
			return err
		}
	}
	for _, array := range []struct {
		name     string
		nullable string
		required []string
	}{
		{
			name: "source_heads", nullable: "last_accepted_at",
			// Older status responses may omit last_parse_completed_at.
			required: []string{
				"device_id", "configured_root_id", "provider", "source_key", "generation",
				"last_accepted_at", "parse_pending", "parse_leased", "parse_failed",
			},
		},
		{name: "devices", nullable: "last_seen_at", required: []string{"device_id", "last_seen_at"}},
	} {
		var items []jsontext.Value
		if err := json.Unmarshal(root[array.name], &items); err != nil {
			return err
		}
		for _, item := range items {
			if _, err := statusObject(item, array.nullable, array.required...); err != nil {
				return err
			}
		}
	}
	return nil
}

func statusObject(body jsontext.Value, nullable string, required ...string) (map[string]jsontext.Value, error) {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	for _, name := range required {
		value, ok := object[name]
		if !ok {
			return nil, fmt.Errorf("missing required field %q", name)
		}
		if name != nullable && value.Kind() == 'n' {
			return nil, fmt.Errorf("required field %q is null", name)
		}
	}
	return object, nil
}
