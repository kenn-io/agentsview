package codexcloud

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

const (
	// usageBudget caps plan usage requests per account and pass, so a
	// large backlog never makes one pass unbounded.
	usageBudget = 5
	// maxUsageGroups and maxUsageIDs are the desktop app's limits for one
	// request. maxUsageIDs counts each root's own ID.
	maxUsageGroups = 100
	maxUsageIDs    = 1000
	// usageRetryFirst and usageRetryLimit time a root's failed or
	// unresolved attempts, and its re-entry wait while it keeps changing.
	usageRetryFirst = 5 * time.Minute
	usageRetryLimit = 6 * time.Hour
	// settleAfter ends retries this long after a root's last change.
	settleAfter = 7 * 24 * time.Hour
	// reentryQuiet resets the re-entry wait after this long without change.
	reentryQuiet = 24 * time.Hour
	// refreshEvery spaces refreshes of a usable row.
	refreshEvery = 24 * time.Hour
	// usageUnavailableFor pauses plan usage after a 403 or 404.
	usageUnavailableFor = 24 * time.Hour
	usageRateFirst      = 5 * time.Minute
	usageRateLimit      = time.Hour

	pauseUnavailable = "unavailable"
	pauseRateLimited = "rate_limited"
)

// doubled is first doubled for each attempt after the first, up to limit.
func doubled(first, limit time.Duration, attempts int) time.Duration {
	wait := first
	for range attempts - 1 {
		if wait >= limit {
			break
		}
		wait *= 2
	}
	return min(wait, limit)
}

// markPending marks a usage root pending after it or a descendant was
// fetched. A root already pending keeps its next attempt; a root with a
// usable row re-enters after its re-entry wait, so a running thread
// fetched every pass is not queried every pass.
func markPending(acct *AccountState, root string, now time.Time, running bool) {
	if entry, ok := acct.UsagePending[root]; ok {
		entry.LastChange = now
		if !running {
			entry.Reentries = 0
		}
		acct.UsagePending[root] = entry
		return
	}
	if ref, ok := acct.Refresh[root]; ok {
		reentries := ref.Reentries
		if !running || now.Sub(ref.LastChange) >= reentryQuiet {
			reentries = 0
		}
		acct.UsagePending[root] = PendingEntry{
			Since: now, NextAttempt: ref.LastAttempt.Add(doubled(usageRetryFirst, usageRetryLimit, reentries+1)),
			LastChange: now, HasRow: true, Reentries: reentries,
		}
		delete(acct.Refresh, root)
		return
	}
	acct.UsagePending[root] = PendingEntry{Since: now, NextAttempt: now, LastChange: now}
}

// linkUsage links a committed thread to its usage root, writes a "counted
// under" record to every thread newly counted under a root, and marks the
// root pending. The caller holds the account lock and saves acct.
func (s *Syncer) linkUsage(key AccountKey, acct *AccountState, threadID, parent string, now time.Time) error {
	root, moved, err := s.Store.LinkParent(key, threadID, parent)
	if err != nil {
		return err
	}
	for _, id := range moved {
		if err := s.countedUnder(key, acct, id, root, now); err != nil {
			return err
		}
	}
	running, err := s.groupRunning(key, acct, root)
	if err != nil {
		return err
	}
	markPending(acct, root, now, running)
	return nil
}

// countedUnder records that threadID's usage counts toward root. It needs
// no request, so it is written even while plan usage is paused.
func (s *Syncer) countedUnder(key AccountKey, acct *AccountState, threadID, root string, now time.Time) error {
	st, err := s.Store.LoadThread(key, threadID)
	if err != nil {
		return err
	}
	st.CountedUnder = root
	record, err := planRecord(mirror.KindPlanUsageOutcome,
		mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeCountedUnder, At: now, Root: root}, now)
	if err != nil {
		return err
	}
	if _, _, err := commitThread(s.Store, key, threadID, st, []mirror.Record{record}, now); err != nil {
		return err
	}
	delete(acct.UsagePending, threadID)
	delete(acct.Refresh, threadID)
	return nil
}

// groupRunning reports whether any thread in root's group has a turn in
// progress.
func (s *Syncer) groupRunning(key AccountKey, acct *AccountState, root string) (bool, error) {
	if acct.Unfinished[root].InProgress {
		return true, nil
	}
	descendants, err := s.Store.LoadDescendants(key, root)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(descendants, func(id string) bool { return acct.Unfinished[id].InProgress }), nil
}

func planRecord(kind mirror.Kind, data any, now time.Time) (mirror.Record, error) {
	raw, err := json.Marshal(data, json.Deterministic(true))
	if err != nil {
		return mirror.Record{}, fmt.Errorf("codex-cloud: encode %s record: %w", kind, err)
	}
	return mirror.Record{Kind: kind, FetchedAt: now, Data: raw}, nil
}

