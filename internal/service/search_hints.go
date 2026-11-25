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

// ContentSearchContext shares a deadline across a keyword search and its follow-ups.
func ContentSearchContext(ctx context.Context, req ContentSearchRequest) (context.Context, context.CancelFunc) {
	if len(req.Concepts) == 0 && db.ContentScanBounded(req.Mode, false) {
		return db.WithContentScanDeadline(ctx)
	}
	return ctx, func() {}
}

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
// whether rows decide that. It mirrors the session scope SQL: automation is
// checked on every session along a parent path, the one-shot gate on the
// root only, and an automated root passes the one-shot gate when the probe
// shows automated sessions. A flat search checks only the session's own row.
//
// A tree search reaches a child through a parent path whose root passes the
// search's other filters, and the classifier cannot check those filters. So
// when a child's parent paths disagree about the probe, or loop, the match is
// undecided.
func (p hintProbe) shows(rows map[string]db.Session, id string, tree, exemptChildren bool) (shown, known bool) {
	switch p.verdict(rows, id, tree, exemptChildren, make(map[string]hintVerdict)) {
	case hintShown:
		return true, true
	case hintUnknown:
		return false, false
	default:
		return false, true
	}
}

type hintVerdict int

const (
	hintHidden hintVerdict = iota
	hintShown
	hintUnknown
)

// verdict decides one session. memo keeps each session's answer, so parents
// that split and rejoin are visited once. A session reached again while it is
// still being decided is on a parent loop and reads as undecided, which keeps
// memo correct: every session on a loop reaches it from any start.
func (p hintProbe) verdict(rows map[string]db.Session, id string, tree, exemptChildren bool, memo map[string]hintVerdict) hintVerdict {
	if v, ok := memo[id]; ok {
		return v
	}
	memo[id] = hintUnknown
	v := p.decide(rows, id, tree, exemptChildren, memo)
	memo[id] = v
	return v
}

func (p hintProbe) decide(rows map[string]db.Session, id string, tree, exemptChildren bool, memo map[string]hintVerdict) hintVerdict {
	s, ok := rows[id]
	if !ok {
		return hintUnknown
	}
	if !p.automated && s.IsAutomated {
		return hintHidden
	}
	parents := hintParentIDs(s)
	if !tree || hintTreeRoot(s) {
		if p.oneShot || s.UserMessageCount > 1 || p.automated && s.IsAutomated ||
			exemptChildren && db.IsSidebarChildRelationship(s.RelationshipType) && len(parents) > 0 {
			return hintShown
		}
		return hintHidden
	}
	// A session that is not a tree root lists at least one parent.
	var result hintVerdict
	for i, parent := range parents {
		switch v := p.verdict(rows, parent, tree, exemptChildren, memo); {
		case i == 0:
			result = v
		case v != result:
			result = hintUnknown
		}
		if result == hintUnknown {
			break
		}
	}
	return result
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

// hintLookup is a shared page whose matched sessions the classifier reads.
// walk says the page is classified as a tree, so each match's ancestors up to
// the tree roots are needed too.
type hintLookup struct {
	page *ContentSearchResult
	walk bool
}

// hintSessions loads the rows the classifier reads. Each parent level costs
// one more lookup; tried and climbed stop a parent loop.
func hintSessions(ctx context.Context, svc SessionService, lookups []hintLookup) (map[string]db.Session, error) {
	rows := make(map[string]db.Session)
	tried := make(map[string]bool)
	climbed := make(map[string]bool)
	var pending, queue []string
	need := func(id string) {
		if id != "" && !tried[id] {
			tried[id] = true
			pending = append(pending, id)
		}
	}
	climb := func(id string) {
		if id != "" && !climbed[id] {
			climbed[id] = true
			queue = append(queue, id)
			need(id)
		}
	}
	for _, l := range lookups {
		for _, match := range l.page.Matches {
			need(match.SessionID)
			if l.walk {
				climb(match.SessionID)
				climb(match.ParentSessionID)
			}
		}
	}
	for len(pending) > 0 || len(queue) > 0 {
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
		// Every queued session has been fetched by now, either in this
		// batch or an earlier one.
		climbing := queue
		queue = nil
		for _, id := range climbing {
			if s, ok := rows[id]; ok && !hintTreeRoot(s) {
				for _, parent := range hintParentIDs(s) {
					climb(parent)
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
// A vector search keeps one search per class.
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
	// A vector retry for one class can rank a different candidate window than
	// the shared retry, so the shared page cannot vouch for a class's own
	// search there. Each class then runs its own search, and the shared page
	// feeds only the combined hint, which needs no rows.
	sortShared := !vector
	var lookup []hintLookup
	if sortShared && flatShared && flatPage != nil {
		lookup = append(lookup, hintLookup{flatPage, tree})
	}
	if treeShared && treePage != nil {
		lookup = append(lookup, hintLookup{treePage, true})
	}
	rows, err := hintSessions(ctx, svc, lookup)
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
				// A match whose rows are missing is unsorted. It still counts
				// when show says so, and then the count is a lower bound.
				shown, known := show(match.SessionID)
				if !known {
					lower, unsorted = true, true
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
	// classify counts one hint from its shared page. When that page failed or
	// is a vector page, or showed none of the hint's matches while it was
	// incomplete or held matches it could not sort, the hint's own search runs
	// so one busy class cannot hide another.
	classify := func(probe hintProbe, page *ContentSearchResult, shared, pageTree bool) {
		if page == nil && !shared {
			singlesOK = false
			return
		}
		show := func(id string) (bool, bool) { return probe.shows(rows, id, pageTree, exempt) }
		if page != nil && (sortShared || !shared) {
			added, unsorted := measure(probe, page, shared, show)
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
		reach := hintProbe{oneShot: true, automated: true}
		measure(hintProbe{code: "excluded_combined", args: args}, combined, true, func(id string) (bool, bool) {
			if singles[id] {
				return false, true
			}
			// Every match on this page is hidden by default and shown by the
			// combined retry. One whose rows are missing, as on every vector
			// page, counts toward a lower bound, since a single retry might
			// also show it.
			shown, known := reach.shows(rows, id, children || tree, false)
			if !known {
				return true, false
			}
			if !shown {
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
