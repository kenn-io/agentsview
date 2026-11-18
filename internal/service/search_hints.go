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

// shows reports whether the probe's search shows a match on session id, and
// whether rows held every session needed to decide that. A tree search
// reaches a child through any of its parents, so the match shows when some
// path from the session to a tree root passes. It mirrors the session scope
// SQL: automation is checked on every session along the path, the one-shot
// gate on the root only, and an automated root passes the one-shot gate when
// the probe shows automated sessions. A flat search checks only the
// session's own row.
func (p hintProbe) shows(rows map[string]db.Session, id string, tree, exemptChildren bool) (shown, known bool) {
	return p.showsPath(rows, id, tree, exemptChildren, make(map[string]bool))
}

func (p hintProbe) showsPath(rows map[string]db.Session, id string, tree, exemptChildren bool, path map[string]bool) (shown, known bool) {
	if path[id] {
		// A parent loop never reaches a root.
		return false, true
	}
	s, ok := rows[id]
	if !ok {
		return false, false
	}
	if !p.automated && s.IsAutomated {
		return false, true
	}
	parents := hintParentIDs(s)
	if !tree || hintTreeRoot(s) {
		return p.oneShot || s.UserMessageCount > 1 || p.automated && s.IsAutomated ||
			exemptChildren && db.IsSidebarChildRelationship(s.RelationshipType) && len(parents) > 0, true
	}
	path[id] = true
	defer delete(path, id)
	known = true
	for _, parent := range parents {
		shown, ok := p.showsPath(rows, parent, tree, exemptChildren, path)
		if shown {
			return true, true
		}
		known = known && ok
	}
	return false, known
}

// hintParentIDs returns every parent a session lists. Hosted reads fill
// ParentSessionIDs and leave ParentSessionID nil when several parents exist.
func hintParentIDs(s db.Session) []string {
	if len(s.ParentSessionIDs) > 0 {
		return s.ParentSessionIDs
	}
	if s.ParentSessionID != nil && *s.ParentSessionID != "" {
		return []string{*s.ParentSessionID}
	}
	return nil
}

// hintTreeRoot mirrors db.BuildCanonicalRootWhere without orphans.
func hintTreeRoot(s db.Session) bool {
	return !db.IsCanonicalChildRelationship(s.RelationshipType) || len(hintParentIDs(s)) == 0
}

// hintSessions loads the rows the classifier reads: the matched sessions
// and, when walk is set, every ancestor up to the tree roots. Each parent
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
				for _, parent := range hintParentIDs(s) {
					add(parent)
				}
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
	measure := func(probe hintProbe, page *ContentSearchResult, shared bool, show func(id string) (shown, known bool)) (added, unsorted bool) {
		count := 0
		lower := vector || page.CandidateTruncated
		for _, match := range page.Matches {
			if keepMatch != nil && !page.ActiveFilterApplied && !keepMatch(match) {
				lower = true
				continue
			}
			if shared {
				shown, known := show(match.SessionID)
				if !known {
					lower, unsorted = true, true
					continue
				}
				if !shown {
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
	// A semantic search returns no cursor, so a page holding the full limit
	// may have left matches out.
	vectorLimit := req.Limit
	if vectorLimit <= 0 || vectorLimit > db.MaxContentSearchLimit {
		vectorLimit = db.DefaultContentSearchLimit
	}
	incomplete := func(page *ContentSearchResult) bool {
		return page.NextCursor != 0 || page.CandidateTruncated || vector && len(page.Matches) >= vectorLimit
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
		show := func(id string) (bool, bool) { return probe.shows(rows, id, pageTree, exempt) }
		if page != nil {
			added, unsorted := measure(probe, page, shared, show)
			if added || !shared || !unsorted && !incomplete(page) {
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
		measure(probe, own, false, nil)
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
		// reach shows every session the combined retry can: one that reaches
		// a tree root through known rows.
		reach := hintProbe{oneShot: true, automated: true, children: true}
		measure(hintProbe{code: "excluded_combined", args: args}, combined, true, func(id string) (bool, bool) {
			if shown, known := reach.shows(rows, id, children || tree, false); !shown || !known {
				return false, known
			}
			if singles[id] {
				return false, true
			}
			// The flat page may not list every session a flat retry shows; a
			// root session's own row decides that retry directly.
			s := rows[id]
			if !children || !hintTreeRoot(s) || db.IsSidebarChildRelationship(s.RelationshipType) {
				return true, true
			}
			return !slices.ContainsFunc(probes, func(p hintProbe) bool {
				shown, _ := p.shows(rows, id, false, false)
				return shown
			}), true
		})
	}
	return hints
}
