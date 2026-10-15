package codexcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// maxParentDepth stops a parent chain walk; a longer chain means a loop
// in the stored parents.
const maxParentDepth = 256

// known reports whether a thread is mirrored. A usage root is a thread
// with no known ancestor.
func (s *Store) known(key AccountKey, threadID string) (bool, error) {
	path, err := s.MirrorPath(key, threadID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("codex-cloud: stat mirror: %w", err)
	}
}

// RootOf follows a thread's resolved parents through known threads and
// returns the usage root its usage counts toward.
func (s *Store) RootOf(key AccountKey, threadID string) (string, error) {
	current := threadID
	for range maxParentDepth {
		st, err := s.LoadThread(key, current)
		if err != nil {
			return "", err
		}
		parent := st.ResolvedParent
		if parent == "" || parent == threadID {
			return current, nil
		}
		ok, err := s.known(key, parent)
		if err != nil {
			return "", err
		}
		if !ok {
			return current, nil
		}
		current = parent
	}
	return "", fmt.Errorf("codex-cloud: chat %s: parent chain longer than %d chats", threadID, maxParentDepth)
}

// LinkParent records child's resolved parent (empty for none) and keeps
// the usage groups in step. A parent that is not mirrored yet gets child
// in its waiting file, so child stays its own root until the parent
// arrives. Threads waiting for child move under child's root. The caller
// holds the account lock.
//
// count is called for every thread newly counted under a root other than
// itself, child's earlier descendants included, before that thread joins
// the root's group. The caller writes its "counted under" record there.
// A link stopped by an error or a crash is redone by linking again, so no
// moved thread misses its record.
func (s *Store) LinkParent(
	key AccountKey, child, parent string, count func(id, root string) error,
) (root string, err error) {
	if parent == child {
		parent = ""
	}
	st, err := s.LoadThread(key, child)
	if err != nil {
		return "", err
	}
	if st.ResolvedParent != parent {
		st.ResolvedParent = parent
		if err := s.SaveThread(key, child, st); err != nil {
			return "", err
		}
	}
	if parent != "" {
		ok, err := s.known(key, parent)
		if err != nil {
			return "", err
		}
		if !ok {
			if err := s.addWaiting(key, parent, child); err != nil {
				return "", err
			}
		}
	}
	root, err = s.RootOf(key, child)
	if err != nil {
		return "", err
	}
	if root != child {
		if err := s.moveGroup(key, child, root, count); err != nil {
			return "", err
		}
	}
	waiting, err := s.LoadWaiting(key, child)
	if err != nil {
		return "", err
	}
	for _, w := range waiting {
		// A thread whose parent changed since it began waiting is no
		// longer child's.
		wst, err := s.LoadThread(key, w)
		if err != nil {
			return "", err
		}
		if wst.ResolvedParent != child {
			continue
		}
		if err := s.moveGroup(key, w, root, count); err != nil {
			return "", err
		}
	}
	if len(waiting) > 0 {
		if err := s.SaveWaiting(key, child, nil); err != nil {
			return "", err
		}
	}
	return root, nil
}

// moveGroup moves a former root and its descendants into root's group.
// Threads not yet in the group are counted first, then the group is saved,
// and only then is the former root's own group removed, so a crash at any
// point leaves every thread in some group and a retry redoes what is left.
func (s *Store) moveGroup(key AccountKey, former, root string, count func(id, root string) error) error {
	if former == root {
		// Only a loop in the stored parents leads here; the root keeps
		// its group.
		return nil
	}
	moving, err := s.LoadDescendants(key, former)
	if err != nil {
		return err
	}
	moving = append([]string{former}, moving...)
	group, err := s.LoadDescendants(key, root)
	if err != nil {
		return err
	}
	var added []string
	for _, id := range moving {
		if id != root && !slices.Contains(group, id) && !slices.Contains(added, id) {
			added = append(added, id)
		}
	}
	for _, id := range added {
		if err := count(id, root); err != nil {
			return err
		}
	}
	if len(added) > 0 {
		if err := s.SaveDescendants(key, root, append(group, added...)); err != nil {
			return err
		}
	}
	return s.SaveDescendants(key, former, nil)
}

func (s *Store) addWaiting(key AccountKey, parent, child string) error {
	waiting, err := s.LoadWaiting(key, parent)
	if err != nil {
		return err
	}
	if slices.Contains(waiting, child) {
		return nil
	}
	return s.SaveWaiting(key, parent, append(waiting, child))
}

