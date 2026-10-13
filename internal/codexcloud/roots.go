package codexcloud

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"
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
