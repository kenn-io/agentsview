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
	vector := len(req.Concepts) > 0 || req.Mode == "semantic" || req.Mode == "hybrid"
	lexical := !vector && (req.Mode == "" || req.Mode == "substring" || req.Mode == "regex" || req.Mode == "fts")
	children := explainChildren && lexical && !req.IncludeChildren
	all := req
	args := make(map[string]any)
	if !req.IncludeOneShot {
		all.IncludeOneShot = true
		args["include_one_shot"] = true
	}
	if !req.IncludeAutomated {
		all.IncludeAutomated = true
		args["include_automated"] = true
	}
	if children {
		all.IncludeChildren = true
		args["include_children"] = true
	}
	search := func(retry ContentSearchRequest) (*ContentSearchResult, error) {
		if !vector {
			retry.Limit = max(req.Limit, db.DefaultContentSearchLimit)
		}
		retry.Context = 0
		return svc.SearchContent(ctx, retry)
	}
	var expanded *ContentSearchResult
	if len(args) > 1 {
		var err error
		all.Context = 0
		expanded, err = svc.SearchContent(ctx, all)
		if err != nil {
			expanded = nil
		}
		if err == nil && len(expanded.Matches) == 0 && expanded.NextCursor == 0 && !expanded.CandidateTruncated {
			return nil
		}
	}
	singleSessions := make(map[string]bool)
	singlesOK := true
	measure := func(code, class string, page *ContentSearchResult, args map[string]any, exclude map[string]bool) {
		count := 0
		filtered := false
		for _, match := range page.Matches {
			if keepMatch != nil && !page.ActiveFilterApplied && !keepMatch(match) {
				filtered = true
				continue
			}
			if exclude == nil {
				singleSessions[match.SessionID] = true
			}
			if exclude[match.SessionID] {
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
		if filtered || page.CandidateTruncated || vector || exclude != nil && page.NextCursor != 0 || keepMatch != nil && !page.ActiveFilterApplied && page.NextCursor != 0 {
			n = "at least " + n
		} else if page.NextCursor != 0 {
			n = "more than " + n
			wording = "matches are"
		}
		message := fmt.Sprintf("%s %s in %s sessions, hidden by default", n, wording, class)
		if code == "hidden_combined" {
			message = fmt.Sprintf("%s %s in child sessions hidden by more than one default", n, wording)
		}
		hints = append(hints, ContentSearchHint{Code: code, Message: message, RetryWith: args})
	}
	probe := func(code, class string, retry ContentSearchRequest, args map[string]any) bool {
		page, err := search(retry)
		if err != nil {
			return false
		}
		measure(code, class, page, args, nil)
		return true
	}
	if !req.IncludeOneShot {
		retry := req
		retry.IncludeOneShot = true
		singlesOK = probe("hidden_one_shot", "one-shot", retry, map[string]any{"include_one_shot": true}) && singlesOK
	}
	if !req.IncludeAutomated {
		retry := req
		retry.IncludeAutomated = true
		singlesOK = probe("hidden_automated", "automated", retry, map[string]any{"include_automated": true}) && singlesOK
	}
	if children {
		retry := req
		retry.IncludeChildren = true
		singlesOK = probe("hidden_children", "child", retry, map[string]any{"include_children": true}) && singlesOK
	}
	if singlesOK && expanded != nil {
		measure("hidden_combined", "child", expanded, args, singleSessions)
	}
	return hints
}
