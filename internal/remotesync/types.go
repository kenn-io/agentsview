package remotesync

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type SyncStats struct {
	SessionsSynced int `json:"sessions_synced"`
	SessionsTotal  int `json:"sessions_total"`
	Skipped        int `json:"skipped"`
	Failed         int `json:"failed"`
}

type TargetSet struct {
	Dirs       map[parser.AgentType][]string `json:"dirs"`
	Files      map[parser.AgentType][]string `json:"files,omitempty"`
	ExtraFiles []string                      `json:"extra_files,omitempty"`
}

// HasFileScopedAgents reports whether any agent exports a curated
// file list rather than a raw directory walk.
func (t TargetSet) HasFileScopedAgents() bool {
	return len(t.Files) > 0
}

// verbatimFileScopedAgent reports whether a file-scoped agent's
// curated files are exported byte-for-byte by WriteArchive. Verbatim
// agents (RooCode) can ride the manifest/delta path: the manifest
// advertises exactly the files the archive streams, so one changed
// transcript transfers alone. Sanitizing agents (Windsurf rewrites
// its state DB) must stay on the full-archive flow, and new
// file-scoped agents default to sanitized until added here.
func verbatimFileScopedAgent(agent parser.AgentType) bool {
	return agent == parser.AgentRooCode
}

// HasSanitizedFileScopedAgents reports whether any agent's export is
// file-scoped and transformed relative to the on-disk tree, which the
// manifest/delta path cannot model.
func (t TargetSet) HasSanitizedFileScopedAgents() bool {
	for agent := range t.Files {
		if !verbatimFileScopedAgent(agent) {
			return true
		}
	}
	return false
}

// IsEmpty reports whether the set names no sync targets at all.
func (t TargetSet) IsEmpty() bool {
	return len(t.Dirs) == 0 && len(t.Files) == 0 && len(t.ExtraFiles) == 0
}

// SplitFileScoped partitions the set into the targets the
// manifest/delta path can model and the sanitized file-scoped agents
// (Windsurf) whose exports differ from the on-disk tree. The
// dir-scoped half — including verbatim file-scoped agents like
// RooCode, whose curated files the manifest advertises directly —
// syncs incrementally via the mirror delta; the sanitized half is
// fetched as a separate small full archive every sync.
func (t TargetSet) SplitFileScoped() (dirScoped, fileScoped TargetSet) {
	for agent, dirs := range t.Dirs {
		if _, ok := t.Files[agent]; ok && !verbatimFileScopedAgent(agent) {
			if fileScoped.Dirs == nil {
				fileScoped.Dirs = make(map[parser.AgentType][]string)
			}
			fileScoped.Dirs[agent] = dirs
			continue
		}
		if dirScoped.Dirs == nil {
			dirScoped.Dirs = make(map[parser.AgentType][]string)
		}
		dirScoped.Dirs[agent] = dirs
	}
	for agent, files := range t.Files {
		target := &fileScoped
		if verbatimFileScopedAgent(agent) {
			target = &dirScoped
		}
		if target.Files == nil {
			target.Files = make(map[parser.AgentType][]string)
		}
		target.Files[agent] = files
	}
	dirScoped.ExtraFiles = t.ExtraFiles
	return dirScoped, fileScoped
}

// DeltaAllowedRoots returns the trusted base paths a delta-archive file
// may resolve under: every non-file-scoped agent directory, the
// verbatim file-scoped agents' curated files (exact matches only —
// their raw directory is never a prefix root, so settings and caches
// stay unreachable), plus the extra files. Sanitized file-scoped
// agents (Windsurf) contribute nothing because their raw tree is
// never delta-streamed. WriteArchiveFiles uses these roots while
// retaining the TargetSet's agent ownership information.
func (t TargetSet) DeltaAllowedRoots() []string {
	roots := make([]string, 0, len(t.Dirs)+len(t.Files)+len(t.ExtraFiles))
	for agent, dirs := range t.Dirs {
		if _, fileScoped := t.Files[agent]; fileScoped {
			continue
		}
		roots = append(roots, dirs...)
	}
	for agent, files := range t.Files {
		if verbatimFileScopedAgent(agent) {
			roots = append(roots, files...)
		}
	}
	roots = append(roots, t.ExtraFiles...)
	return roots
}

// ArchiveRequest is the archive endpoint's request body. DeltaFiles,
// when present, selects delta mode: only the named files are streamed
// (validated by SelectAllowedFiles). Old servers ignore the unknown
// field and return the full tree, which is why clients only send
// DeltaFiles after a successful manifest probe.
type ArchiveRequest struct {
	TargetSet
	DeltaFiles []string `json:"delta_files,omitempty"`
}

func (r ArchiveRequest) MarshalJSON() ([]byte, error) {
	out := make(map[string]any)
	if r.Dirs != nil {
		out["dirs"] = r.Dirs
	}
	if r.Files != nil {
		out["files"] = r.Files
	}
	if len(r.ExtraFiles) > 0 {
		out["extra_files"] = r.ExtraFiles
	}
	if r.DeltaFiles != nil {
		out["delta_files"] = r.DeltaFiles
	}
	return json.Marshal(out)
}

func (r *ArchiveRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		Dirs       map[parser.AgentType][]string `json:"dirs"`
		Files      json.RawMessage               `json:"files"`
		ExtraFiles []string                      `json:"extra_files"`
		DeltaFiles []string                      `json:"delta_files"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.TargetSet = TargetSet{
		Dirs:       raw.Dirs,
		ExtraFiles: raw.ExtraFiles,
	}
	r.DeltaFiles = raw.DeltaFiles
	if len(raw.Files) == 0 {
		return nil
	}
	files := bytes.TrimSpace(raw.Files)
	if bytes.Equal(files, []byte("null")) {
		return nil
	}
	switch files[0] {
	case '{':
		return json.Unmarshal(files, &r.Files)
	case '[':
		if raw.DeltaFiles != nil {
			return fmt.Errorf("archive request cannot use both files delta list and delta_files")
		}
		return json.Unmarshal(files, &r.DeltaFiles)
	default:
		return fmt.Errorf("archive request files must be an object or array")
	}
}

type Importer struct {
	Host                    string
	Full                    bool
	DB                      *db.DB
	BlockedResultCategories []string
	Progress                syncpkg.ProgressFunc
}
