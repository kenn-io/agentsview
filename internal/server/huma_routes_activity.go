package server

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const (
	activityReportFilterValueMaxBytes = 1024
	activityReportFilterTotalMaxBytes = 3072
	activityReportTimezoneMaxBytes    = 128
)

func (s *Server) registerActivityRoutes() {
	group := newRouteGroup(s.api, "/api/v1/activity", "Activity")
	s.stream(group, http.MethodGet, "/report", "Get activity report",
		s.humaActivityReport, streamJSONResponseSchema("ActivityReport"))
	s.getLong(group, "/report/{report_id}/sessions",
		"Page activity report sessions", s.humaActivityReportSessions)
}

type activityReportInput struct {
	Preset    string `query:"preset" enum:"day,week,month,custom" doc:"Range preset"`
	Date      string `query:"date" format:"date" doc:"Calendar day (YYYY-MM-DD) for presets"`
	From      string `query:"from" doc:"Range start (RFC3339) for custom ranges"`
	To        string `query:"to" doc:"Range end (RFC3339) for custom ranges"`
	Timezone  string `query:"timezone" doc:"IANA timezone name"`
	Bucket    string `query:"bucket" enum:"5m,15m,1h,1d,1w" doc:"Timeline bucket size override"`
	Project   string `query:"project" doc:"Filter by project"`
	GitBranch string `query:"git_branch" doc:"Filter by git branch; opaque (project, branch) tokens from the /branches endpoint"`
	Agent     string `query:"agent" doc:"Filter by agent"`
	Machine   string `query:"machine" doc:"Filter by machine"`
	// Automation classes the report: "all" (default) keeps both, "interactive"
	// drops automated sessions, "automated" drops interactive ones. Empty is
	// treated as "all"; any other value is rejected.
	Automation string `query:"automation" default:"all" doc:"Automation class: all, interactive, or automated"`
}

type activitySelectionInput struct {
	Preset, Date, From, To, Timezone, Bucket string
	Project, GitBranch, Agent, Machine       string
	Automation                               string
}

type resolvedActivitySelection struct {
	query  activity.Query
	filter db.AnalyticsFilter
}

type activityReportBuildInputs struct {
	artifactStore db.ActivityReportArtifactStore
	tokenStore    db.ActivityReportTokenStore
	probe         activity.SourceProbe
}

func (s *Server) humaActivityReport(
	ctx context.Context, in *activityReportInput,
) (*huma.StreamResponse, error) {
	selection, err := resolveActivitySelection(activitySelectionInput{
		Preset: in.Preset, Date: in.Date, From: in.From, To: in.To,
		Timezone: in.Timezone, Bucket: in.Bucket, Project: in.Project,
		GitBranch: in.GitBranch, Agent: in.Agent, Machine: in.Machine,
		Automation: in.Automation,
	}, time.Now())
	if err != nil {
		return nil, err
	}
	buildInputs, err := s.resolveActivityReportBuildInputs(ctx, selection)
	if err != nil {
		var responseErr *apiErrorResponse
		if errors.As(err, &responseErr) && responseErr.Status >= 400 &&
			responseErr.Status < 500 {
			return nil, err
		}
		return nil, internalError("activity report setup", err)
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		var sse *SSEStream
		streaming := strings.Contains(hctx.Header("Accept"), "text/event-stream")
		if streaming {
			var ok bool
			sse, ok = newHumaSSEStream(hctx)
			if !ok {
				writeHumaJSON(hctx, http.StatusInternalServerError,
					apiErrorResponse{Message: "streaming not supported"})
				return
			}
		}
		var onProgress activity.ProgressFunc
		if streaming {
			sendProgress := newPushProgressStreamSender(func(progress activity.Progress) {
				sse.SendJSON("progress", progress)
			})
			onProgress = sendProgress
		}
		report, buildErr := s.buildActivityReport(
			ctx, selection, buildInputs, onProgress,
		)
		if buildErr != nil {
			publicErr := internalError("activity report build", buildErr)
			if publicErr == nil {
				return
			}
			if streaming {
				sse.SendJSON("error", map[string]string{"error": publicErr.Error()})
				return
			}
			status := http.StatusInternalServerError
			if responseErr, ok := errors.AsType[*apiErrorResponse](publicErr); ok {
				status = responseErr.Status
			}
			writeHumaJSON(hctx, status,
				apiErrorResponse{Message: publicErr.Error()})
			return
		}
		if streaming {
			sse.SendJSON("report", report)
			return
		}
		writeHumaJSON(hctx, http.StatusOK, report)
	}}, nil
}

