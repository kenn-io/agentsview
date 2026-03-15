package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestResumeSession(t *testing.T) {
	te := setup(t)

	// Seed a claude session with an absolute project path.
	projectDir := t.TempDir()
	te.seedSession(t, "sess-1", projectDir, 5, func(s *db.Session) {
		s.Agent = "claude"
	})

	t.Run("command only", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/sessions/sess-1/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Launched {
			t.Error("expected launched=false for command_only")
		}
		if resp.Command == "" {
			t.Error("expected non-empty command")
		}
		if resp.Cwd != projectDir {
			t.Errorf("cwd = %q, want %q", resp.Cwd, projectDir)
		}
	})

	t.Run("not found", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/sessions/nonexistent/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusNotFound)
	})

	t.Run("copilot command only", func(t *testing.T) {
		projectDir := t.TempDir()
		// Use a prefixed ID to exercise the agent-prefix stripping
		// logic (e.g. "copilot:abc123" → raw ID "abc123").
		te.seedSession(t, "copilot:abc123", projectDir, 3, func(s *db.Session) {
			s.Agent = "copilot"
		})
		w := te.post(t,
			"/api/v1/sessions/copilot:abc123/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Launched {
			t.Error("expected launched=false for command_only")
		}
		wantCmd := "copilot --resume=abc123"
		if resp.Command != wantCmd {
			t.Errorf("command = %q, want %q", resp.Command, wantCmd)
		}
	})

	t.Run("unsupported agent", func(t *testing.T) {
		te.seedSession(t, "cursor-1", "/tmp", 3, func(s *db.Session) {
			s.Agent = "cursor"
		})
		w := te.post(t,
			"/api/v1/sessions/cursor-1/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
	})

	t.Run("deleted session rejected", func(t *testing.T) {
		te.seedSession(t, "del-1", "/tmp", 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		if err := te.db.SoftDeleteSession("del-1"); err != nil {
			t.Fatal(err)
		}
		w := te.post(t,
			"/api/v1/sessions/del-1/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusNotFound)
	})
}

func TestGetSessionDirectory(t *testing.T) {
	te := setup(t)

	projectDir := t.TempDir()
	te.seedSession(t, "dir-1", projectDir, 3)

	t.Run("returns resolved directory", func(t *testing.T) {
		w := te.get(t, "/api/v1/sessions/dir-1/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Path != projectDir {
			t.Errorf("path = %q, want %q", resp.Path, projectDir)
		}
	})

	t.Run("empty path for relative project", func(t *testing.T) {
		te.seedSession(t, "dir-2", "my-repo", 3)
		w := te.get(t, "/api/v1/sessions/dir-2/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Path != "" {
			t.Errorf("path = %q, want empty", resp.Path)
		}
	})

	t.Run("not found", func(t *testing.T) {
		w := te.get(t, "/api/v1/sessions/nonexistent/directory")
		assertStatus(t, w, http.StatusNotFound)
	})

	t.Run("prefers session file cwd", func(t *testing.T) {
		cwdDir := filepath.Join(t.TempDir(), "nested")
		if err := os.Mkdir(cwdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
		cwdJSON, _ := json.Marshal(cwdDir)
		content := `{"cwd":` + string(cwdJSON) + "}\n"
		if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		te.seedSession(t, "dir-3", projectDir, 3, func(s *db.Session) {
			s.FilePath = &sessionFile
		})
		w := te.get(t, "/api/v1/sessions/dir-3/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Path != cwdDir {
			t.Errorf("path = %q, want %q", resp.Path, cwdDir)
		}
	})
}

func TestListOpeners(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/openers")
	assertStatus(t, w, http.StatusOK)

	var resp struct {
		Openers []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			Bin  string `json:"bin"`
		} `json:"openers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The response should always be an array (possibly empty),
	// never null.
	if resp.Openers == nil {
		t.Error("openers should be [] not null")
	}
}

func TestGetTerminalConfig(t *testing.T) {
	te := setup(t)

	t.Run("default config", func(t *testing.T) {
		w := te.get(t, "/api/v1/config/terminal")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Mode != "auto" {
			t.Errorf("mode = %q, want %q", resp.Mode, "auto")
		}
	})

	t.Run("set and get", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/config/terminal",
			`{"mode":"clipboard"}`,
		)
		assertStatus(t, w, http.StatusOK)

		w = te.get(t, "/api/v1/config/terminal")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Mode != "clipboard" {
			t.Errorf("mode = %q, want %q", resp.Mode, "clipboard")
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/config/terminal",
			`{"mode":"invalid"}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
	})

	t.Run("custom requires bin", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/config/terminal",
			`{"mode":"custom","custom_bin":""}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
	})
}
