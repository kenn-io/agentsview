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
	return "", fmt.Errorf("codex-cloud: thread %s: parent chain longer than %d threads", threadID, maxParentDepth)
}

// LinkParent records child's resolved parent (empty for none) and keeps
// the usage groups in step. A parent that is not mirrored yet gets child
// in its waiting file, so child stays its own root until the parent
// arrives. Threads waiting for child move under child's root. The caller
// holds the account lock, writes a "counted under" record to each moved
// thread, and drops moved threads from usage retries.
//
// It returns child's root and every thread newly counted under a root
// other than itself, child's earlier descendants included.
func (s *Store) LinkParent(key AccountKey, child, parent string) (root string, moved []string, err error) {
	if parent == child {
		parent = ""
	}
	st, err := s.LoadThread(key, child)
	if err != nil {
		return "", nil, err
	}
	if st.ResolvedParent != parent {
		st.ResolvedParent = parent
		if err := s.SaveThread(key, child, st); err != nil {
			return "", nil, err
		}
	}
	if parent != "" {
		ok, err := s.known(key, parent)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			if err := s.addWaiting(key, parent, child); err != nil {
				return "", nil, err
			}
		}
	}
	root, err = s.RootOf(key, child)
	if err != nil {
		return "", nil, err
	}
	if root != child {
		group, err := s.takeGroup(key, child)
		if err != nil {
			return "", nil, err
		}
		added, err := s.addDescendants(key, root, group)
		if err != nil {
			return "", nil, err
		}
		moved = append(moved, added...)
	}
	waiting, err := s.LoadWaiting(key, child)
	if err != nil {
		return "", nil, err
	}
	for _, w := range waiting {
		// A thread whose parent changed since it began waiting is no
		// longer child's.
		wst, err := s.LoadThread(key, w)
		if err != nil {
			return "", nil, err
		}
		if wst.ResolvedParent != child {
			continue
		}
		group, err := s.takeGroup(key, w)
		if err != nil {
			return "", nil, err
		}
		added, err := s.addDescendants(key, root, group)
		if err != nil {
			return "", nil, err
		}
		moved = append(moved, added...)
	}
	if len(waiting) > 0 {
		if err := s.SaveWaiting(key, child, nil); err != nil {
			return "", nil, err
		}
	}
	return root, moved, nil
}

// takeGroup returns a former root and its descendants and removes its
// descendants file, since it is no longer a root.
func (s *Store) takeGroup(key AccountKey, former string) ([]string, error) {
	descendants, err := s.LoadDescendants(key, former)
	if err != nil {
		return nil, err
	}
	if err := s.SaveDescendants(key, former, nil); err != nil {
		return nil, err
	}
	return append([]string{former}, descendants...), nil
}

// addDescendants adds ids to root's group and returns those not already
// in it.
func (s *Store) addDescendants(key AccountKey, root string, ids []string) ([]string, error) {
	descendants, err := s.LoadDescendants(key, root)
	if err != nil {
		return nil, err
	}
	var added []string
	for _, id := range ids {
		if id != root && !slices.Contains(descendants, id) {
			descendants = append(descendants, id)
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, s.SaveDescendants(key, root, descendants)
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