func (s *Server) buildActivityReport(
	ctx context.Context,
	selection resolvedActivitySelection,
	inputs *activityReportBuildInputs,
	onProgress activity.ProgressFunc,
) (activity.Report, error) {
	if inputs == nil {
		return s.db.GetActivityReport(ctx, selection.filter, selection.query)
	}
	artifacts, err := s.buildActivityArtifacts(
		ctx, inputs.artifactStore, selection, inputs.probe, onProgress,
	)
	if err != nil {
		return activity.Report{}, err
	}
	return s.prepareActivityReport(
		selection, inputs.tokenStore, artifacts, inputs.probe,
	)
}

func (s *Server) resolveActivityReportBuildInputs(
	ctx context.Context, selection resolvedActivitySelection,
) (*activityReportBuildInputs, error) {
	artifactStore, artifactsOK := s.db.(db.ActivityReportArtifactStore)
	tokenStore, tokenOK := s.db.(db.ActivityReportTokenStore)
	if !artifactsOK || !tokenOK {
		return nil, nil
	}
	probe, err := s.activitySourceProbe(ctx)
	if err != nil {
		return nil, err
	}
	if err := preflightActivityReportToken(tokenStore, selection, probe); err != nil {
		if errors.Is(err, db.ErrActivityReportTokenTooLong) {
			return nil, apiError(http.StatusBadRequest,
				"activity filters produce an oversized report ID")
		}
		return nil, err
	}
	return &activityReportBuildInputs{
		artifactStore: artifactStore, tokenStore: tokenStore, probe: probe,
	}, nil
}

func preflightActivityReportToken(
	tokenStore db.ActivityReportTokenStore,
	selection resolvedActivitySelection,
	probe activity.SourceProbe,
) error {
	_, err := encodeActivityToken(tokenStore, newActivityReportTokenPayload(
		selection, strings.Repeat("0", sha256.Size*2), probe,
	))
	return err
}

func (s *Server) activitySourceProbe(ctx context.Context) (activity.SourceProbe, error) {
	probeStore, ok := s.db.(db.ActivityReportProbeStore)
	if !ok {
		return activity.SourceProbe{}, nil
	}
	return probeStore.ActivityReportSourceProbe(ctx)
}

func (s *Server) buildActivityArtifacts(
	ctx context.Context,
	artifactStore db.ActivityReportArtifactStore,
	selection resolvedActivitySelection,
	probe activity.SourceProbe,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	build := func(buildCtx context.Context) (activity.CandidateArtifacts, error) {
		return artifactStore.BuildActivityReportArtifacts(
			buildCtx, selection.filter, selection.query, onProgress,
		)
	}
	// Progress callbacks are bound to one HTTP response. Do not put them in a
	// shared flight where the initiating client can disconnect while another
	// waiter remains, leaving the shared build writing to a stale response.
	if onProgress != nil {
		return build(ctx)
	}
	keyPayload, err := json.Marshal(
		newActivityReportTokenPayload(selection, "", probe),
	)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	if s.activityReportFlights == nil {
		s.activityReportFlights = newActivityReportBuildGroup()
	}
	return s.activityReportFlights.do(ctx, string(keyPayload), build)
}

