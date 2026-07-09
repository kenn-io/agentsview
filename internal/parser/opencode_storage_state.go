// ABOUTME: Change-detection state for file-backed OpenCode storage sessions,
// ABOUTME: built from the stat signature of every file the parser would read.
package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StatOpenCodeStorageSessionState captures a change-detection state for a
// file-backed OpenCode-format storage session: a digest over the name, size,
// and nanosecond mtime of the session JSON and of every message and part
// file its parse would read (the same .json filters as
// loadOpenCodeStorageMessages and loadOpenCodeStorageParts). Two equal
// states mean the parse inputs carry an identical stat signature, so a
// session whose state matches a previously verified one can skip re-parsing.
//
// The signature is deliberately per-file rather than an aggregate max-mtime:
// a composite max cannot see a child rewritten within one filesystem mtime
// granule or with a restored timestamp, while a per-file (name, size,
// mtimeNS) set narrows the blind spot to a same-size in-place rewrite that
// also preserves the file's mtime. Callers that learn about a change out of
// band (a watcher event naming a child file) must invalidate any trust in
// the previous state rather than rely on the stat signature alone.
//
// ok is false when the session file is missing or a directory listing
// fails, in which case the session must never be treated as unchanged.
func StatOpenCodeStorageSessionState(sessionPath string) (string, bool) {
	info, err := os.Stat(sessionPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	h := sha256.New()
	record := func(kind, name string, size, mtimeNS int64) {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", kind, name, size, mtimeNS)
	}
	record(
		"session", filepath.Base(sessionPath),
		info.Size(), info.ModTime().UnixNano(),
	)

	root := filepath.Dir(filepath.Dir(filepath.Dir(
		filepath.Dir(sessionPath),
	)))
	sessionID := strings.TrimSuffix(
		filepath.Base(sessionPath), filepath.Ext(sessionPath),
	)
	messageDir := filepath.Join(root, "storage", "message", sessionID)
	msgEntries, err := os.ReadDir(messageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("%x", h.Sum(nil)), true
		}
		return "", false
	}
	for _, entry := range msgEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		msgInfo, err := entry.Info()
		if err != nil {
			return "", false
		}
		record(
			"message", entry.Name(),
			msgInfo.Size(), msgInfo.ModTime().UnixNano(),
		)
		messageID := strings.TrimSuffix(
			entry.Name(), filepath.Ext(entry.Name()),
		)
		partDir := filepath.Join(root, "storage", "part", messageID)
		partEntries, err := os.ReadDir(partDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false
		}
		for _, partEntry := range partEntries {
			if partEntry.IsDir() ||
				!strings.HasSuffix(partEntry.Name(), ".json") {
				continue
			}
			partInfo, err := partEntry.Info()
			if err != nil {
				return "", false
			}
			record(
				"part", messageID+"/"+partEntry.Name(),
				partInfo.Size(), partInfo.ModTime().UnixNano(),
			)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), true
}
