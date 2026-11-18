package service

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

// MaxListSessionIDs is the most session IDs one List call may select; the
// HTTP session list rejects larger batches.
const MaxListSessionIDs = 100

// ContentSearchHint describes a measured reason an empty search can hide matches.
// RetryWith contains the arguments to change while preserving the original request.
type ContentSearchHint struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RetryWith map[string]any `json:"retry_with"`
}

// hintProbe is the retry one hint suggests: the original request plus the
// classes in args. oneShot, automated, and children say whether the retry
// shows those sessions.
type hintProbe struct {
	code, class                  string
	args                         map[string]any
	oneShot, automated, children bool
}

// shows reports whether the probe's search shows a match whose session chain
// runs from the matched session to its tree root. It mirrors the session
// scope SQL: automation is checked on every chain member, the one-shot gate
// on the root only, and an automated root passes the one-shot gate when the
// probe shows automated sessions.
func (p hintProbe) shows(chain []db.Session, exemptChildren bool) bool {
	if !p.automated && slices.ContainsFunc(chain, func(s db.Session) bool { return s.IsAutomated }) {
		return false
	}
	root := chain[len(chain)-1]
	return p.oneShot || root.UserMessageCount > 1 || p.automated && root.IsAutomated ||
		exemptChildren && db.IsSidebarChildRelationship(root.RelationshipType) && hintParentID(root) != ""
}

func hintParentID(s db.Session) string {
	if s.ParentSessionID == nil {
		return ""
	}
	return *s.ParentSessionID
}

// hintTreeRoot mirrors db.BuildCanonicalRootWhere without orphans.
func hintTreeRoot(s db.Session) bool {
	return !db.IsCanonicalChildRelationship(s.RelationshipType) || hintParentID(s) == ""
}

// hintChain returns the session chain for id, or nil when a member is
// unknown or the parent links loop. A flat search checks only the session's
// own row.
func hintChain(rows map[string]db.Session, id string, tree bool) []db.Session {
	s, ok := rows[id]
	if !ok {
		return nil
	}
	chain := []db.Session{s}
	seen := map[string]bool{id: true}
	for tree && !hintTreeRoot(s) {
		parentID := hintParentID(s)
		parent, ok := rows[parentID]
		if !ok || seen[parentID] {
			return nil
		}
		seen[parentID] = true
		chain = append(chain, parent)
		s = parent
	}
	return chain
}

// hintSessions loads the rows the classifier reads: the matched sessions
// and, when walk is set, their ancestors up to each tree root. Each parent
// level costs one more lookup; tried stops a parent loop.
func hintSessions(ctx context.Context, svc SessionService, pages []*ContentSearchResult, walk bool) (map[string]db.Session, error) {
	rows := make(map[string]db.Session)
	tried := make(map[string]bool)
	var pending []string
	add := func(id string) {
		if id != "" && !tried[id] {
			tried[id] = true
			pending = append(pending, id)
		}
	}
	for _, page := range pages {
		for _, match := range page.Matches {
			add(match.SessionID)
			if walk {
				add(match.ParentSessionID)
			}
		}
	}
	for len(pending) > 0 {
		batch := pending
		pending = nil
		for ids := range slices.Chunk(batch, MaxListSessionIDs) {
			list, err := svc.List(ctx, ListFilter{IDs: ids, Limit: db.MaxSessionLimit})
			if err != nil {
				return nil, fmt.Errorf("hint session lookup: %w", err)
			}
			for _, s := range list.Sessions {
				rows[s.ID] = s
			}
		}
		if !walk {
			break
		}
		for _, id := range batch {
			if s, ok := rows[id]; ok && !hintTreeRoot(s) {
				add(hintParentID(s))
			}
		}
	}
	return rows, nil
}

