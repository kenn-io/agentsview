package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
)

// errFrictionDigestNotFound reports a friction_review request for a date
// without a stored digest.
var errFrictionDigestNotFound = errors.New("friction digest not found")

// frictionReviewPageSize is the page size used to read a digest's findings
// and the recent pattern rows.
const frictionReviewPageSize = 1000

// validateFrictionReviewRequest applies friction_review's scope before any
// stream opens: exactly one digest date, no project or session filters, and
// a stored digest for that date.
func (s *Server) validateFrictionReviewRequest(
	ctx context.Context, req generateInsightRequest,
) error {
	if req.DateFrom != req.DateTo {
		return apiError(http.StatusBadRequest,
			"friction_review covers one digest date: date_from must equal date_to")
	}
	if req.Project != "" || req.Filters != nil || req.AutomatedScope != "" || req.Timezone != "" {
		return apiError(http.StatusBadRequest,
			"friction_review does not accept project or session filters")
	}
	digest, err := s.db.GetFrictionDigest(ctx, req.DateFrom)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return handled
		}
		return serverError(err)
	}
	if digest == nil {
		return apiError(http.StatusNotFound, errFrictionDigestNotFound.Error())
	}
	return nil
}

// buildFrictionReviewPayload builds the deterministic friction_review input
// from the stored digest. It only reads friction state. Its signature matches
// buildCannedPayload so generateCannedInsight can dispatch on kind.
func (s *Server) buildFrictionReviewPayload(
	ctx context.Context,
	kind insight.CannedKind,
	req generateInsightRequest,
	_ insight.CannedSessionFilters,
	generationOptions insight.GenerateOptions,
) (insight.CannedAggregatePayload, string, string, error) {
	date := req.DateFrom
	digest, err := s.db.GetFrictionDigest(ctx, date)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("loading friction digest %s: %w", date, err)
	}
	if digest == nil {
		return insight.CannedAggregatePayload{}, "", "", errFrictionDigestNotFound
	}
	var summary map[string]any
	if err := json.Unmarshal(digest.SummaryJSON, &summary); err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("decoding friction summary %s: %w", date, err)
	}
	var p0 struct {
		P0Alerts map[string][]string `json:"p0_alerts"`
	}
	if err := json.Unmarshal(digest.SummaryJSON, &p0); err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("decoding friction p0 alerts %s: %w", date, err)
	}
	findings, err := s.frictionDigestFindings(ctx, date)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	fingerprints := make(map[string]bool, len(findings))
	for _, f := range findings {
		fingerprints[f.Fingerprint] = true
	}
	firstSeen, err := s.frictionFirstSeen(ctx, date, fingerprints)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}

	review := insight.CannedFrictionReviewInput{
		Date:         date,
		Timezone:     digest.Timezone,
		RulesVersion: digest.RulesVersion,
		Revision:     digest.Revision,
		Summary:      summary,
		TopPatterns: insight.RankCannedFrictionPatterns(
			date, findings, firstSeen, insight.MaxCannedFrictionPatterns,
		),
		P0Alerts: insight.CannedFrictionP0Alerts(p0.P0Alerts),
	}
	filters := insight.CannedSessionFilters{
		Timezone:       digest.Timezone,
		IncludeOneShot: true,
		AutomatedScope: "all",
	}
	payload := insight.CannedAggregatePayload{
		Kind:           kind,
		DateFrom:       date,
		DateTo:         date,
		AutomatedScope: filters.AutomatedScope,
		Filters:        filters,
		Focus:          req.Prompt,
		Friction:       &review,
	}
	payload.EvidenceRefs = insight.CannedFrictionEvidenceRefs(review)

	aggregateHash, err := insight.CannedAggregateHash(payload)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	cacheKey, err := insight.CannedCacheKey(
		kind, date, date, "", req.Agent, req.Prompt, aggregateHash,
		filters.AutomatedScope, filters, generationOptions,
	)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	return payload, aggregateHash, cacheKey, nil
}

func (s *Server) frictionDigestFindings(
	ctx context.Context, date string,
) ([]db.FrictionFinding, error) {
	var out []db.FrictionFinding
	cursor := ""
	for {
		page, next, err := s.db.ListFrictionFindings(ctx, db.FrictionFindingFilter{
			Date: date, Limit: frictionReviewPageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing friction findings for %s: %w", date, err)
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// frictionFirstSeen returns first_seen_date for the given fingerprints. Every
// fingerprint in digest D has last_seen_date >= D, so Since: date covers them.
func (s *Server) frictionFirstSeen(
	ctx context.Context, date string, fingerprints map[string]bool,
) (map[string]string, error) {
	out := make(map[string]string, len(fingerprints))
	if len(fingerprints) == 0 {
		return out, nil
	}
	cursor := ""
	for {
		page, next, err := s.db.ListFrictionPatterns(ctx, db.FrictionPatternFilter{
			Since: date, Limit: frictionReviewPageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing friction patterns since %s: %w", date, err)
		}
		for _, p := range page {
			if fingerprints[p.Fingerprint] {
				out[p.Fingerprint] = p.FirstSeenDate
			}
		}
		if next == "" || len(out) == len(fingerprints) {
			return out, nil
		}
		cursor = next
	}
}