// UsageGroupFor builds root's plan usage group from stored state.
func (s *Store) UsageGroupFor(key AccountKey, root string) (UsageGroup, error) {
	st, err := s.LoadThread(key, root)
	if err != nil {
		return UsageGroup{}, err
	}
	descendants, err := s.LoadDescendants(key, root)
	if err != nil {
		return UsageGroup{}, err
	}
	return UsageGroup{ThreadID: root, CreatedAt: time.Unix(st.CreatedAt, 0), Descendants: descendants}, nil
}

// rebuildUsageQueue refills acct's plan usage queue from the per-thread
// state, for an account whose account.json was lost or set aside. Every
// mirrored usage root goes back where its newest plan usage record and its
// group's newest change put it; see restoreRoot. A root whose files cannot
// be read is logged and left out, so one damaged chat does not stop the
// account from syncing. It reads only local files and fetches nothing. It
// reports whether the account has any thread state files.
func (s *Store) rebuildUsageQueue(key AccountKey, acct *AccountState) (bool, error) {
	dir, err := s.accountDir(key)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("codex-cloud: read account directory: %w", err)
	}
	threads, skipped := false, 0
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), threadStateSuffix)
		if !ok || entry.IsDir() || !plainName(id) {
			continue
		}
		threads = true
		if err := s.restoreRoot(key, acct, id); err != nil {
			skipped++
			s.Log.Printf("codex-cloud: chat %s: %v; plan usage for this chat waits for its next change", id, err)
		}
	}
	if threads {
		summary := fmt.Sprintf("codex-cloud: account.json was missing or set aside, so AgentsView rebuilt the "+
			"plan usage queue from the saved chat state. %d chats will be checked now. %d stay on the daily refresh.",
			len(acct.UsagePending), len(acct.Refresh))
		if skipped > 0 {
			summary += fmt.Sprintf(" %d could not be read (logged above).", skipped)
		}
		s.Log.Print(summary)
	}
	return threads, nil
}

// restoreRoot puts one thread back in acct's plan usage queue if it is a
// mirrored usage root. A root whose newest plan usage record settled it
// after its group's newest change stays out. A root whose newest record is
// a complete row fetched after that change goes back on the refresh
// schedule, so it drops out without a request once its chat has gone
// settleAfter without a change, and a later partial answer cannot replace
// its row. A complete row fetched before that change leaves the check for
// the change outstanding, so that root is pending and keeps its row. Any
// other root is pending. Both kinds keep the group's newest change as
// their last change, not now, so a root that changed more than
// settleAfter ago settles on its first answer.
func (s *Store) restoreRoot(key AccountKey, acct *AccountState, id string) error {
	mirrored, err := s.known(key, id)
	if err != nil || !mirrored {
		return err
	}
	root, err := s.RootOf(key, id)
	if err != nil || root != id {
		return err
	}
	st, err := s.LoadThread(key, id)
	if err != nil {
		return err
	}
	change, err := s.groupChange(key, id, st)
	if err != nil {
		return err
	}
	plan, err := s.storedPlan(key, id, st)
	if err != nil {
		return err
	}
	switch {
	case plan.settledAt.After(change):
	case plan.available && (plan.fetchedAt.IsZero() || !change.After(plan.fetchedAt)):
		// With no last attempt, the refresh falls due a day after the
		// row's data.
		acct.Refresh[id] = RefreshEntry{DataAsOf: plan.dataAsOf, LastChange: change}
	case plan.available:
		// The chat changed after the row was fetched, so the request for
		// that change was still outstanding. On the refresh schedule a
		// root past settleAfter would drop out unasked.
		acct.UsagePending[id] = PendingEntry{Since: change, LastChange: change, HasRow: true}
	default:
		acct.UsagePending[id] = PendingEntry{Since: change, LastChange: change}
	}
	return nil
}

// groupChange is the newest change stored for root's group: the root's
// own change, the change that last marked it pending, and each
// descendant's change.
func (s *Store) groupChange(key AccountKey, root string, st ThreadState) (time.Time, error) {
	change := st.changedAt()
	if st.UsageChange.After(change) {
		change = st.UsageChange
	}
	descendants, err := s.LoadDescendants(key, root)
	if err != nil {
		return time.Time{}, err
	}
	for _, d := range descendants {
		dst, err := s.LoadThread(key, d)
		if err != nil {
			return time.Time{}, err
		}
		if c := dst.changedAt(); c.After(change) {
			change = c
		}
	}
	return change, nil
}

