package service

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// ContentSearchHint describes a measured reason an empty search can hide matches.
// RetryWith contains the arguments to change while preserving the original request.
type ContentSearchHint struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RetryWith map[string]any `json:"retry_with"`
}

// SearchContentHints probes default exclusions only for an empty first page.
// Diagnostics are optional: a failed probe contributes no hint. keepMatch applies
// caller activity filtering when the backend has not applied it.
func SearchContentHints(ctx context.Context, svc SessionService, req ContentSearchRequest, res *ContentSearchResult, explainChildren bool, keepMatch func(db.ContentMatch) bool) []ContentSearchHint {
	if req.Cursor != 0 || len(res.Matches) != 0 || res.NextCursor != 0 || res.CandidateTruncated {
		return nil
	}
	var hints []ContentSearchHint
	probe := func(code, class string, retry ContentSearchRequest, args map[string]any) {
		retry.Limit = 50
		retry.Context = 0
		page, err := svc.SearchContent(ctx, retry)
		if err != nil {
			return
		}
		count := 0
		filtered := false
		for _, match := range page.Matches {
			if keepMatch != nil && !page.ActiveFilterApplied && !keepMatch(match) {
				filtered = true
				continue
			}
			count++
		}
		if count == 0 {
			return
		}
		n := fmt.Sprint(count)
		wording := "matches are"
		if count == 1 {
			wording = "match is"
		}
		if filtered || page.CandidateTruncated || keepMatch != nil && !page.ActiveFilterApplied && page.NextCursor != 0 {
			n = "at least " + n
		} else if page.NextCursor != 0 {
			n = "more than " + n
			wording = "matches are"
		}
		hints = append(hints, ContentSearchHint{Code: code, Message: fmt.Sprintf("%s %s in %s sessions, hidden by default", n, wording, class), RetryWith: args})
	}
	if !req.IncludeOneShot {
		retry := req
		retry.IncludeOneShot = true
		probe("hidden_one_shot", "one-shot", retry, map[string]any{"include_one_shot": true})
	}
	if !req.IncludeAutomated {
		retry := req
		retry.IncludeAutomated = true
		probe("hidden_automated", "automated", retry, map[string]any{"include_automated": true})
	}
	lexical := req.Mode == "" || req.Mode == "substring" || req.Mode == "regex" || req.Mode == "fts"
	if explainChildren && lexical && req.Scope == "" && !req.IncludeChildren {
		retry := req
		retry.IncludeChildren = true
		args := map[string]any{"include_children": true}
		probe("hidden_children", "child", retry, args)
	}
	return hints
}
