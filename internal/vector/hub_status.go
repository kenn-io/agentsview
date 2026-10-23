package vector

import (
	"context"
	"encoding/json/v2"
)

type HubPullStatus struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	Unmatched   int64  `json:"unmatched"`
	Reason      string `json:"reason,omitempty"`
}

func (ix *Index) SetHubPullStatus(ctx context.Context, status HubPullStatus) error {
	if err := ix.requireWritable(); err != nil {
		return err
	}
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return ix.metaSet(ctx, "hub_pull_status", string(raw))
}

func (ix *Index) HubPullStatus(ctx context.Context) (*HubPullStatus, error) {
	raw, ok, err := ix.metaGet(ctx, "hub_pull_status")
	if err != nil || !ok {
		return nil, err
	}
	var status HubPullStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return nil, err
	}
	return &status, nil
}
