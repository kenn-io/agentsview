package server

import (
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const activityReportTokenVersion = 1

type activityReportTokenQuery struct {
	Timezone      string              `json:"tz"`
	RangeStart    time.Time           `json:"start"`
	RangeEnd      time.Time           `json:"end"`
	EffectiveEnd  time.Time           `json:"effective"`
	Partial       bool                `json:"partial"`
	BucketUnit    activity.BucketUnit `json:"bucket_unit"`
	BucketSeconds int                 `json:"bucket_seconds"`
	GapCapSeconds float64             `json:"gap_cap_seconds"`
}

type activityReportTokenFilter struct {
	Project            string `json:"project,omitempty"`
	GitBranch          string `json:"git_branch,omitempty"`
	Agent              string `json:"agent,omitempty"`
	Machine            string `json:"machine,omitempty"`
	ExcludeAutomated   bool   `json:"exclude_automated,omitempty"`
	ExcludeInteractive bool   `json:"exclude_interactive,omitempty"`
	Timezone           string `json:"timezone"`
}

type activityReportTokenPayload struct {
	Version int                       `json:"v"`
	Schema  int                       `json:"schema"`
	Digest  string                    `json:"digest"`
	Query   activityReportTokenQuery  `json:"query"`
	Filter  activityReportTokenFilter `json:"filter"`
	Probe   activity.SourceProbe      `json:"probe"`
}

type activitySessionCursorPayload struct {
	Version   int                  `json:"v"`
	Schema    int                  `json:"schema"`
	Digest    string               `json:"digest"`
	Offset    int                  `json:"offset"`
	Sort      activity.SessionSort `json:"sort"`
	Direction string               `json:"direction"`
	Bucket    *int                 `json:"bucket,omitempty"`
}

func newActivityReportTokenPayload(
	selection resolvedActivitySelection, digest string, probe ...activity.SourceProbe,
) activityReportTokenPayload {
	query := selection.query
	filter := selection.filter
	payload := activityReportTokenPayload{
		Version: activityReportTokenVersion,
		Schema:  export.ActivityReportSchemaVersion,
		Digest:  digest,
		Query: activityReportTokenQuery{
			Timezone: query.Timezone, RangeStart: query.RangeStart,
			RangeEnd: query.RangeEnd, EffectiveEnd: query.EffectiveEnd,
			Partial: query.Partial, BucketUnit: query.Bucket.Unit,
			BucketSeconds: query.Bucket.NominalSeconds,
			GapCapSeconds: query.GapCapSeconds,
		},
		Filter: activityReportTokenFilter{
			Project: filter.Project, GitBranch: filter.GitBranch,
			Agent: filter.Agent, Machine: filter.Machine,
			ExcludeAutomated:   filter.ExcludeAutomated,
			ExcludeInteractive: filter.ExcludeInteractive,
			Timezone:           filter.Timezone,
		},
	}
	if len(probe) > 0 {
		payload.Probe = probe[0]
	}
	return payload
}

func (payload activityReportTokenPayload) selection() (resolvedActivitySelection, error) {
	if payload.Version != activityReportTokenVersion ||
		payload.Schema != export.ActivityReportSchemaVersion {
		return resolvedActivitySelection{}, fmt.Errorf("unsupported activity report token version")
	}
	location, err := time.LoadLocation(payload.Query.Timezone)
	if err != nil {
		return resolvedActivitySelection{}, fmt.Errorf("activity report token timezone: %w", err)
	}
	selection := resolvedActivitySelection{
		query: activity.Query{
			Timezone: payload.Query.Timezone, Loc: location,
			RangeStart: payload.Query.RangeStart, RangeEnd: payload.Query.RangeEnd,
			EffectiveEnd: payload.Query.EffectiveEnd, Partial: payload.Query.Partial,
			Bucket: activity.BucketSpec{
				Unit:           payload.Query.BucketUnit,
				NominalSeconds: payload.Query.BucketSeconds,
			},
			GapCapSeconds: payload.Query.GapCapSeconds,
		},
		filter: db.AnalyticsFilter{
			Project: payload.Filter.Project, GitBranch: payload.Filter.GitBranch,
			Agent: payload.Filter.Agent, Machine: payload.Filter.Machine,
			ExcludeAutomated:   payload.Filter.ExcludeAutomated,
			ExcludeInteractive: payload.Filter.ExcludeInteractive,
			Timezone:           payload.Filter.Timezone,
		},
	}
	if err := activity.ValidateResolvedQuery(selection.query); err != nil {
		return resolvedActivitySelection{}, fmt.Errorf("invalid activity report token query: %w", err)
	}
	if selection.filter.Timezone != selection.query.Timezone {
		return resolvedActivitySelection{}, fmt.Errorf("activity report token timezone mismatch")
	}
	if selection.filter.ExcludeAutomated && selection.filter.ExcludeInteractive {
		return resolvedActivitySelection{}, fmt.Errorf("activity report token excludes all sessions")
	}
	if err := validateActivitySelectionSize(activitySelectionInput{
		Timezone: selection.query.Timezone,
		Project:  selection.filter.Project, GitBranch: selection.filter.GitBranch,
		Agent: selection.filter.Agent, Machine: selection.filter.Machine,
	}); err != nil {
		return resolvedActivitySelection{}, fmt.Errorf("invalid activity report token filter: %w", err)
	}
	return selection, nil
}

func encodeActivityToken(tokenStore db.ActivityReportTokenStore, payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding activity report token payload: %w", err)
	}
	return tokenStore.EncodeActivityReportToken(encoded)
}

func decodeActivityToken[T any](
	tokenStore db.ActivityReportTokenStore, token string,
) (T, error) {
	var payload T
	encoded, err := tokenStore.DecodeActivityReportToken(token)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return payload, fmt.Errorf("decoding activity report token payload: %w", err)
	}
	return payload, nil
}
