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

// HasFileScopedAgents reports whether any agent exports a curated,
// possibly sanitized file list (Windsurf) rather than a raw directory
// walk. The manifest/delta incremental path has no way to model the
// full-archive writer's per-agent sanitization, so these targets only
// ever travel through the full-archive flow.
func (t TargetSet) HasFileScopedAgents() bool {
	return len(t.Files) > 0
}

// IsEmpty reports whether the set names no sync targets at all.
func (t TargetSet) IsEmpty() bool {
	return len(t.Dirs) == 0 && len(t.Files) == 0 && len(t.ExtraFiles) == 0
}

// SplitFileScoped partitions the set into the targets the
// manifest/delta path can model (raw directory walks plus extra files)
// and the file-scoped agents whose exports are curated and possibly
// sanitized (Windsurf). The dir-scoped half syncs incrementally via
// the mirror delta; the file-scoped half is fetched as a separate
// small full archive every sync, so a host with Windsurf sessions no
// longer drags its whole corpus onto the full-archive path.
func (t TargetSet) SplitFileScoped() (dirScoped, fileScoped TargetSet) {
	for agent, dirs := range t.Dirs {
		if _, ok := t.Files[agent]; ok {
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
	fileScoped.Files = t.Files
	dirScoped.ExtraFiles = t.ExtraFiles
	return dirScoped, fileScoped
}

// DeltaAllowedRoots returns the trusted base paths a delta-archive file
// may resolve under: every non-file-scoped agent directory plus the
// extra files. File-scoped agents (Windsurf) are excluded because
// their raw tree is never delta-streamed. WriteArchiveFiles re-checks
// each requested file against these roots before reading it.
func (t TargetSet) DeltaAllowedRoots() []string {
	roots := make([]string, 0, len(t.Dirs)+len(t.ExtraFiles))
	for agent, dirs := range t.Dirs {
		if _, fileScoped := t.Files[agent]; fileScoped {
			continue
		}
		roots = append(roots, dirs...)
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
