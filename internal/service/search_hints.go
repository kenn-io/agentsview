package service

import (
	"context"
	"fmt"
)

// ContentSearchHint describes a measured reason an empty search can hide matches.
// RetryWith contains the arguments to change while preserving the original request.
type ContentSearchHint struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RetryWith map[string]any `json:"retry_with"`
}

// SearchContentHints probes default exclusions only for an empty first page.
// Diagnostics are optional: a failed probe contributes no hint.
func SearchContentHints(ctx context.Context, svc SessionService, req ContentSearchRequest, res *ContentSearchResult, explainChildren bool) []ContentSearchHint {
	if req.Cursor != 0 || len(res.Matches) != 0 || res.NextCursor != 0 || res.CandidateTruncated {
		return nil
	}
	var hints []ContentSearchHint
	probe := func(code, class string, retry ContentSearchRequest, args map[string]any, children bool) {
		retry.Limit = 50
		retry.Context = 0
		page, err := svc.SearchContent(ctx, retry)
		if err != nil {
			return
		}
		count := 0
		for _, match := range page.Matches {
			if !children || match.ParentSessionID != "" {
				count++
			}
		}
		if count == 0 {
			return
		}
		n := fmt.Sprint(count)
		wording := "matches are"
		if count == 1 {
			wording = "match is"
		}
		if page.NextCursor != 0 || page.CandidateTruncated {
			n = "more than " + n
			wording = "matches are"
		}
		hints = append(hints, ContentSearchHint{Code: code, Message: fmt.Sprintf("%s %s in %s sessions, hidden by default", n, wording, class), RetryWith: args})
	}
	if !req.IncludeOneShot {
		retry := req
		retry.IncludeOneShot = true
		probe("hidden_one_shot", "one-shot", retry, map[string]any{"include_one_shot": true}, false)
	}
	if !req.IncludeAutomated {
		retry := req
		retry.IncludeAutomated = true
		probe("hidden_automated", "automated", retry, map[string]any{"include_automated": true}, false)
	}
	lexical := req.Mode == "" || req.Mode == "substring" || req.Mode == "regex" || req.Mode == "fts"
	if explainChildren && lexical && req.Scope == "" && !req.IncludeChildren {
		retry := req
		retry.IncludeChildren, retry.IncludeOneShot, retry.IncludeAutomated = true, true, true
		args := map[string]any{"include_children": true, "include_one_shot": true, "include_automated": true}
		if req.Mode != "fts" {
			retry.Scope = "subordinate"
		}
		probe("hidden_children", "child", retry, args, true)
	}
	return hints
}
