package sync

import (
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// roocodeEffectiveStat returns the composite size and latest mtime of
// a RooCode task's history_item.json and its ui_messages.json sibling
// using stat calls only. The values mirror what
// rooCodeFingerprintSource stamps on stored sessions (summed size,
// max mtime), so a stat-only comparison against the stored row is
// sufficient to detect any change to either file.
func roocodeEffectiveStat(historyPath string, info os.FileInfo) (int64, int64) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	msgPath := filepath.Join(filepath.Dir(historyPath), "ui_messages.json")
	if msgInfo, err := os.Stat(msgPath); err == nil && !msgInfo.IsDir() {
		size += msgInfo.Size()
		if ts := msgInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	return size, mtime
}

// clineEffectiveStat returns the composite size and latest mtime of
// a Cline session's <id>.json, its <id>.messages.json sibling, and any
// teammate *.messages.json files using stat calls only. The values mirror
// what clineFingerprintSource stamps on stored sessions (summed size, max mtime).
func clineEffectiveStat(metaPath string, info os.FileInfo) (int64, int64) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	dir := filepath.Dir(metaPath)
	sessionID := filepath.Base(dir)
	msgPath := filepath.Join(dir, sessionID+".messages.json")
	if msgInfo, err := os.Lstat(msgPath); err == nil && msgInfo.Mode().IsRegular() {
		size += msgInfo.Size()
		if ts := msgInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if parser.IsClineTeammateMessagesFile(sessionID, name) {
				teammatePath := filepath.Join(dir, name)
				if tInfo, err := os.Lstat(teammatePath); err == nil && tInfo.Mode().IsRegular() {
					size += tInfo.Size()
					if ts := tInfo.ModTime().UnixNano(); ts > mtime {
						mtime = ts
					}
				}
			}
		}
	}
	return size, mtime
}

// kiloLegacyEffectiveStat returns the composite size and latest mtime of
// a Kilo Legacy task's task_metadata.json, ui_messages.json, and
// api_conversation_history.json using stat calls only. The values mirror
// what kiloLegacyFingerprintSource stamps on stored sessions (summed
// size, max mtime), so a stat-only comparison against the stored row is
// sufficient to detect any change to any of the three files.
func kiloLegacyEffectiveStat(metadataPath string, info os.FileInfo) (int64, int64) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	dir := filepath.Dir(metadataPath)
	for _, name := range []string{
		"ui_messages.json",
		"api_conversation_history.json",
	} {
		sibPath := filepath.Join(dir, name)
		sibInfo, err := os.Stat(sibPath)
		if err != nil || sibInfo.IsDir() {
			continue
		}
		size += sibInfo.Size()
		if ts := sibInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	return size, mtime
}

// copilotEffectiveMtime returns max(events.jsonl mtime,
// workspace.yaml mtime). For flat .jsonl sessions (no
// workspace.yaml sibling) it returns the events.jsonl mtime.
func copilotEffectiveMtime(eventsPath string, info os.FileInfo) int64 {
	m := info.ModTime().UnixNano()
	if filepath.Base(eventsPath) != "events.jsonl" {
		return m
	}
	yamlPath := filepath.Join(
		filepath.Dir(eventsPath), "workspace.yaml",
	)
	if yi, err := os.Stat(yamlPath); err == nil {
		if ym := yi.ModTime().UnixNano(); ym > m {
			m = ym
		}
	}
	return m
}

func reasonixEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := path + ".meta"
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
}

// vibeEffectiveInfo returns size/mtime for a Vibe session that account
// for the sibling meta.json file: size is the sum of both files, and
// mtime is the larger of the two. Returns info unchanged when meta.json
// is absent or unreadable.
func vibeEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	siblings := []string{filepath.Join(filepath.Dir(path), "meta.json")}
	// A unified session's running turn is appended to its journal segment.
	if journal, ok := parser.VibeUnifiedJournalPath(path); ok {
		siblings = append(siblings, journal)
	}
	for _, sibling := range siblings {
		if siblingInfo, err := os.Stat(sibling); err == nil {
			size += siblingInfo.Size()
			mtime = max(mtime, siblingInfo.ModTime().UnixNano())
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
}

func commandCodeEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
}