// usageRequest is one planned plan usage request.
type usageRequest struct {
	groups []UsageGroup
	ids    int
}

// runUsage is the plan usage step. It runs after the thread step, and
// even while the thread host is waiting; its failures never change thread
// sync status, waits, or watermarks. Only a local failure is returned.
func (s *Syncer) runUsage(ctx context.Context, gates Gates, key AccountKey, logins []Login) error {
	now := s.Now()
	requests, err := s.planUsage(key, now)
	if err != nil || len(requests) == 0 {
		if err == nil {
			s.showUsage(key, now)
		}
		return err
	}
	homes := slices.Clone(logins)
	for _, req := range requests {
		resp, login, err := s.queryUsage(ctx, gates, &homes, req.groups)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stop, applyErr := s.applyUsage(key, login, req.groups, resp, err)
		if applyErr != nil {
			return applyErr
		}
		s.noteUsage(key, resp, err, login)
		if stop {
			break
		}
	}
	s.showUsage(key, now)
	return nil
}

// noteUsage keeps the outcome of the latest plan usage request for the
// status line. Status is in memory, like the rest of the board.
func (s *Syncer) noteUsage(key AccountKey, resp UsageResponse, err error, login Login) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usageLines == nil {
		s.usageLines = map[AccountKey]string{}
	}
	switch {
	case err == nil:
		s.usageLines[key] = "Plan usage as of " + resp.DataAsOf.Local().Format("Jan 2 15:04")
	case errors.Is(err, errNoUsageHome):
		s.usageLines[key] = "Plan usage: signed out of ChatGPT in this Codex home. Run `codex login`."
	default:
		if f := FailureFor(err, login); f != FailRateLimited {
			s.usageLines[key] = "Plan usage: " + Message(f, time.Time{})
		}
	}
}

// planUsage picks this pass's requests: due pending roots, oldest first,
// then due refresh roots by next attempt, packed into at most usageBudget
// requests. A root too large to send settles here with its outcome.
func (s *Syncer) planUsage(key AccountKey, now time.Time) ([]usageRequest, error) {
	unlock := s.Store.Lock(key)
	defer unlock()
	acct, err := s.Store.LoadAccount(key)
	if err != nil {
		return nil, err
	}
	if now.Before(acct.UsagePause.Until) {
		return nil, nil
	}
	var pending []string
	for id, entry := range acct.UsagePending {
		if !now.Before(entry.NextAttempt) {
			pending = append(pending, id)
		}
	}
	slices.SortFunc(pending, func(a, b string) int {
		return cmp.Or(acct.UsagePending[a].Since.Compare(acct.UsagePending[b].Since), cmp.Compare(a, b))
	})
	var refresh []string
	for id, entry := range acct.Refresh {
		if now.Sub(entry.LastChange) >= settleAfter {
			delete(acct.Refresh, id)
			continue
		}
		if !now.Before(refreshDue(entry)) {
			refresh = append(refresh, id)
		}
	}
	slices.SortFunc(refresh, func(a, b string) int {
		return cmp.Or(refreshDue(acct.Refresh[a]).Compare(refreshDue(acct.Refresh[b])), cmp.Compare(a, b))
	})
	var requests []usageRequest
	for _, root := range append(pending, refresh...) {
		group, err := s.Store.UsageGroupFor(key, root)
		if err != nil {
			return nil, err
		}
		ids := 1 + len(group.Descendants)
		if ids > maxUsageIDs {
			if err := s.tooManyDescendants(key, &acct, root, now); err != nil {
				return nil, err
			}
			continue
		}
		last := len(requests) - 1
		if last < 0 || len(requests[last].groups) >= maxUsageGroups || requests[last].ids+ids > maxUsageIDs {
			if len(requests) == usageBudget {
				break
			}
			requests = append(requests, usageRequest{})
			last++
		}
		requests[last].groups = append(requests[last].groups, group)
		requests[last].ids += ids
	}
	return requests, s.Store.SaveAccount(key, acct)
}

func refreshDue(entry RefreshEntry) time.Time {
	due := entry.DataAsOf.Add(refreshEvery)
	if later := entry.LastAttempt.Add(refreshEvery); later.After(due) {
		due = later
	}
	return due
}

func (s *Syncer) tooManyDescendants(key AccountKey, acct *AccountState, root string, now time.Time) error {
	s.Log.Printf("codex-cloud: thread %s: too many sub-agent threads for OpenAI's usage report", root)
	delete(acct.UsagePending, root)
	delete(acct.Refresh, root)
	return s.appendPlan(key, root, now, mirror.KindPlanUsageOutcome,
		mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeTooManyDescendants, At: now})
}

