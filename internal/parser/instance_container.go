package parser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// InstanceContainerSpec describes how a meta-harness (such as T3 Code)
// isolates per-instance session roots beneath a single container directory.
//
// Supported layouts beneath a configured root:
//   - Direct session root:     <root>/<DirectMarker>...
//   - Single profile root:     <root>/<AppDataDir>/<DirectMarker>...
//   - Direct instance layout:  <root>/<instance>/<DirectMarker>...
//   - Nested instance layout:  <root>/<instance>/<AppDataDir>/<DirectMarker>...
type InstanceContainerSpec struct {
	Agent AgentType
	// DirectMarkers are directory names that directly identify a leaf session
	// root (for example, "conversations" and "implicit").
	DirectMarkers []string
	// AppDataDirs are optional per-profile application data directory names
	// (for example, "antigravity-acp" and "antigravity-cli").
	AppDataDirs []string
	// SessionRootEntries are top-level entry names (directories or files) that
	// live directly inside a leaf session root. InstanceRootForPath uses them
	// to map changed paths inside direct-instance containers in O(1) without
	// scanning sibling directories.
	SessionRootEntries []string
	// IsKnownContainer reports whether a configured root path matches a known
	// meta-harness container layout even before the directory exists or holds
	// any instances, so live watching activates when the first instance appears.
	IsKnownContainer func(string) bool
}

// ExpandRoot resolves a configured root into one or more concrete leaf session
// roots. When root is a container (known or detected from its child layout),
// isContainer is true and every non-symlink child instance root is returned in
// sorted order. Non-NotExist read failures are returned alongside any healthy
// expanded roots so callers can sync healthy instances while reporting
// incomplete discovery instead of tombstoning sessions from unreadable branches.
func (s InstanceContainerSpec) ExpandRoot(root string) (roots []string, isContainer bool, err error) {
	clean := filepath.Clean(root)
	if clean == "" || clean == "." {
		return nil, false, nil
	}
	for _, marker := range s.DirectMarkers {
		if info, statErr := os.Stat(filepath.Join(clean, marker)); statErr == nil && info.IsDir() {
			return []string{clean}, false, nil
		}
	}

	known := s.IsKnownContainer != nil && s.IsKnownContainer(clean)
	entries, readErr := os.ReadDir(clean)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			if known {
				return nil, true, nil
			}
			return []string{clean}, false, nil
		}
		if known {
			return nil, true, fmt.Errorf("expand %s container %s: %w", s.Agent, clean, readErr)
		}
		return []string{clean}, false, nil
	}

	var expanded []string
	var expandErr error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		child := filepath.Join(clean, name)

		// Single-profile root: <root>/<AppDataDir>/...
		if slices.Contains(s.AppDataDirs, name) {
			expanded = append(expanded, child)
			continue
		}

		// Nested-instance container: <root>/<instance>/<AppDataDir>/...
		foundAppData := false
		for _, appDir := range s.AppDataDirs {
			candidate := filepath.Join(child, appDir)
			ok, lstatErr := isNonSymlinkDir(candidate)
			if lstatErr != nil {
				expandErr = errors.Join(expandErr, fmt.Errorf("inspect %s instance %s: %w", s.Agent, candidate, lstatErr))
				continue
			}
			if ok {
				expanded = append(expanded, candidate)
				foundAppData = true
			}
		}
		if foundAppData {
			continue
		}

		// Direct-instance container: <root>/<instance>/<DirectMarker>/...
		for _, marker := range s.DirectMarkers {
			candidate := filepath.Join(child, marker)
			ok, lstatErr := isNonSymlinkDir(candidate)
			if lstatErr != nil {
				expandErr = errors.Join(expandErr, fmt.Errorf("inspect %s instance %s: %w", s.Agent, candidate, lstatErr))
				continue
			}
			if ok {
				expanded = append(expanded, child)
				break
			}
		}
	}

	if len(expanded) > 0 || expandErr != nil || known {
		slices.Sort(expanded)
		return expanded, true, expandErr
	}
	return []string{clean}, false, nil
}

// IsContainer reports whether root should be treated as a container of
// per-instance roots rather than a single direct session root.
func (s InstanceContainerSpec) IsContainer(root string) bool {
	_, isContainer, _ := s.ExpandRoot(root)
	return isContainer
}

// InstanceRootForPath maps a changed path beneath a container root directly to
// its owning leaf session root in O(1) using only path segments, without
// scanning sibling instances. It returns ("", false) when changedPath belongs
// directly to containerRoot as a leaf root rather than to a nested instance.
func (s InstanceContainerSpec) InstanceRootForPath(containerRoot, changedPath string) (string, bool) {
	cleanRoot := filepath.Clean(containerRoot)
	rel, ok := relUnder(cleanRoot, filepath.Clean(changedPath))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return "", false
	}
	// Single-profile root: <root>/<AppDataDir>/<entry>...
	if slices.Contains(s.AppDataDirs, parts[0]) {
		return filepath.Join(cleanRoot, parts[0]), true
	}
	// Nested-instance container: <root>/<instance>/<AppDataDir>/<entry>...
	if len(parts) >= 3 && slices.Contains(s.AppDataDirs, parts[1]) &&
		!slices.Contains(s.SessionRootEntries, parts[0]) {
		return filepath.Join(cleanRoot, parts[0], parts[1]), true
	}
	// Direct-instance container: <root>/<instance>/<sessionEntry>...
	if !slices.Contains(s.SessionRootEntries, parts[0]) &&
		slices.Contains(s.SessionRootEntries, parts[1]) {
		return filepath.Join(cleanRoot, parts[0]), true
	}
	return "", false
}

func isNonSymlinkDir(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	return info.IsDir(), nil
}
