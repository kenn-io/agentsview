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

// Health reads the tenant's report using caller-supplied warning thresholds.
func (c *Client) Health(ctx context.Context, query rawsync.JobHealthQuery) (rawsync.JobHealthReport, error) {
	if err := query.Validate(); err != nil {
		return rawsync.JobHealthReport{}, err
	}
	response, err := c.do(ctx, func(api *apiclient.Client) (*apiclient.GetAPIV1RawSyncHealthResp, error) {
		return api.GetAPIV1RawSyncHealthWithResponse(ctx, &apiclient.GetAPIV1RawSyncHealthRequestOptions{
			Query: &apiclient.GetAPIV1RawSyncHealthQuery{MaxAttempts: query.MaxAttempts, StaleAfterSeconds: query.StaleAfterSeconds},
		})
	})
	if err != nil {
		return rawsync.JobHealthReport{}, err
	}
	if response.StatusCode != http.StatusOK || response.JSON200 == nil {
		return rawsync.JobHealthReport{}, errors.New("rawclient: invalid health response")
	}
	if _, err := statusObject(response.Body, "",
		"observed_at", "max_attempts", "stale_after_seconds",
		"orphaned_manifests", "orphaned_manifest_count", "expired_leases", "expired_lease_count",
		"failed_jobs_by_error_class", "failed_job_count", "retrying_near_limit", "retrying_near_limit_count",
		"stale_source_heads", "stale_source_head_count",
	); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("rawclient: invalid health response: %w", err)
	}
	var report rawsync.JobHealthReport
	if err := json.Unmarshal(response.Body, &report); err != nil {
		return report, fmt.Errorf("rawclient: decode health response: %w", err)
	}
	if report.ObservedAt.IsZero() || report.MaxAttempts != query.MaxAttempts || report.StaleAfterSeconds != query.StaleAfterSeconds {
		return rawsync.JobHealthReport{}, errors.New("rawclient: incomplete or mismatched health response")
	}
	return report, nil
}
