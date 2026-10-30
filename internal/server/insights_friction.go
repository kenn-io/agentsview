package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/insight"
)

// errFrictionDigestNotFound reports a friction_review request for a date
// without a stored digest.
var errFrictionDigestNotFound = errors.New("friction digest not found")

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
	fr, err := s.frictionReader()
	if err != nil {
		return err
	}
	digest, err := fr.GetFrictionDigest(ctx, req.DateFrom)
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
// withoutRecurrence clears first-seen dates for the cache hash. They come
// from shared pattern rows that a later backfill of an earlier day can move,
// so an unchanged digest revision keeps reusing its saved summary.
func withoutRecurrence(payload insight.CannedAggregatePayload) insight.CannedAggregatePayload {
	frozen := *payload.Friction
	frozen.TopPatterns = slices.Clone(frozen.TopPatterns)
	for i := range frozen.TopPatterns {
		frozen.TopPatterns[i].FirstSeenDate = ""
		frozen.TopPatterns[i].Recurring = false
	}
	payload.Friction = &frozen
	return payload
}

func (s *Server) buildFrictionReviewPayload(
	ctx context.Context,
	kind insight.CannedKind,
	req generateInsightRequest,
	_ insight.CannedSessionFilters,
	generationOptions insight.GenerateOptions,
) (insight.CannedAggregatePayload, string, string, error) {
	date := req.DateFrom
	fr, err := s.frictionReader()
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "", err
	}
	digest, err := fr.GetFrictionDigest(ctx, date)
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
	// P0Alerts carries per-tool counts; the session IDs behind them add
	// nothing for the model and can push a CLI generator past its argument
	// limit.
	delete(summary, "p0_alerts")
	var p0 struct {
		P0Alerts map[string][]string `json:"p0_alerts"`
	}
	if err := json.Unmarshal(digest.SummaryJSON, &p0); err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("decoding friction p0 alerts %s: %w", date, err)
	}
	// Rank from the digest's frozen snapshot so the payload, and its cache
	// key, describe that digest even after later sessions add findings.
	snap, err := review.DecodeSnapshot(digest.SnapshotJSON)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("decoding friction snapshot %s: %w", date, err)
	}
	occurrences := make([]insight.CannedFrictionOccurrence, 0, len(snap.Signals))
	for _, sig := range snap.Signals {
		occurrences = append(occurrences, insight.CannedFrictionOccurrence{
			SubjectID: sig.SubjectID, Kind: string(sig.Kind),
			Title: sig.Title(), Fingerprint: sig.Fingerprint(),
		})
	}
	patterns, err := frictionDigestPatterns(ctx, fr, snap.Signals)
	if err != nil {
		return insight.CannedAggregatePayload{}, "", "",
			fmt.Errorf("loading friction patterns for %s: %w", date, err)
	}
	firstSeen := make(map[string]string, len(patterns))
	for _, p := range patterns {
		firstSeen[p.Fingerprint] = p.FirstSeenDate
	}

	input := insight.CannedFrictionReviewInput{
		Date:         date,
		Timezone:     digest.Timezone,
		RulesVersion: digest.RulesVersion,
		Revision:     digest.Revision,
		Summary:      summary,
		TopPatterns: insight.RankCannedFrictionPatterns(
			date, occurrences, firstSeen, insight.MaxCannedFrictionPatterns,
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
		Friction:       &input,
	}
	payload.EvidenceRefs = insight.CannedFrictionEvidenceRefs(input)

	aggregateHash, err := insight.CannedAggregateHash(withoutRecurrence(payload))
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
