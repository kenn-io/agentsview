package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/search/rrf"
)

const (
	minContentConcepts    = 2
	maxContentConcepts    = 5
	maxConceptSearchLimit = 50
	// maxConceptCandidates bounds each concept's semantic search, so a
	// five-concept request reads at most 5 * 250 units.
	maxConceptCandidates = 250
	conceptRankConstant  = 60
)

// NormalizeContentSearchRequest validates the multi-concept contract and
// returns a copy with trimmed concepts and the semantic mode they require. A
// request without concepts is returned unchanged.
func NormalizeContentSearchRequest(req ContentSearchRequest) (ContentSearchRequest, error) {
	if len(req.Concepts) == 0 {
		return req, nil
	}
	if strings.TrimSpace(req.Pattern) != "" {
		return req, &db.SearchInputError{Msg: "search: pattern and concepts are mutually exclusive"}
	}
	if len(req.Concepts) < minContentConcepts || len(req.Concepts) > maxContentConcepts {
		return req, &db.SearchInputError{Msg: fmt.Sprintf(
			"search: concepts must contain between %d and %d values",
			minContentConcepts, maxContentConcepts)}
	}
	concepts := make([]string, len(req.Concepts))
	for i, concept := range req.Concepts {
		concepts[i] = strings.TrimSpace(concept)
		if concepts[i] == "" {
			return req, &db.SearchInputError{Msg: "search: concepts must not be blank"}
		}
		if slices.Contains(concepts[:i], concepts[i]) {
			return req, &db.SearchInputError{Msg: "search: concepts must be distinct"}
		}
	}
	if req.Mode != "" && req.Mode != "semantic" {
		return req, &db.SearchInputError{Msg: "search: mode must be semantic when concepts are supplied"}
	}
	if req.Cursor != 0 {
		return req, &db.SearchInputError{Msg: "search: cursor is not supported with concepts"}
	}
	if req.Limit < 0 || req.Limit > maxConceptSearchLimit {
		return req, &db.SearchInputError{Msg: fmt.Sprintf(
			"search: limit must be between 1 and %d with concepts", maxConceptSearchLimit)}
	}
	req.Concepts = concepts
	req.Mode = "semantic"
	return req, nil
}

// conceptMember points at one session's best match in one concept's leg.
type conceptMember struct {
	leg, index int
}

// searchContentConcepts runs one semantic search per concept and keeps the
// sessions every concept found, even when different messages carry the
// evidence. Results rank by the mean of each concept's best score; the legs
// share one vector generation, so their scores are comparable. truncated
// reports that a full candidate window or the limit may have hidden sessions.
func (b *directBackend) searchContentConcepts(
	ctx context.Context, f db.ContentSearchFilter, concepts []string,
) (page db.ContentSearchPage, truncated bool, err error) {
	limit := f.Limit
	if limit <= 0 {
		limit = maxConceptSearchLimit
	}
	candidateLimit := min(limit*5, maxConceptCandidates)
	best := make([][]db.ContentMatch, len(concepts))
	legs := make([]rrf.GroupLeg[string, conceptMember], len(concepts))
	for i, concept := range concepts {
		legFilter := f
		legFilter.Pattern = concept
		legFilter.Limit = candidateLimit
		legFilter.Cursor = 0
		legPage, err := b.db.SearchContent(ctx, legFilter)
		if err != nil {
			return db.ContentSearchPage{}, false, err
		}
		if len(legPage.Matches) >= candidateLimit || legPage.NextCursor > 0 {
			truncated = true
		}
		best[i] = bestMatchPerSession(legPage.Matches)
		groups := make([]rrf.Group[string, conceptMember], len(best[i]))
		for j, match := range best[i] {
			groups[j] = rrf.Group[string, conceptMember]{
				Key: match.SessionID, Members: []conceptMember{{leg: i, index: j}},
			}
		}
		legs[i] = rrf.GroupLeg[string, conceptMember]{
			Name: fmt.Sprintf("concept-%d", i), Weight: 1, Groups: groups,
		}
	}
	hits, err := rrf.FuseGroupsEvery(conceptRankConstant, legs)
	if err != nil {
		return db.ContentSearchPage{}, false, err
	}

	type ranked struct {
		match db.ContentMatch
		score float64
	}
	results := make([]ranked, 0, len(hits))
	for _, hit := range hits {
		evidence := make([]db.ConceptEvidence, len(concepts))
		var sum float64
		// Each leg has one member per session, so there is exactly one
		// alternate per concept.
		for _, alt := range hit.Alternates {
			match := best[alt.Member.leg][alt.Member.index]
			score := matchScore(match)
			sum += score
			evidence[alt.Member.leg] = db.ConceptEvidence{
				Concept: concepts[alt.Member.leg], Ordinal: match.Ordinal,
				OrdinalRange: match.OrdinalRange, Snippet: match.Snippet,
				Score: score, TranscriptRevision: match.TranscriptRevision,
			}
		}
		first := hit.Alternates[0].Member
		match := best[first.leg][first.index]
		mean := sum / float64(len(concepts))
		match.Score = &mean
		match.ConceptEvidence = evidence
		results = append(results, ranked{match: match, score: mean})
	}
	// FuseGroupsEvery returns reciprocal-rank order, which breaks score ties.
	slices.SortStableFunc(results, func(a, b ranked) int {
		return cmp.Compare(b.score, a.score)
	})
	if len(results) > limit {
		results = results[:limit]
		truncated = true
	}
	page.Matches = make([]db.ContentMatch, len(results))
	for i := range results {
		page.Matches[i] = results[i].match
	}
	return page, truncated, nil
}

// bestMatchPerSession keeps each session's highest-scoring match, preferring
// the earlier ordinal on a tie. Sessions keep the order in which the leg
// first ranked them.
func bestMatchPerSession(matches []db.ContentMatch) []db.ContentMatch {
	index := make(map[string]int, len(matches))
	out := make([]db.ContentMatch, 0, len(matches))
	for _, match := range matches {
		i, ok := index[match.SessionID]
		if !ok {
			index[match.SessionID] = len(out)
			out = append(out, match)
			continue
		}
		current := out[i]
		if matchScore(match) > matchScore(current) ||
			(matchScore(match) == matchScore(current) && match.Ordinal < current.Ordinal) {
			out[i] = match
		}
	}
	return out
}

func matchScore(match db.ContentMatch) float64 {
	if match.Score == nil {
		return 0
	}
	return *match.Score
}

// matchRevisionBound reports whether a match and all of its concept evidence
// carry the transcript revision their queries observed.
func matchRevisionBound(match db.ContentMatch) bool {
	if match.TranscriptRevision == "" {
		return false
	}
	for _, evidence := range match.ConceptEvidence {
		if evidence.TranscriptRevision == "" {
			return false
		}
	}
	return true
}
