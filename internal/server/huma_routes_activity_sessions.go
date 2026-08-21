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
	Sort          string           `query:"sort" enum:"agent_minutes,cost,first_active,project,agent" doc:"Session sort"`
	Direction     string           `query:"direction" enum:"asc,desc" doc:"Sort direction"`
	BucketStart   optionalIntParam `query:"bucket_start" minimum:"0" doc:"First timeline bucket in the half-open range"`
	BucketEnd     optionalIntParam `query:"bucket_end" minimum:"1" doc:"Exclusive end of the timeline bucket range"`
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
	if in.BucketStart.IsSet != in.BucketEnd.IsSet {
		return nil, apiError(http.StatusBadRequest,
			"activity report bucket range requires both start and end")
	}
	if in.BucketStart.IsSet {
		options.BucketRange = &activity.BucketRange{
			Start: in.BucketStart.Value,
			End:   in.BucketEnd.Value,
		}
	}
	var continuation *activity.SessionPageOptions
	var cursorOffset int
	if in.Cursor != "" {
		cursor, cursorErr := decodeActivityToken[activitySessionCursorPayload](
			tokenStore, in.Cursor,
		)
		if cursorErr != nil || !validActivitySessionCursor(cursor, digest) {
			return nil, apiError(http.StatusBadRequest, "invalid activity session cursor")
		}
		continuation = &activity.SessionPageOptions{
			Sort: cursor.Sort, Direction: cursor.Direction,
			BucketRange: cursor.BucketRange,
		}
		cursorOffset = cursor.Offset
	}
	options, err = activity.ResolveSessionPageOptions(
		options, continuation, activity.SessionPageOptionPresence{
			Sort: in.Sort != "", Direction: in.Direction != "",
			BucketRange: in.BucketStart.IsSet,
		},
	)
	if err != nil {
		if in.Cursor != "" {
			return nil, apiError(http.StatusBadRequest, "invalid activity session cursor")
		}
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if options.BucketRange != nil &&
		(options.BucketRange.Start < 0 ||
			options.BucketRange.End > artifacts.Report.BucketCount) {
		if in.Cursor != "" {
			return nil, apiError(http.StatusBadRequest, "invalid activity session cursor")
		}
		return nil, apiError(http.StatusBadRequest, "invalid activity report bucket")
	}
	options.Offset = cursorOffset
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
			Version: activitySessionCursorVersion,
			Schema:  export.ActivityReportSchemaVersion,
			Digest:  digest, Offset: page.Next, Sort: options.Sort,
			Direction: options.Direction, BucketRange: options.BucketRange,
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