func (s *Server) prepareActivityReport(
	selection resolvedActivitySelection,
	tokenStore db.ActivityReportTokenStore,
	artifacts activity.CandidateArtifacts,
	probe activity.SourceProbe,
) (activity.Report, error) {
	digest, err := activity.ArtifactDigest(artifacts)
	if err != nil {
		return activity.Report{}, err
	}
	reportID, err := encodeActivityToken(
		tokenStore, newActivityReportTokenPayload(selection, digest, probe),
	)
	if err != nil {
		return activity.Report{}, err
	}
	page, err := activity.PageSessions(
		artifacts.Sessions, artifacts.Membership, activity.SessionPageOptions{},
	)
	if err != nil {
		return activity.Report{}, err
	}
	if page.HasNext {
		page.NextCursor, err = encodeActivityToken(tokenStore, activitySessionCursorPayload{
			Version: activitySessionCursorVersion,
			Schema:  export.ActivityReportSchemaVersion,
			Digest:  digest, Offset: page.Next,
			Sort: activity.SessionSortAgentMinutes, Direction: "desc",
		})
		if err != nil {
			return activity.Report{}, err
		}
	}
	report := artifacts.Report
	report.ReportID = reportID
	report.BySession = page.Sessions
	report.SessionsNextCursor = page.NextCursor
	report.SessionsTotal = page.Total
	if s.activityReports == nil {
		s.activityReports = newActivityReportCache()
	}
	s.activityReports.put(reportID, digest, artifacts)
	return report, nil
}

func resolveActivitySelection(
	in activitySelectionInput,
	now time.Time,
) (resolvedActivitySelection, error) {
	if err := validateActivitySelectionSize(in); err != nil {
		return resolvedActivitySelection{}, apiError(http.StatusBadRequest, err.Error())
	}
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return resolvedActivitySelection{},
			apiError(http.StatusBadRequest, "invalid timezone: "+tz)
	}
	input := activity.QueryInput{
		Preset: in.Preset, Date: in.Date, From: in.From, To: in.To,
		Timezone: tz, BucketOverride: in.Bucket,
	}
	// Presets need an anchor date; default to today in the requested
	// timezone, matching the prior day-only handler's behavior.
	if input.Date == "" && input.From == "" {
		input.Date = now.In(loc).Format("2006-01-02")
	}
	q, err := activity.ResolveQuery(input, now)
	if err != nil {
		return resolvedActivitySelection{}, apiError(http.StatusBadRequest, err.Error())
	}
	excludeAutomated, excludeInteractive, err := activityAutomationFilter(in.Automation)
	if err != nil {
		return resolvedActivitySelection{}, apiError(http.StatusBadRequest, err.Error())
	}
	// The activity report intentionally includes one-shot sessions, unlike
	// analytics which excludes them by default. The automation class is the
	// caller's choice (default "all" keeps both automated and interactive).
	f := db.AnalyticsFilter{
		Timezone: tz, Project: in.Project, GitBranch: in.GitBranch,
		Agent: in.Agent, Machine: in.Machine,
		ExcludeOneShot:     false,
		ExcludeAutomated:   excludeAutomated,
		ExcludeInteractive: excludeInteractive,
	}
	return resolvedActivitySelection{query: q, filter: f}, nil
}

func validateActivitySelectionSize(in activitySelectionInput) error {
	if len(in.Timezone) > activityReportTimezoneMaxBytes {
		return fmt.Errorf("activity timezone exceeds %d bytes",
			activityReportTimezoneMaxBytes)
	}
	values := []struct {
		name  string
		value string
	}{
		{"project", in.Project},
		{"git_branch", in.GitBranch},
		{"agent", in.Agent},
		{"machine", in.Machine},
	}
	total := 0
	for _, field := range values {
		if len(field.value) > activityReportFilterValueMaxBytes {
			return fmt.Errorf("activity %s filter exceeds %d bytes",
				field.name, activityReportFilterValueMaxBytes)
		}
		total += len(field.value)
	}
	if total > activityReportFilterTotalMaxBytes {
		return fmt.Errorf("activity filters exceed %d bytes total",
			activityReportFilterTotalMaxBytes)
	}
	return nil
}

// activityAutomationFilter maps the activity report's automation query value to
// the AnalyticsFilter class exclusions. Empty and "all" keep both classes;
// "interactive" drops automated sessions; "automated" drops interactive ones.
// Any other value is an error so a typo surfaces as 400 rather than silently
// returning the unfiltered report.
func activityAutomationFilter(
	automation string,
) (excludeAutomated, excludeInteractive bool, err error) {
	switch automation {
	case "", "all":
		return false, false, nil
	case "interactive":
		return true, false, nil
	case "automated":
		return false, true, nil
	default:
		return false, false, fmt.Errorf(
			"invalid automation %q (want all, interactive, or automated)",
			automation)
	}
}