// queryUsage sends one request through the first home that may serve plan
// usage. A 401 marks that home and moves to the next; homes is trimmed so
// later requests start there.
func (s *Syncer) queryUsage(
	ctx context.Context, gates Gates, homes *[]Login, groups []UsageGroup,
) (UsageResponse, Login, error) {
	for len(*homes) > 0 {
		login := (*homes)[0]
		if !s.Waits.HomeReady(login.Home, login.AuthModTime) || !s.Waits.UsageHomeReady(login.Home, login.AuthModTime) {
			*homes = (*homes)[1:]
			continue
		}
		resp, err := QueryUsage(ctx, s.UsageClient, gates.CodexCloud.UsageEndpoint, login, s.Version, groups)
		if httpErr, ok := errors.AsType[*HTTPError](err); ok && httpErr.Status == http.StatusUnauthorized {
			s.Log.Printf("codex-cloud: plan usage refused the login in %s", login.Home)
			s.Waits.RecordUsageHome(login.Home, login.AuthModTime)
			*homes = (*homes)[1:]
			continue
		}
		return resp, login, err
	}
	return UsageResponse{}, Login{}, errNoUsageHome
}

var errNoUsageHome = errors.New("codex-cloud: no Codex home can read plan usage")

// applyUsage records one request's result. It reports stop when no
// further request should be sent this pass.
func (s *Syncer) applyUsage(
	key AccountKey, login Login, groups []UsageGroup, resp UsageResponse, reqErr error,
) (stop bool, err error) {
	unlock := s.Store.Lock(key)
	defer unlock()
	acct, err := s.Store.LoadAccount(key)
	if err != nil {
		return false, err
	}
	now := s.Now()
	if reqErr != nil {
		stop, err = s.usageFailure(key, &acct, login, groups, reqErr, now)
	} else {
		err = s.usageAnswer(key, &acct, groups, resp, now)
	}
	if err != nil {
		return false, err
	}
	return stop, s.Store.SaveAccount(key, acct)
}

func (s *Syncer) usageFailure(
	key AccountKey, acct *AccountState, login Login, groups []UsageGroup, reqErr error, now time.Time,
) (stop bool, err error) {
	if errors.Is(reqErr, errNoUsageHome) {
		return true, nil
	}
	s.Log.Printf("codex-cloud: plan usage: %v", Redact(reqErr, login.Token))
	if httpErr, ok := errors.AsType[*HTTPError](reqErr); ok {
		switch httpErr.Status {
		case http.StatusForbidden, http.StatusNotFound:
			// OpenAI does not offer plan usage to this account. Each root
			// in the request says so, which keeps the work bounded.
			acct.UsagePause = UsagePauseState{Until: now.Add(usageUnavailableFor), Reason: pauseUnavailable}
			for _, g := range groups {
				if err := s.appendPlan(key, g.ThreadID, now, mirror.KindPlanUsageOutcome,
					mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeAccountUnavailable, At: now}); err != nil {
					return false, err
				}
			}
			return true, nil
		case http.StatusTooManyRequests:
			attempts := 1
			if acct.UsagePause.Reason == pauseRateLimited {
				attempts = acct.UsagePause.Attempts + 1
			}
			wait := doubled(usageRateFirst, usageRateLimit, attempts)
			if httpErr.RetryAfter > 0 {
				wait = httpErr.RetryAfter
			}
			acct.UsagePause = UsagePauseState{Until: now.Add(wait), Reason: pauseRateLimited, Attempts: attempts}
			return true, nil
		}
	}
	for _, g := range groups {
		if ref, ok := acct.Refresh[g.ThreadID]; ok {
			ref.LastAttempt = now
			acct.Refresh[g.ThreadID] = ref
			continue
		}
		entry, ok := acct.UsagePending[g.ThreadID]
		if !ok {
			continue
		}
		if now.Sub(entry.LastChange) >= settleAfter && entry.Answered.Before(entry.LastChange) {
			// No request was answered in the 7 days after the last change.
			// Its last row, if any, stays and is not marked final.
			delete(acct.UsagePending, g.ThreadID)
			if err := s.appendPlan(key, g.ThreadID, now, mirror.KindPlanUsageOutcome,
				mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeCouldNotCheck, At: now}); err != nil {
				return false, err
			}
			continue
		}
		entry.Attempts++
		entry.NextAttempt = now.Add(doubled(usageRetryFirst, usageRetryLimit, entry.Attempts))
		acct.UsagePending[g.ThreadID] = entry
	}
	return false, nil
}