// SearchContentHints explains an empty first page by searching once more
// with the hidden classes included and sorting each match by the classes
// that hid it. A lexical search that explains children also runs a
// parent-tree search, because child sessions are selected through their
// root while one-shot and automated sessions are matched on their own rows.
// Diagnostics are optional: a failed search or session lookup contributes no
// hint. keepMatch applies caller activity filtering when the backend has not
// applied it.
func SearchContentHints(ctx context.Context, svc SessionService, req ContentSearchRequest, res *ContentSearchResult, explainChildren bool, keepMatch func(db.ContentMatch) bool) []ContentSearchHint {
	if req.Cursor != 0 || res.NextCursor != 0 || res.CandidateTruncated {
		return nil
	}
	for _, match := range res.Matches {
		if keepMatch == nil || res.ActiveFilterApplied || keepMatch(match) {
			return nil
		}
	}
	vector := len(req.Concepts) > 0 || req.Mode == "semantic" || req.Mode == "hybrid"
	lexical := !vector && (req.Mode == "" || req.Mode == "substring" || req.Mode == "regex" || req.Mode == "fts")
	// These searches scope every session by its own row, with no child
	// exclusion and no one-shot gate on children (db.SemanticContentSessionFilter).
	exempt := vector || req.Mode == "terms" || req.Scope != "" && db.ContentSearchModeSupportsScope(req.Mode)
	// A named session is also matched on its own row (db.BuildContentScopeSQL).
	flatOnly := exempt || req.SessionID != ""
	children := explainChildren && lexical && !req.IncludeChildren && !flatOnly
	tree := req.IncludeChildren && !flatOnly

	var probes []hintProbe
	args := make(map[string]any)
	if !req.IncludeOneShot {
		probes = append(probes, hintProbe{"excluded_one_shot", "one-shot", map[string]any{"include_one_shot": true}, true, req.IncludeAutomated, req.IncludeChildren})
		args["include_one_shot"] = true
	}
	if !req.IncludeAutomated {
		probes = append(probes, hintProbe{"excluded_automated", "automated", map[string]any{"include_automated": true}, req.IncludeOneShot, true, req.IncludeChildren})
		args["include_automated"] = true
	}
	child := hintProbe{"excluded_children", "child", map[string]any{"include_children": true}, req.IncludeOneShot, req.IncludeAutomated, true}
	if children {
		args["include_children"] = true
	}
	if len(args) == 0 {
		return nil
	}

	search := func(retry ContentSearchRequest) *ContentSearchResult {
		retry.Context = 0
		page, err := svc.SearchContent(ctx, retry)
		if err != nil {
			return nil
		}
		return page
	}
	// A page that serves one hint is that hint's own search and needs no
	// sorting. A shared page is sorted by the hidden sessions' rows.
	flatShared := len(probes) > 1
	treeShared := children && len(args) > 1
	// A shared lexical page asks for the most matches the API returns so one
	// class rarely crowds another off it; a larger vector limit would widen
	// the candidate fetch instead.
	shareLimit := func(retry ContentSearchRequest, shared bool) ContentSearchRequest {
		if shared && !vector {
			retry.Limit = db.MaxContentSearchLimit
		}
		return retry
	}
	var flatPage, treePage *ContentSearchResult
	if len(probes) > 0 {
		retry := req
		retry.IncludeOneShot, retry.IncludeAutomated = true, true
		flatPage = search(shareLimit(retry, flatShared))
	}
	if children {
		retry := req
		retry.IncludeOneShot, retry.IncludeAutomated, retry.IncludeChildren = true, true, true
		treePage = search(shareLimit(retry, treeShared))
	}
	var lookup []*ContentSearchResult
	if flatShared && flatPage != nil {
		lookup = append(lookup, flatPage)
	}
	if treeShared && treePage != nil {
		lookup = append(lookup, treePage)
	}
	rows, err := hintSessions(ctx, svc, lookup, treeShared || tree && flatShared)
	if err != nil {
		if flatShared {
			flatPage = nil
		}
		if treeShared {
			treePage = nil
		}
	}

	var hints []ContentSearchHint
	singles := make(map[string]bool)
	singlesOK := true
	// measure appends a hint for the page's matches that show reports and
	// says whether it did and whether some matches could not be sorted.
	measure := func(probe hintProbe, page *ContentSearchResult, shared, pageTree bool, show func([]db.Session) bool) (added, unsorted bool) {
		count := 0
		lower := vector || page.CandidateTruncated
		for _, match := range page.Matches {
			if keepMatch != nil && !page.ActiveFilterApplied && !keepMatch(match) {
				lower = true
				continue
			}
			if shared {
				chain := hintChain(rows, match.SessionID, pageTree)
				if chain == nil {
					lower, unsorted = true, true
					continue
				}
				if !show(chain) {
					continue
				}
			}
			if probe.code != "excluded_combined" {
				singles[match.SessionID] = true
			}
			count++
		}
		if count == 0 {
			return false, unsorted
		}
		n := strconv.Itoa(count)
		wording := "matches are"
		if count == 1 {
			wording = "match is"
		}
		if page.NextCursor != 0 && (shared || keepMatch != nil && !page.ActiveFilterApplied) {
			lower = true
		}
		if lower {
			n = "at least " + n
		} else if page.NextCursor != 0 {
			n = "more than " + n
			wording = "matches are"
		}
		message := fmt.Sprintf("%s %s in %s sessions, hidden by default", n, wording, probe.class)
		if probe.code == "excluded_combined" {
			message = fmt.Sprintf("%s %s hidden by default", n, wording)
		}
		hints = append(hints, ContentSearchHint{Code: probe.code, Message: message, RetryWith: probe.args})
		return true, unsorted
	}
	// classify counts one hint from its shared page. When that page failed,
	// or showed none of the hint's matches while it was incomplete or held
	// matches it could not sort, the hint's own search runs so one busy class
	// cannot hide another.
	classify := func(probe hintProbe, page *ContentSearchResult, shared, pageTree bool) {
		if page == nil && !shared {
			singlesOK = false
			return
		}
		show := func(chain []db.Session) bool { return probe.shows(chain, exempt) }
		if page != nil {
			added, unsorted := measure(probe, page, shared, pageTree, show)
			if added || !shared || !unsorted && page.NextCursor == 0 && !page.CandidateTruncated {
				return
			}
		}
		retry := req
		retry.IncludeOneShot, retry.IncludeAutomated, retry.IncludeChildren = probe.oneShot, probe.automated, probe.children
		own := search(retry)
		if own == nil {
			singlesOK = false
			return
		}
		measure(probe, own, false, pageTree, nil)
	}
	for _, probe := range probes {
		classify(probe, flatPage, flatShared, tree)
	}
	if children {
		classify(child, treePage, treeShared, true)
	}
	combined := treePage
	if !children && flatShared {
		combined = flatPage
	}
	if singlesOK && combined != nil && (treeShared || !children && flatShared) {
		measure(hintProbe{code: "excluded_combined", args: args}, combined, true, children || tree, func(chain []db.Session) bool {
			s := chain[0]
			if singles[s.ID] {
				return false
			}
			// The flat page may not list every session a flat retry shows; a
			// root session's own row decides that retry directly.
			if !children || !hintTreeRoot(s) || db.IsSidebarChildRelationship(s.RelationshipType) {
				return true
			}
			return !slices.ContainsFunc(probes, func(p hintProbe) bool { return p.shows(chain, false) })
		})
	}
	return hints
}
