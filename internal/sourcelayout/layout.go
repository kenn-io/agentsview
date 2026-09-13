// Package sourcelayout decides whether an archived session's source
// can be safely moved to the trash, per agent storage layout.
//
// Three layout families exist:
//
//   - plain files: one transcript file per session (Claude, Codex,
//     Gemini, ...). The file is deletable.
//   - opencode file tree: a session JSON plus its message and part
//     directories under storage/. All of them are deletable together.
//   - app-owned stores: Trae keeps sessions inside its own
//     state.vscdb and OpenCode's SQLite container keeps rows inside
//     opencode.db. Mutating those stores from the outside risks
//     corrupting application state, so they are report-only.
package sourcelayout

import (
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ReportOnlyReason is the stable string surfaced to CLI and web
// callers when a session's source cannot be trashed.
const ReportOnlyReason = "unsupported app-owned layout (report-only)"

// Decision reports whether the session's source may be trashed and,
// when it may, the full set of paths to move together.
type Decision struct {
	Deletable bool
	Reason    string
	Paths     []string
}

// Layout answers the deletability question for one session.
type Layout interface {
	Decide(sess db.Session) Decision
}

// For returns the layout for an agent key. Unknown agents fall back
// to the plain-file layout, which is conservative: it only ever
// touches the recorded transcript file.
func For(agent string) Layout {
	switch agent {
	case "opencode":
		return openCodeTree{}
	case "traex":
		return reportOnly{}
	default:
		return fileLayout{}
	}
}

// fileLayout trashes the recorded transcript file when it exists.
type fileLayout struct{}

func (fileLayout) Decide(sess db.Session) Decision {
	path := derefPath(sess.FilePath)
	if path == "" {
		return Decision{Reason: "no source file recorded"}
	}
	if _, err := os.Stat(path); err != nil {
		return Decision{Reason: "source file missing"}
	}
	return Decision{Deletable: true, Paths: []string{path}}
}

// openCodeTree trashes the session JSON together with its message
// directory and every per-message part directory.
type openCodeTree struct{}

func (openCodeTree) Decide(sess db.Session) Decision {
	sessionPath := derefPath(sess.FilePath)
	if sessionPath == "" {
		return Decision{Reason: "no source file recorded"}
	}
	if isContainerPath(sessionPath) {
		return Decision{Reason: ReportOnlyReason}
	}
	// storage/session/<project>/<session>.json under the opencode
	// root; the session id is the file stem.
	base := filepath.Base(sessionPath)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))
	root := filepath.Dir(filepath.Dir(filepath.Dir(sessionPath)))
	paths := []string{sessionPath}

	messageDir := filepath.Join(root, "storage", "message", sessionID)
	entries, err := os.ReadDir(messageDir)
	if err == nil {
		paths = append(paths, messageDir)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			messageID := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			partDir := filepath.Join(root, "storage", "part", messageID)
			if info, statErr := os.Stat(partDir); statErr == nil && info.IsDir() {
				paths = append(paths, partDir)
			}
		}
	}
	return Decision{Deletable: true, Paths: paths}
}

// reportOnly never deletes: the source lives inside an application
// database that agentsview does not own.
type reportOnly struct{}

func (reportOnly) Decide(db.Session) Decision {
	return Decision{Reason: ReportOnlyReason}
}

// isContainerPath reports whether a recorded source path points at a
// SQLite container (opencode.db) rather than a session file.
func isContainerPath(path string) bool {
	return strings.HasSuffix(path, ".db") ||
		strings.HasSuffix(path, ".db-wal") ||
		strings.HasSuffix(path, ".vscdb")
}

func derefPath(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