// planState is what a root's newest plan usage record says about it.
type planState struct {
	// settledAt is when the record that settled the root was stored, zero
	// when the newest record does not settle it.
	settledAt time.Time
	// available marks a newest record that is a complete row that is not
	// final; dataAsOf is that row's data time and fetchedAt when it was
	// fetched, zero in a state file saved before that was kept.
	available bool
	dataAsOf  time.Time
	fetchedAt time.Time
}

// storedPlan reads a root's newest plan usage record. A row that is not
// final is in the thread state. A final row or an outcome may settle the
// root, and only the mirror file has the time it was stored, so those are
// read from there.
func (s *Store) storedPlan(key AccountKey, id string, st ThreadState) (planState, error) {
	switch mirror.Kind(st.LastPlanRecordKind) {
	case mirror.KindPlanUsageRow:
		var row mirror.PlanUsageRowData
		if err := json.Unmarshal(st.LastPlanRow, &row); err != nil {
			return planState{}, fmt.Errorf("stored plan usage row: %w", err)
		}
		if !row.Final {
			return planState{
				available: row.Row.DataStatus == mirror.UsageAvailable, dataAsOf: row.DataAsOf, fetchedAt: st.LastPlanRowAt,
			}, nil
		}
		return s.mirrorPlan(key, id)
	case mirror.KindPlanUsageOutcome:
		return s.mirrorPlan(key, id)
	default:
		return planState{}, nil
	}
}

// mirrorPlan reads the newest plan usage record in a root's mirror file.
// A final row settles the root. Of the outcomes, "settled with no row",
// "could not check", and "too many sub-agent chats" settle it, and "left
// out" settles it only when it is the mark written in one commit with a
// final row, as settleLeftOut does.
func (s *Store) mirrorPlan(key AccountKey, id string) (planState, error) {
	path, err := s.MirrorPath(key, id)
	if err != nil {
		return planState{}, err
	}
	// A local read with no request behind it to cancel.
	records, err := mirror.ReadAll(context.Background(), path)
	if err != nil {
		return planState{}, fmt.Errorf("read mirror: %w", err)
	}
	var plan []mirror.Record
	for _, r := range records {
		if r.Kind == mirror.KindPlanUsageRow || r.Kind == mirror.KindPlanUsageOutcome {
			plan = append(plan, r)
		}
	}
	if len(plan) == 0 {
		return planState{}, nil
	}
	last := plan[len(plan)-1]
	if last.Kind == mirror.KindPlanUsageRow {
		return rowPlan(last)
	}
	var outcome mirror.PlanUsageOutcomeData
	if err := json.Unmarshal(last.Data, &outcome); err != nil {
		return planState{}, fmt.Errorf("stored plan usage outcome: %w", err)
	}
	switch outcome.Kind {
	case mirror.OutcomeSettledNoRow, mirror.OutcomeCouldNotCheck, mirror.OutcomeTooManyDescendants:
		return planState{settledAt: last.FetchedAt}, nil
	case mirror.OutcomeLeftOut:
		if len(plan) < 2 {
			return planState{}, nil
		}
		before := plan[len(plan)-2]
		if before.Kind != mirror.KindPlanUsageRow || !before.FetchedAt.Equal(last.FetchedAt) {
			return planState{}, nil
		}
		row, err := rowPlan(before)
		if err != nil || row.settledAt.IsZero() {
			// A row that is not final would be stored after the mark.
			return planState{}, err
		}
		return row, nil
	default:
		return planState{}, nil
	}
}

// rowPlan is what one stored plan usage row says about its root.
func rowPlan(record mirror.Record) (planState, error) {
	var row mirror.PlanUsageRowData
	if err := json.Unmarshal(record.Data, &row); err != nil {
		return planState{}, fmt.Errorf("stored plan usage row: %w", err)
	}
	if row.Final {
		return planState{settledAt: record.FetchedAt}, nil
	}
	return planState{
		available: row.Row.DataStatus == mirror.UsageAvailable, dataAsOf: row.DataAsOf, fetchedAt: record.FetchedAt,
	}, nil
}
