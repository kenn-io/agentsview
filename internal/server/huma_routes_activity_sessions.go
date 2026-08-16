package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

type activityReportSessionsInput struct {
	ReportID      string           `path:"report_id" required:"true" doc:"Signed Activity report ID"`
	Limit         int              `query:"limit" minimum:"0" maximum:"500" doc:"Maximum session rows"`
	Cursor        string           `query:"cursor" doc:"Opaque page cursor"`
	Sort          string           `query:"sort" doc:"Sort: agent_minutes, cost, first_active, project, or agent"`
	Direction     string           `query:"direction" enum:"asc,desc" doc:"Sort direction"`
	Bucket        optionalIntParam `query:"bucket" minimum:"0" doc:"Optional timeline bucket index"`
	IncludeReport bool             `query:"include_report" doc:"Include full report metadata for stateless clients"`
}

type activityReportSessionsResponse struct {
	ReportID        string                `json:"report_id"`
	Sessions        []activity.SessionRow `json:"sessions"`
	NextCursor      string                `json:"next_cursor,omitempty"`
	Total           int                   `json:"total"`
	RefreshRequired bool                  `json:"refresh_required,omitempty"`
	Report          *activity.Report      `json:"report,omitempty"`
}

func (s *Server) humaActivityReportSessions(
	ctx context.Context,
	in *activityReportSessionsInput,
) (*jsonOutput[activityReportSessionsResponse], error) {
	artifactStore, artifactsOK := s.db.(db.ActivityReportArtifactStore)
	tokenStore, tokenOK := s.db.(db.ActivityReportTokenStore)
	if !artifactsOK || !tokenOK {
		return nil, apiError(http.StatusNotImplemented,
			"activity report session paging is not available for this store")
	}
	reportToken, err := decodeActivityToken[activityReportTokenPayload](
		tokenStore, in.ReportID,
	)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, "invalid activity report ID")
	}
	selection, err := reportToken.selection()
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if s.activityReports == nil {
		s.activityReports = newActivityReportCache()
	}
	currentProbe, err := s.activitySourceProbe(ctx)
	if err != nil {
		return nil, internalError("activity report source probe error", err)
	}
	if currentProbe != reportToken.Probe {
		artifacts, buildErr := s.buildActivityArtifacts(
			ctx, artifactStore, selection, currentProbe, nil,
		)
		if buildErr != nil {
			return nil, internalError("activity report refresh rebuild error", buildErr)
		}
		refreshed, prepareErr := s.prepareActivityReport(
			selection, tokenStore, artifacts, currentProbe,
		)
		if prepareErr != nil {
			return nil, internalError("activity report refresh error", prepareErr)
		}
		return &jsonOutput[activityReportSessionsResponse]{Body: activityReportSessionsResponse{
			ReportID: refreshed.ReportID, Sessions: refreshed.BySession,
			NextCursor: refreshed.SessionsNextCursor,
			Total:      refreshed.SessionsTotal, RefreshRequired: true,
			Report: &refreshed,
		}}, nil
	}

	artifacts, digest, cached := s.activityReports.get(in.ReportID)
	if !cached {
		artifacts, err = s.buildActivityArtifacts(
			ctx, artifactStore, selection, currentProbe, nil,
		)
		if err != nil {
			return nil, internalError("activity report page rebuild error", err)
		}
		digest, err = activity.ArtifactDigest(artifacts)
		if err != nil {
			return nil, internalError("activity report page digest error", err)
		}
		if digest != reportToken.Digest {
			refreshed, prepareErr := s.prepareActivityReport(
				selection, tokenStore, artifacts, currentProbe,
			)
			if prepareErr != nil {
				return nil, internalError("activity report refresh error", prepareErr)
			}
			return &jsonOutput[activityReportSessionsResponse]{Body: activityReportSessionsResponse{
				ReportID: refreshed.ReportID, Sessions: refreshed.BySession,
				NextCursor: refreshed.SessionsNextCursor,
				Total:      refreshed.SessionsTotal, RefreshRequired: true,
				Report: &refreshed,
			}}, nil
		}
		s.activityReports.put(in.ReportID, digest, artifacts)
	}
	if digest != reportToken.Digest {
		return nil, apiError(http.StatusConflict, "activity report refresh required")
	}

	options := activity.SessionPageOptions{
		Limit: in.Limit, Sort: activity.SessionSort(in.Sort), Direction: in.Direction,
	}
	if in.Bucket.IsSet {
		bucket := in.Bucket.Value
		if bucket < 0 || bucket >= artifacts.Report.BucketCount {
			return nil, apiError(http.StatusBadRequest, "invalid activity report bucket")
		}
		options.Bucket = &bucket
	}
	options, err = activity.NormalizeSessionPageOptions(options)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if in.Cursor != "" {
		cursor, cursorErr := decodeActivityToken[activitySessionCursorPayload](
			tokenStore, in.Cursor,
		)
		if cursorErr != nil || cursor.Version != activityReportTokenVersion ||
			cursor.Schema != export.ActivityReportSchemaVersion ||
			cursor.Digest != digest || cursor.Sort != options.Sort ||
			cursor.Direction != options.Direction || !sameOptionalBucket(cursor.Bucket, options.Bucket) {
			return nil, apiError(http.StatusBadRequest, "invalid activity session cursor")
		}
		options.Offset = cursor.Offset
	}
	page, pageCached, err := s.activityReports.page(in.ReportID, options)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if !pageCached {
		page, err = activity.PageSessions(artifacts.Sessions, artifacts.Membership, options)
		if err != nil {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
	}
	if page.HasNext {
		page.NextCursor, err = encodeActivityToken(tokenStore, activitySessionCursorPayload{
			Version: activityReportTokenVersion,
			Schema:  export.ActivityReportSchemaVersion,
			Digest:  digest, Offset: page.Next, Sort: options.Sort,
			Direction: options.Direction, Bucket: options.Bucket,
		})
		if err != nil {
			return nil, internalError("activity session cursor error", err)
		}
	}
	response := activityReportSessionsResponse{
		ReportID: in.ReportID, Sessions: page.Sessions,
		NextCursor: page.NextCursor, Total: page.Total,
	}
	if in.IncludeReport {
		pageReport := artifacts.Report
		pageReport.ReportID = in.ReportID
		pageReport.BySession = page.Sessions
		pageReport.SessionsNextCursor = page.NextCursor
		pageReport.SessionsTotal = page.Total
		response.Report = &pageReport
	}
	return &jsonOutput[activityReportSessionsResponse]{Body: response}, nil
}

func sameOptionalBucket(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