func (s *Syncer) usageAnswer(key AccountKey, acct *AccountState, groups []UsageGroup, resp UsageResponse, now time.Time) error {
	if acct.UsagePause.Reason == pauseRateLimited {
		acct.UsagePause = UsagePauseState{}
	}
	rows := map[string]mirror.UsageRow{}
	for _, row := range resp.Threads {
		rows[row.ThreadID] = row
	}
	for _, g := range groups {
		root := g.ThreadID
		row, ok := rows[root]
		entry, pending := acct.UsagePending[root]
		if !pending {
			ref := acct.Refresh[root]
			if ok && row.DataStatus == mirror.UsageAvailable {
				if err := s.appendPlan(key, root, now, mirror.KindPlanUsageRow,
					mirror.PlanUsageRowData{Row: row, DataAsOf: resp.DataAsOf}); err != nil {
					return err
				}
				ref.DataAsOf, ref.LastAttempt = resp.DataAsOf, now
				acct.Refresh[root] = ref
				continue
			}
			// A refreshed row that is no longer usable makes the root
			// pending again.
			entry = PendingEntry{Since: now, LastChange: ref.LastChange, HasRow: true, Reentries: ref.Reentries}
		}
		entry.Answered = now
		settles := now.Sub(entry.LastChange) >= settleAfter
		switch {
		case ok && row.DataStatus == mirror.UsageAvailable:
			if err := s.appendPlan(key, root, now, mirror.KindPlanUsageRow,
				mirror.PlanUsageRowData{Row: row, DataAsOf: resp.DataAsOf}); err != nil {
				return err
			}
			reentries := entry.Reentries
			if entry.HasRow {
				reentries++
			}
			delete(acct.UsagePending, root)
			acct.Refresh[root] = RefreshEntry{
				DataAsOf: resp.DataAsOf, LastAttempt: now, LastChange: entry.LastChange, Reentries: reentries,
			}
			continue
		case ok:
			// A partial or unavailable row: stored, still pending, and
			// final once the root settles.
			if err := s.appendPlan(key, root, now, mirror.KindPlanUsageRow,
				mirror.PlanUsageRowData{Row: row, DataAsOf: resp.DataAsOf, Final: settles}); err != nil {
				return err
			}
		case settles:
			if err := s.settleLeftOut(key, root, now); err != nil {
				return err
			}
		default:
			if err := s.appendPlan(key, root, now, mirror.KindPlanUsageOutcome,
				mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeLeftOut, At: now}); err != nil {
				return err
			}
		}
		delete(acct.Refresh, root)
		if settles {
			delete(acct.UsagePending, root)
			continue
		}
		entry.Attempts++
		entry.NextAttempt = now.Add(doubled(usageRetryFirst, usageRetryLimit, entry.Attempts))
		acct.UsagePending[root] = entry
	}
	return nil
}

// settleLeftOut settles a root the report left out 7 days after its last
// change: its last row is stored again as final, or, with none, a
// "settled with no row" record.
func (s *Syncer) settleLeftOut(key AccountKey, root string, now time.Time) error {
	st, err := s.Store.LoadThread(key, root)
	if err != nil {
		return err
	}
	if len(st.LastPlanRow) == 0 {
		return s.appendPlan(key, root, now, mirror.KindPlanUsageOutcome,
			mirror.PlanUsageOutcomeData{Kind: mirror.OutcomeSettledNoRow, At: now})
	}
	var last mirror.PlanUsageRowData
	if err := json.Unmarshal(st.LastPlanRow, &last); err != nil {
		return fmt.Errorf("codex-cloud: thread %s: stored plan usage row: %w", root, err)
	}
	last.Final = true
	return s.appendPlan(key, root, now, mirror.KindPlanUsageRow, last)
}

// appendPlan appends one plan usage record to root's mirror file unless
// it equals the newest plan usage record there.
func (s *Syncer) appendPlan(key AccountKey, root string, now time.Time, kind mirror.Kind, data any) error {
	record, err := planRecord(kind, data, now)
	if err != nil {
		return err
	}
	st, err := s.Store.LoadThread(key, root)
	if err != nil {
		return err
	}
	_, _, err = commitThread(s.Store, key, root, st, []mirror.Record{record}, now)
	return err
}

// showUsage sets the account's plan usage line.
func (s *Syncer) showUsage(key AccountKey, now time.Time) {
	acct, err := s.Store.LoadAccount(key)
	if err != nil {
		s.Log.Printf("codex-cloud: %v", err)
		return
	}
	switch {
	case acct.UsagePause.Reason == pauseUnavailable && now.Before(acct.UsagePause.Until):
		s.Status.SetPlanUsage(string(key), "Usage isn't available for this account")
	case acct.UsagePause.Reason == pauseRateLimited && now.Before(acct.UsagePause.Until):
		s.Status.SetPlanUsage(string(key), "Plan usage: "+Message(FailRateLimited, acct.UsagePause.Until))
	default:
		s.usageMu.Lock()
		line := s.usageLines[key]
		s.usageMu.Unlock()
		if line == "" {
			line = "Plan usage: not checked yet"
		}
		s.Status.SetPlanUsage(string(key), line)
	}
}
