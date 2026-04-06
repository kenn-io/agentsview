package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func canonicalTestPath(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = filepath.Clean(resolved)
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(clean, "/private/") {
		publicPath := filepath.Clean(strings.TrimPrefix(clean, "/private"))
		if info, err := os.Stat(publicPath); err == nil && info.IsDir() {
			return publicPath
		}
	}
	return clean
}

func assertSamePath(t *testing.T, label, got, want string) {
	t.Helper()
	got = canonicalTestPath(got)
	want = canonicalTestPath(want)
	if got != want {
		t.Errorf("%s = %q, want %q", label, got, want)
	}
}

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
		assertSamePath(t, "cwd", resp.Cwd, projectDir)
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

	t.Run("claude desktop rejects non-claude agent", func(t *testing.T) {
		te.seedSession(t, "codex-desk", t.TempDir(), 3, func(s *db.Session) {
			s.Agent = "codex"
		})
		w := te.post(t,
			"/api/v1/sessions/codex-desk/resume",
			`{"opener_id":"claude-desktop"}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
	})

	t.Run("cursor command only", func(t *testing.T) {
		projectDir := t.TempDir()
		runDir := filepath.Join(projectDir, "frontend")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		te.seedSession(t, "cursor:chat-1", projectDir, 3, func(s *db.Session) {
			s.Agent = "cursor"
			s.FilePath = &sessionFile
		})
		w := te.post(t,
			"/api/v1/sessions/cursor:chat-1/resume",
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
		wantProjectDir := canonicalTestPath(projectDir)
		wantCmd := "cursor agent --resume chat-1 --workspace '" + wantProjectDir + "'"
		if resp.Command != wantCmd {
			t.Errorf("command = %q, want %q", resp.Command, wantCmd)
		}
		assertSamePath(t, "cwd", resp.Cwd, runDir)
	})

	t.Run("cursor command only falls back workspace to cwd", func(t *testing.T) {
		runDir := filepath.Join(t.TempDir(), "frontend")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		te.seedSession(t, "cursor:chat-2", "li_tools", 3, func(s *db.Session) {
			s.Agent = "cursor"
			s.FilePath = &sessionFile
		})
		w := te.post(t,
			"/api/v1/sessions/cursor:chat-2/resume",
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
		wantRunDir := canonicalTestPath(runDir)
		wantCmd := "cursor agent --resume chat-2 --workspace '" + wantRunDir + "'"
		if resp.Command != wantCmd {
			t.Errorf("command = %q, want %q", resp.Command, wantCmd)
		}
		assertSamePath(t, "cwd", resp.Cwd, runDir)
	})

	t.Run("unsupported agent", func(t *testing.T) {
		te.seedSession(t, "vscode-1", "/tmp", 3, func(s *db.Session) {
			s.Agent = "vscode-copilot"
		})
		w := te.post(t,
			"/api/v1/sessions/vscode-1/resume",
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
		assertSamePath(t, "path", resp.Path, projectDir)
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
		assertSamePath(t, "path", resp.Path, cwdDir)
	})

	t.Run("cursor directory returns workspace root", func(t *testing.T) {
		projectDir := t.TempDir()
		runDir := filepath.Join(projectDir, "frontend")
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		te.seedSession(t, "dir-cursor", projectDir, 3, func(s *db.Session) {
			s.Agent = "cursor"
			s.FilePath = &sessionFile
		})

		w := te.get(t, "/api/v1/sessions/dir-cursor/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		assertSamePath(t, "path", resp.Path, projectDir)
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
