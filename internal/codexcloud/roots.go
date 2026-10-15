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
// mirrored usage root without a settled plan usage outcome becomes
// pending. Its last change is the newest change stored for its group, not
// now, so a root that changed more than settleAfter ago settles on its
// first answer. It reads only local files and fetches nothing. It reports
// whether the account has any thread state files.
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
	threads := false
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), threadStateSuffix)
		if !ok || entry.IsDir() || !plainName(id) {
			continue
		}
		threads = true
		pending, change, err := s.unsettledRoot(key, id)
		if err != nil {
			return false, err
		}
		if pending {
			acct.UsagePending[id] = PendingEntry{Since: change, LastChange: change}
		}
	}
	if threads {
		s.Log.Printf("codex-cloud: account state was missing or unreadable; rebuilt the plan usage queue "+
			"with %d chats from the saved chat state", len(acct.UsagePending))
	}
	return threads, nil
}

// unsettledRoot reports whether id is a mirrored usage root whose plan
// usage has not settled, and the newest change stored for its group.
func (s *Store) unsettledRoot(key AccountKey, id string) (bool, time.Time, error) {
	mirrored, err := s.known(key, id)
	if err != nil || !mirrored {
		return false, time.Time{}, err
	}
	root, err := s.RootOf(key, id)
	if err != nil || root != id {
		return false, time.Time{}, err
	}
	st, err := s.LoadThread(key, id)
	if err != nil {
		return false, time.Time{}, err
	}
	settled, err := s.planSettled(key, id, st)
	if err != nil || settled {
		return false, time.Time{}, err
	}
	change := st.changedAt()
	if st.UsageChange.After(change) {
		change = st.UsageChange
	}
	descendants, err := s.LoadDescendants(key, id)
	if err != nil {
		return false, time.Time{}, err
	}
	for _, d := range descendants {
		dst, err := s.LoadThread(key, d)
		if err != nil {
			return false, time.Time{}, err
		}
		if c := dst.changedAt(); c.After(change) {
			change = c
		}
	}
	return true, change, nil
}

// planSettled reports whether a root's newest plan usage record settles
// it: a final row, or an outcome that ends its checks.
func (s *Store) planSettled(key AccountKey, id string, st ThreadState) (bool, error) {
	switch mirror.Kind(st.LastPlanRecordKind) {
	case mirror.KindPlanUsageRow:
		var row mirror.PlanUsageRowData
		if err := json.Unmarshal(st.LastPlanRow, &row); err != nil {
			return false, fmt.Errorf("codex-cloud: chat %s: stored plan usage row: %w", id, err)
		}
		return row.Final, nil
	case mirror.KindPlanUsageOutcome:
		return s.outcomeSettled(key, id)
	default:
		return false, nil
	}
}

// outcomeSettled reads the newest plan usage outcome in a root's mirror
// file, which its state does not keep, and reports whether it settles the
// root. A "left out" outcome settles it only when it is the mark written in
// one commit with a final row, as settleLeftOut does.
func (s *Store) outcomeSettled(key AccountKey, id string) (bool, error) {
	path, err := s.MirrorPath(key, id)
	if err != nil {
		return false, err
	}
	// A local read with no request behind it to cancel.
	records, err := mirror.ReadAll(context.Background(), path)
	if err != nil {
		return false, fmt.Errorf("codex-cloud: chat %s: read mirror: %w", id, err)
	}
	var plan []mirror.Record
	for _, r := range records {
		if r.Kind == mirror.KindPlanUsageRow || r.Kind == mirror.KindPlanUsageOutcome {
			plan = append(plan, r)
		}
	}
	if len(plan) == 0 || plan[len(plan)-1].Kind != mirror.KindPlanUsageOutcome {
		return false, nil
	}
	last := plan[len(plan)-1]
	var outcome mirror.PlanUsageOutcomeData
	if err := json.Unmarshal(last.Data, &outcome); err != nil {
		return false, fmt.Errorf("codex-cloud: chat %s: stored plan usage outcome: %w", id, err)
	}
	switch outcome.Kind {
	case mirror.OutcomeSettledNoRow, mirror.OutcomeCouldNotCheck, mirror.OutcomeTooManyDescendants:
		return true, nil
	case mirror.OutcomeLeftOut:
		if len(plan) < 2 {
			return false, nil
		}
		before := plan[len(plan)-2]
		if before.Kind != mirror.KindPlanUsageRow || !before.FetchedAt.Equal(last.FetchedAt) {
			return false, nil
		}
		var row mirror.PlanUsageRowData
		if err := json.Unmarshal(before.Data, &row); err != nil {
			return false, fmt.Errorf("codex-cloud: chat %s: stored plan usage row: %w", id, err)
		}
		return row.Final, nil
	default:
		return false, nil
	}
}
