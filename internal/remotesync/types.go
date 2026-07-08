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
// full-archive writer's per-agent sanitization, so callers fall back
// to the full-archive flow for these targets.
func (t TargetSet) HasFileScopedAgents() bool {
	return len(t.Files) > 0
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
