package server_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

type failingResumeModelCountsStore struct {
	readOnlyTestStore
}

func (failingResumeModelCountsStore) GetResumeModelCounts(
	context.Context, string,
) ([]db.ModelCount, error) {
	return nil, errors.New("boom")
}

type resumeCountsOnlyStore struct {
	readOnlyTestStore
}

func (resumeCountsOnlyStore) GetAllMessages(
	context.Context, string,
) ([]db.Message, error) {
	return nil, errors.New("unexpected GetAllMessages call")
}

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
	if got == want {
		return
	}
	gotInfo, gotErr := os.Stat(got)
	wantInfo, wantErr := os.Stat(want)
	if gotErr == nil && wantErr == nil && os.SameFile(gotInfo, wantInfo) {
		return
	}
	assert.Fail(t, "path mismatch", "%s = %q, want %q", label, got, want)
}

func messagePointPromptGlob(t *testing.T, sessionID string, ordinal int) string {
	t.Helper()
	cacheDir, err := os.UserCacheDir()
	require.NoError(t, err)
	return filepath.Join(
		cacheDir,
		"agentsview",
		"claude-message-points",
		fmt.Sprintf("%s-ordinal-%d-*.txt", sessionID, ordinal),
	)
}

func removeMessagePointPrompts(t *testing.T, sessionID string, ordinal int) {
	t.Helper()
	matches, err := filepath.Glob(
		messagePointPromptGlob(t, sessionID, ordinal),
	)
	require.NoError(t, err)
	for _, match := range matches {
		_ = os.Remove(match)
	}
}

func findSingleMessagePointPrompt(
	t *testing.T, sessionID string, ordinal int,
) string {
	t.Helper()
	matches, err := filepath.Glob(
		messagePointPromptGlob(t, sessionID, ordinal),
	)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	return matches[0]
}

func assertNoMessagePointPrompts(
	t *testing.T, sessionID string, ordinal int,
) {
	t.Helper()
	matches, err := filepath.Glob(
		messagePointPromptGlob(t, sessionID, ordinal),
	)
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func assertMessagePointCommandForRuntime(
	t *testing.T, command string, promptPath string,
) {
	t.Helper()

	if runtime.GOOS == "windows" {
		script := decodeMessagePointPowerShellCommandForTest(t, command)
		quotedPromptPath := powerShellSingleQuoteForTest(promptPath)
		assert.Contains(t, script,
			"Get-Content -Raw -Encoding UTF8 -LiteralPath "+
				quotedPromptPath)
		assert.Contains(t, script,
			"Remove-Item -LiteralPath "+quotedPromptPath+
				" -Force -ErrorAction SilentlyContinue")
		assert.NotContains(t, command, " < ")
		assert.NotContains(t, command, "rm -f --")
		assert.NotContains(t, script, " < ")
		assert.NotContains(t, script, "rm -f --")
		return
	}
	assert.Contains(t, command, "claude <")
	assert.Contains(t, command, "rm -f --")
}

func decodeMessagePointPowerShellCommandForTest(
	t *testing.T, command string,
) string {
	t.Helper()

	const prefix = "powershell.exe -NoProfile -EncodedCommand "
	require.True(t, strings.HasPrefix(command, prefix), "command = %q", command)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
	require.NoError(t, err)
	require.Zero(t, len(raw)%2, "UTF-16LE byte length must be even")

	codeUnits := make([]uint16, len(raw)/2)
	for i := range codeUnits {
		codeUnits[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(codeUnits))
}

func powerShellSingleQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestResumeRemoteCommandOnly(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "devbox1~claude:abc-123", "remote-project", 1, func(s *db.Session) {
		s.Agent = "claude"
		s.Cwd = "/home/user/project"
	})
	w := te.post(t, "/api/v1/sessions/devbox1~claude:abc-123/resume", `{"command_only":true}`)
	t.Logf("status=%d body=%s", w.Code, w.Body.String())
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Launched bool   `json:"launched"`
		Command  string `json:"command"`
		Cwd      string `json:"cwd"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Launched)
	assert.Equal(t, "cd '/home/user/project' && claude --resume abc-123", resp.Command)
	assert.Equal(t, "/home/user/project", resp.Cwd)
	assert.NotContains(t, resp.Command, "~")
}

func TestResumeSession(t *testing.T) {
	te := setup(t)

	t.Run("remote launch guard", func(t *testing.T) {
		te.seedSession(t, "devbox1~claude:guard", "remote", 1, func(s *db.Session) { s.Agent = "claude" })
		for _, body := range []string{`{}`, `{"command_only":false}`, `{"opener_id":"claude-desktop"}`, `{"opener_id":"missing-terminal"}`, `{"command_only":true,"fork_session":true,"from_ordinal":0}`} {
			w := te.post(t, "/api/v1/sessions/devbox1~claude:guard/resume", body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.JSONEq(t, `{"error":"cannot resume remote session"}`, w.Body.String())
		}
	})

	t.Run("remote unsupported agent", func(t *testing.T) {
		te.seedSession(t, "devbox1~unsupported", "remote", 1, func(s *db.Session) { s.Agent = "vscode-copilot" })
		w := te.post(t, "/api/v1/sessions/devbox1~unsupported/resume", `{"command_only":true}`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.JSONEq(t, `{"error":"agent \"vscode-copilot\" does not support resume"}`, w.Body.String())
	})

	t.Run("remote deleted row precedes guard", func(t *testing.T) {
		te.seedSession(t, "devbox1~deleted", "remote", 1)
		require.NoError(t, te.db.SoftDeleteSession(t.Context(), "devbox1~deleted"))
		for _, body := range []string{`{}`, `{"command_only":true}`} {
			w := te.post(t, "/api/v1/sessions/devbox1~deleted/resume", body)
			assert.Equal(t, http.StatusNotFound, w.Code)
		}
	})

	t.Run("local namespace false positive", func(t *testing.T) {
		te.seedSession(t, "codex:local-id", "remote-project", 1, func(s *db.Session) {
			s.Agent = "codex"
			s.Machine = "devbox1~remote"
		})
		assertStatus(t, te.post(t, "/api/v1/config/terminal", `{"mode":"clipboard"}`), http.StatusOK)
		for _, body := range []string{`{}`, `{"command_only":true}`} {
			w := te.post(t, "/api/v1/sessions/codex:local-id/resume", body)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, `{"launched":false,"command":"codex resume local-id"}`, w.Body.String())
		}
	})

	// Seed a claude session with an absolute project path.
	projectDir := t.TempDir()
	te.seedSession(t, "sess-1", projectDir, 5, func(s *db.Session) {
		s.Agent = "claude"
	})

	t.Run("claude_recorded_model", func(t *testing.T) {
		te.seedSession(t, "claude-model", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "claude-model", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "claude sonnet"
			}
		})
		w := te.post(t, "/api/v1/sessions/claude-model/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(t, resp.Command, "claude --resume claude-model --model 'claude sonnet'")
	})

	t.Run("claude_recorded_model_shell_quoted", func(t *testing.T) {
		te.seedSession(t, "claude-model-quoted", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "claude-model-quoted", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "x'$(command)"
			}
		})
		w := te.post(t, "/api/v1/sessions/claude-model-quoted/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(
			t,
			resp.Command,
			`claude --resume claude-model-quoted --model 'x'"'"'$(command)'`,
		)
	})

	t.Run("codex_recorded_model", func(t *testing.T) {
		te.seedSession(t, "codex-model", projectDir, 3, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "codex-model", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "o3-mini"
			}
		})
		w := te.post(t, "/api/v1/sessions/codex-model/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume codex-model -m o3-mini", resp.Command)
	})

	t.Run("codex_recorded_model_shell_quoted", func(t *testing.T) {
		te.seedSession(t, "codex-model-quoted", projectDir, 3, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "codex-model-quoted", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "x'$(command)"
			}
		})
		w := te.post(t, "/api/v1/sessions/codex-model-quoted/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(
			t,
			resp.Command,
			`codex resume codex-model-quoted -m 'x'"'"'$(command)'`,
		)
	})

	t.Run("mixed_model", func(t *testing.T) {
		te.seedSession(t, "mixed-model", projectDir, 5, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "mixed-model", 5, func(i int, m *db.Message) {
			switch i {
			case 1:
				m.Model = "mixed-model-tie-z"
			case 3:
				m.Model = "mixed-model-tie-a"
			}
		})
		w := te.post(t, "/api/v1/sessions/mixed-model/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(t, resp.Command, "claude --resume mixed-model --model mixed-model-tie-a")
	})

	t.Run("no_recorded_model", func(t *testing.T) {
		w := te.post(t, "/api/v1/sessions/sess-1/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Contains(t, resp.Command, "claude --resume sess-1")
		assert.NotContains(t, resp.Command, "--model")
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		assert.NotEmpty(t, resp.Command)
		assertSamePath(t, "cwd", resp.Cwd, projectDir)
	})

	t.Run("fork session command only", func(t *testing.T) {
		w := te.post(t,
			"/api/v1/sessions/sess-1/resume",
			`{"command_only":true,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		assert.Contains(t, resp.Command, "claude --resume sess-1 --fork-session")
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		assert.Equal(t, "copilot --resume=abc123", resp.Command)
	})

	t.Run("copilot ignores model-bearing messages", func(t *testing.T) {
		projectDir := t.TempDir()
		te.seedSession(t, "copilot:model-bearing", projectDir, 3, func(s *db.Session) {
			s.Agent = "copilot"
		})
		te.seedMessages(t, "copilot:model-bearing", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Role = "assistant"
				m.Model = "model-bearing-non-target"
			}
		})
		w := te.post(t,
			"/api/v1/sessions/copilot:model-bearing/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		assert.Equal(t, "copilot --resume=model-bearing", resp.Command)
	})

	t.Run("kiro current-store command only", func(t *testing.T) {
		projectDir := t.TempDir()
		te.seedSession(t, "kiro:sqlite-chat", "kiro_app", 3, func(s *db.Session) {
			s.Agent = "kiro"
			s.Cwd = projectDir
		})
		w := te.post(t,
			"/api/v1/sessions/kiro:sqlite-chat/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		const cmdSuffix = "' && kiro-cli chat --resume-id sqlite-chat"
		if !strings.HasPrefix(resp.Command, "cd '") ||
			!strings.HasSuffix(resp.Command, cmdSuffix) {
			assert.Fail(t, "command shape mismatch",
				"command = %q, want cd command ending with %q",
				resp.Command, cmdSuffix)
		} else {
			commandCwd := strings.TrimSuffix(
				strings.TrimPrefix(resp.Command, "cd '"),
				cmdSuffix,
			)
			assertSamePath(t, "command cwd", commandCwd, projectDir)
		}
		assertSamePath(t, "cwd", resp.Cwd, projectDir)
	})

	t.Run("pi_command_only", func(t *testing.T) {
		projectDir := filepath.Join(t.TempDir(), "project~1")
		require.NoError(t, os.Mkdir(projectDir, 0o755))
		v1Path := filepath.Join(projectDir, "2025-01-01T09-00-00-000Z_parent-uuid.jsonl")
		remotePath := "/home/user/.pi/agent/sessions/session-1.jsonl"
		remoteV1Path := "/home/user/.pi/agent/sessions/2025-01-01T09-00-00-000Z_parent-uuid.jsonl"
		te.seedSession(t, "pi:session-1", "pi-project", 3, func(s *db.Session) {
			s.Agent = "pi"
			s.Cwd = projectDir
		})
		te.seedSession(t, "pi:$(whoami)", "pi-project", 3, func(s *db.Session) {
			s.Agent = "pi"
			s.Cwd = projectDir
		})
		te.seedSession(t, "devbox1~pi:session-1", "remote-project", 3, func(s *db.Session) {
			s.Agent = "pi"
			s.Cwd = "/home/user/project"
			storedPath := "devbox1:" + remotePath
			s.FilePath = &storedPath
		})
		te.seedSession(t, "devbox1~pi:2025-01-01T09-00-00-000Z_parent-uuid", "remote-project", 3, func(s *db.Session) {
			s.Agent = "pi"
			s.Cwd = "/home/user/project"
			storedPath := "devbox1:" + remoteV1Path
			s.FilePath = &storedPath
		})
		te.seedSession(t, "pi:2025-01-01T09-00-00-000Z_parent-uuid", "pi-project", 3, func(s *db.Session) {
			s.Agent = "pi"
			s.Cwd = projectDir
			s.FilePath = &v1Path
		})

		for _, tt := range []struct {
			name       string
			id         string
			wantCwd    string
			wantSuffix string
		}{
			{
				name:       "local session",
				id:         "pi:session-1",
				wantCwd:    projectDir,
				wantSuffix: "pi --session session-1",
			},
			{
				name:       "local shell metacharacter",
				id:         "pi:$(whoami)",
				wantCwd:    projectDir,
				wantSuffix: "pi --session '$(whoami)'",
			},
			{
				name:       "remote session",
				id:         "devbox1~pi:session-1",
				wantCwd:    "/home/user/project",
				wantSuffix: "pi --session '" + remotePath + "'",
			},
			{
				name:       "remote v1 session strips storage host",
				id:         "devbox1~pi:2025-01-01T09-00-00-000Z_parent-uuid",
				wantCwd:    "/home/user/project",
				wantSuffix: "pi --session '" + remoteV1Path + "'",
			},
			{
				name:       "v1 session uses file path",
				id:         "pi:2025-01-01T09-00-00-000Z_parent-uuid",
				wantCwd:    projectDir,
				wantSuffix: "pi --session '" + v1Path + "'",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				w := te.post(t,
					"/api/v1/sessions/"+tt.id+"/resume",
					`{"command_only":true}`,
				)
				t.Logf("id=%s command=%s", tt.id, w.Body.String())
				assertStatus(t, w, http.StatusOK)
				var resp struct {
					Launched bool   `json:"launched"`
					Command  string `json:"command"`
					Cwd      string `json:"cwd"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				assert.False(t, resp.Launched, "expected launched=false for command_only")
				assert.Equal(t, "cd '"+tt.wantCwd+"' && "+tt.wantSuffix, resp.Command)
				assert.Equal(t, tt.wantCwd, resp.Cwd)
			})
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
		require.NoError(t, os.MkdirAll(runDir, 0o755))
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		wantProjectDir := canonicalTestPath(projectDir)
		assert.Equal(t, "cursor agent --resume chat-1 --workspace '"+wantProjectDir+"'",
			resp.Command)
		assertSamePath(t, "cwd", resp.Cwd, runDir)
	})

	t.Run("cursor command only omits unresolved workspace", func(t *testing.T) {
		runDir := filepath.Join(t.TempDir(), "frontend")
		require.NoError(t, os.MkdirAll(runDir, 0o755))
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		assert.Equal(t, "cursor agent --resume chat-2", resp.Command)
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

	t.Run("message point command only", func(t *testing.T) {
		removeMessagePointPrompts(t, "sess-2", 1)

		te.seedSession(t, "sess-2", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-2", 3)

		w := te.post(t,
			"/api/v1/sessions/sess-2/resume",
			`{"command_only":true,"from_ordinal":1,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		promptPath := findSingleMessagePointPrompt(t, "sess-2", 1)
		assertMessagePointCommandForRuntime(t, resp.Command, promptPath)
		if runtime.GOOS != "windows" {
			assert.Contains(t, resp.Command, "< '")
		}
		assertSamePath(t, "cwd", resp.Cwd, projectDir)

		if runtime.GOOS != "windows" {
			idx := strings.LastIndex(resp.Command, "< ")
			require.Positive(t, idx, "command = %q", resp.Command)
			extracted := strings.TrimSpace(resp.Command[idx+2:])
			if semi := strings.Index(extracted, ";"); semi >= 0 {
				extracted = strings.TrimSpace(extracted[:semi])
			}
			extracted = strings.TrimPrefix(extracted, "'")
			extracted = strings.TrimSuffix(extracted, "'")
			assertSamePath(t, "prompt file", extracted, promptPath)
		}

		data, err := os.ReadFile(promptPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Remove(promptPath) })
		text := string(data)
		assert.Contains(t, text, "Message A")
		assert.Contains(t, text, "Message B")
		assert.NotContains(t, text, "Message C")
	})

	t.Run("message point command only finds sparse ordinals", func(t *testing.T) {
		removeMessagePointPrompts(t, "sess-sparse", 3)

		te.seedSession(t, "sess-sparse", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-sparse", 3, func(i int, m *db.Message) {
			if i == 2 {
				m.Ordinal = 3
				m.Content = "Message D"
			}
		})

		w := te.post(t,
			"/api/v1/sessions/sess-sparse/resume",
			`{"command_only":true,"from_ordinal":3,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched, "expected launched=false for command_only")
		promptPath := findSingleMessagePointPrompt(t, "sess-sparse", 3)
		assertMessagePointCommandForRuntime(t, resp.Command, promptPath)
		assertSamePath(t, "cwd", resp.Cwd, projectDir)
		t.Cleanup(func() { _ = os.Remove(promptPath) })

		data, err := os.ReadFile(promptPath)
		require.NoError(t, err)
		text := string(data)
		assert.Contains(t, text, "Message A")
		assert.Contains(t, text, "Message B")
		assert.Contains(t, text, "Message D")
	})

	t.Run("message point rejects unsupported agents", func(t *testing.T) {
		removeMessagePointPrompts(t, "codex-desk", 0)

		te.seedSession(t, "codex-desk", t.TempDir(), 3, func(s *db.Session) {
			s.Agent = "codex"
		})
		w := te.post(t,
			"/api/v1/sessions/codex-desk/resume",
			`{"command_only":true,"from_ordinal":0,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
		assertNoMessagePointPrompts(t, "codex-desk", 0)
	})

	t.Run("message point requires fork session", func(t *testing.T) {
		removeMessagePointPrompts(t, "sess-need-fork", 0)

		te.seedSession(t, "sess-need-fork", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-need-fork", 3)

		w := te.post(t,
			"/api/v1/sessions/sess-need-fork/resume",
			`{"command_only":true,"from_ordinal":0}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
		assertNoMessagePointPrompts(t, "sess-need-fork", 0)
	})

	t.Run("message point rejects opener id", func(t *testing.T) {
		removeMessagePointPrompts(t, "sess-opener", 0)

		te.seedSession(t, "sess-opener", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-opener", 3)

		w := te.post(t,
			"/api/v1/sessions/sess-opener/resume",
			`{"command_only":true,"from_ordinal":0,"fork_session":true,"opener_id":"claude-desktop"}`,
		)
		assertStatus(t, w, http.StatusBadRequest)
		assertNoMessagePointPrompts(t, "sess-opener", 0)
	})

	t.Run("message point rejects missing ordinals", func(t *testing.T) {
		removeMessagePointPrompts(t, "sess-3", 99)

		te.seedSession(t, "sess-3", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-3", 3)
		w := te.post(t,
			"/api/v1/sessions/sess-3/resume",
			`{"command_only":true,"from_ordinal":99,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusNotFound)
		assertNoMessagePointPrompts(t, "sess-3", 99)
	})

	t.Run("message point remote launch rejects before writing prompt", func(t *testing.T) {
		te := setupPGMode(t)
		removeMessagePointPrompts(t, "sess-remote", 1)

		te.seedSession(t, "sess-remote", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-remote", 3)

		w := te.post(t,
			"/api/v1/sessions/sess-remote/resume",
			`{"from_ordinal":1,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusNotImplemented)
		assertNoMessagePointPrompts(t, "sess-remote", 1)
	})

	t.Run("message point command only works in read only mode", func(t *testing.T) {
		te := setupPGMode(t)
		removeMessagePointPrompts(t, "sess-remote-copy", 1)

		te.seedSession(t, "sess-remote-copy", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-remote-copy", 3)

		w := te.post(t,
			"/api/v1/sessions/sess-remote-copy/resume",
			`{"command_only":true,"from_ordinal":1,"fork_session":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched)
		promptPath := findSingleMessagePointPrompt(t, "sess-remote-copy", 1)
		assertMessagePointCommandForRuntime(t, resp.Command, promptPath)
		t.Cleanup(func() { _ = os.Remove(promptPath) })
	})

	t.Run("whole session remote launch rejects before local launch", func(t *testing.T) {
		dir := tempDirWithRetryCleanup(t)
		dbPath := filepath.Join(dir, "test.db")
		database := dbtest.OpenTestDBAt(t, dbPath)
		store := failingResumeModelCountsStore{
			readOnlyTestStore{Store: database},
		}
		cfg := config.Config{
			Host:         "127.0.0.1",
			Port:         0,
			DataDir:      dir,
			DBPath:       dbPath,
			WriteTimeout: 30 * time.Second,
		}
		srv := server.New(cfg, store, nil)
		te := &testEnv{
			srv:         srv,
			handler:     wrapTestHandler(cfg, srv.Handler()),
			db:          database,
			engine:      nil,
			broadcaster: nil,
			dataDir:     dir,
		}
		te.seedSession(t, "sess-remote-launch", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})

		w := te.post(t,
			"/api/v1/sessions/sess-remote-launch/resume",
			`{}`,
		)
		assertStatus(t, w, http.StatusNotImplemented)
	})

	t.Run("whole session command only works in read only mode", func(t *testing.T) {
		te := setupPGMode(t)
		te.seedSession(t, "sess-remote-command", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-remote-command", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "claude sonnet"
			}
		})

		w := te.post(t,
			"/api/v1/sessions/sess-remote-command/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched)
		assert.Contains(
			t,
			resp.Command,
			"claude --resume sess-remote-command --model 'claude sonnet'",
		)
		assertSamePath(t, "cwd", resp.Cwd, projectDir)
	})

	t.Run("whole session command only uses compact model counts", func(t *testing.T) {
		dir := tempDirWithRetryCleanup(t)
		dbPath := filepath.Join(dir, "test.db")
		database := dbtest.OpenTestDBAt(t, dbPath)
		store := resumeCountsOnlyStore{
			readOnlyTestStore{Store: database},
		}
		cfg := config.Config{
			Host:         "127.0.0.1",
			Port:         0,
			DataDir:      dir,
			DBPath:       dbPath,
			WriteTimeout: 30 * time.Second,
		}
		srv := server.New(cfg, store, nil)
		te := &testEnv{
			srv:         srv,
			handler:     wrapTestHandler(cfg, srv.Handler()),
			db:          database,
			engine:      nil,
			broadcaster: nil,
			dataDir:     dir,
		}

		te.seedSession(t, "sess-remote-compact", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		te.seedMessages(t, "sess-remote-compact", 3, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "claude sonnet"
			}
		})

		w := te.post(t,
			"/api/v1/sessions/sess-remote-compact/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.False(t, resp.Launched)
		assert.Contains(
			t,
			resp.Command,
			"claude --resume sess-remote-compact --model 'claude sonnet'",
		)
	})

	t.Run("whole session command only reports model lookup failure", func(t *testing.T) {
		dir := tempDirWithRetryCleanup(t)
		dbPath := filepath.Join(dir, "test.db")
		database := dbtest.OpenTestDBAt(t, dbPath)
		store := failingResumeModelCountsStore{
			readOnlyTestStore{Store: database},
		}
		cfg := config.Config{
			Host:         "127.0.0.1",
			Port:         0,
			DataDir:      dir,
			DBPath:       dbPath,
			WriteTimeout: 30 * time.Second,
		}
		srv := server.New(cfg, store, nil)
		te := &testEnv{
			srv:         srv,
			handler:     wrapTestHandler(cfg, srv.Handler()),
			db:          database,
			engine:      nil,
			broadcaster: nil,
			dataDir:     dir,
		}

		te.seedSession(t, "sess-remote-error", projectDir, 3, func(s *db.Session) {
			s.Agent = "claude"
		})

		w := te.post(t,
			"/api/v1/sessions/sess-remote-error/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("deleted session rejected", func(t *testing.T) {
		te.seedSession(t, "del-1", "/tmp", 3, func(s *db.Session) {
			s.Agent = "claude"
		})
		require.NoError(t, te.db.SoftDeleteSession(t.Context(), "del-1"))
		w := te.post(t,
			"/api/v1/sessions/del-1/resume",
			`{"command_only":true}`,
		)
		assertStatus(t, w, http.StatusNotFound)
	})
}

type resumeTestResponse struct {
	Command string `json:"command"`
}

func TestPrimaryResumeModel(t *testing.T) {
	te := setup(t)
	t.Run("alphabetical tie", func(t *testing.T) {
		te.seedSession(t, "model-selection", t.TempDir(), 5, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "model-selection", 5, func(i int, m *db.Message) {
			if i == 1 {
				m.Model = "mixed-model-tie-z"
			}
			if i == 3 {
				m.Model = "mixed-model-tie-a"
			}
		})
		w := te.post(t, "/api/v1/sessions/model-selection/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume model-selection -m mixed-model-tie-a", resp.Command)
	})

	t.Run("higher count ignores user-only models", func(t *testing.T) {
		te.seedSession(t, "model-selection-count", t.TempDir(), 5, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "model-selection-count", 5, func(i int, m *db.Message) {
			switch i {
			case 0:
				m.Role = "user"
				m.Model = "user-only-model"
			case 1, 3:
				m.Model = "later-model"
			case 4:
				m.Model = "earlier-model"
			}
		})
		w := te.post(t, "/api/v1/sessions/model-selection-count/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume model-selection-count -m later-model", resp.Command)
	})

	t.Run("UTF-16 tie parity", func(t *testing.T) {
		te.seedSession(t, "model-selection-utf16", t.TempDir(), 5, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "model-selection-utf16", 5, func(i int, m *db.Message) {
			switch i {
			case 1:
				m.Model = "\uE000"
			case 3:
				m.Model = "\U00010000"
			}
		})
		w := te.post(t, "/api/v1/sessions/model-selection-utf16/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume model-selection-utf16 -m '𐀀'", resp.Command)
	})

	t.Run("synthetic-only histories omit model pin", func(t *testing.T) {
		te.seedSession(t, "model-selection-synthetic", t.TempDir(), 5, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "model-selection-synthetic", 5, func(i int, m *db.Message) {
			if i == 1 || i == 3 {
				m.Model = "<synthetic>"
			}
		})
		w := te.post(t, "/api/v1/sessions/model-selection-synthetic/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume model-selection-synthetic", resp.Command)
	})

	t.Run("synthetic models lose to real models", func(t *testing.T) {
		te.seedSession(t, "model-selection-real", t.TempDir(), 5, func(s *db.Session) {
			s.Agent = "codex"
		})
		te.seedMessages(t, "model-selection-real", 5, func(i int, m *db.Message) {
			switch i {
			case 1, 3:
				m.Model = "<synthetic>"
			case 2:
				m.Role = "assistant"
				m.Model = "real-model"
			}
		})
		w := te.post(t, "/api/v1/sessions/model-selection-real/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp resumeTestResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "codex resume model-selection-real -m real-model", resp.Command)
	})
}

func TestResumeRemoteCwd(t *testing.T) {
	for _, tc := range []struct{ agent, command string }{
		{"claude", "claude --resume abc-123"},
		{"kiro", "kiro-cli chat --resume-id abc-123"},
		{"cursor", "cursor agent --resume abc-123"},
		{"codex", "codex resume abc-123"},
		{"copilot", "copilot --resume=abc-123"},
		{"gemini", "gemini --resume abc-123"},
		{"opencode", "opencode --session abc-123"},
		{"amp", "amp --resume abc-123"},
	} {
		for _, cwd := range []string{"", "/home/user/project", "/remote/project dir", `C:\remote\project`} {
			t.Run(tc.agent+"/"+cwd, func(t *testing.T) {
				te := setup(t)
				localDir := t.TempDir()
				file := filepath.Join(t.TempDir(), "session.jsonl")
				pathJSON, err := json.Marshal(localDir)
				require.NoError(t, err)
				// Conflicting local transcript paths must never influence remote output.
				content := `{"cwd":` + string(pathJSON) + `,"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"working_directory":` + string(pathJSON) + `}}]}}`
				require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
				id := "devbox1~" + tc.agent + ":abc-123"
				te.seedSession(t, id, localDir, 1, func(s *db.Session) {
					s.Agent = tc.agent
					s.Cwd = cwd
					s.FilePath = &file
				})
				w := te.post(t, "/api/v1/sessions/"+id+"/resume", `{"command_only":true,"opener_id":"missing-terminal"}`)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var resp struct {
					Launched bool   `json:"launched"`
					Command  string `json:"command"`
					Cwd      string `json:"cwd"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				want := tc.command
				if cwd != "" && (tc.agent == "claude" || tc.agent == "kiro") {
					want = "cd '" + cwd + "' && " + want
				}
				assert.Equal(t, want, resp.Command)
				assert.Equal(t, cwd, resp.Cwd)
				assert.False(t, resp.Launched)
			})
		}
	}
}

func TestGetSessionDirectory(t *testing.T) {
	te := setup(t)

	t.Run("removed_absolute_paths", func(t *testing.T) {
		type testCase struct {
			name    string
			id      string
			project string
			setup   func(*db.Session)
			want    string
		}

		removedChild := func(t *testing.T, name string) string {
			t.Helper()
			parent := t.TempDir()
			path := filepath.Join(parent, name)
			require.NoError(t, os.Mkdir(path, 0o755))
			require.NoError(t, os.Remove(path))
			return path
		}

		embeddedPath := removedChild(t, "embedded path")
		embeddedLater := t.TempDir()
		embeddedSource := filepath.Join(t.TempDir(), "session.jsonl")
		embeddedJSON, err := json.Marshal(embeddedPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			embeddedSource,
			[]byte(`{"cwd":`+string(embeddedJSON)+"}\n"),
			0o600,
		))

		cachedPath := removedChild(t, "cached path")
		cachedLater := t.TempDir()
		projectPath := removedChild(t, "project path")

		cases := []testCase{
			{
				name:    "embedded_cwd",
				id:      "dir-removed-embedded",
				project: embeddedLater,
				setup: func(s *db.Session) {
					s.FilePath = &embeddedSource
				},
				want: embeddedPath,
			},
			{
				name:    "cached_cwd",
				id:      "dir-removed-cached",
				project: cachedLater,
				setup: func(s *db.Session) {
					s.Cwd = cachedPath
				},
				want: cachedPath,
			},
			{
				name:    "project",
				id:      "dir-removed-project",
				project: projectPath,
				want:    projectPath,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var opts []func(*db.Session)
				if tc.setup != nil {
					opts = append(opts, tc.setup)
				}
				te.seedSession(t, tc.id, tc.project, 1, opts...)
				w := te.get(t, "/api/v1/sessions/"+tc.id+"/directory")
				assertStatus(t, w, http.StatusOK)
				var resp struct {
					Path string `json:"path"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				t.Logf("status=%d path=%q want=%q", w.Code, resp.Path, tc.want)
				assert.Equal(t, tc.want, resp.Path)
			})
		}
	})

	t.Run("candidate_order", func(t *testing.T) {
		embeddedDir := t.TempDir()
		cachedDir := t.TempDir()
		projectDir := t.TempDir()
		sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
		embeddedJSON, err := json.Marshal(embeddedDir)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			sessionFile,
			[]byte(`{"cwd":`+string(embeddedJSON)+"}\n"),
			0o600,
		))
		te.seedSession(t, "dir-order-embedded", projectDir, 1, func(s *db.Session) {
			s.Cwd = cachedDir
			s.FilePath = &sessionFile
		})
		w := te.get(t, "/api/v1/sessions/dir-order-embedded/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("embedded precedence status=%d path=%q", w.Code, resp.Path)
		assert.Equal(t, embeddedDir, resp.Path)

		cursorWorkspace := filepath.Join(t.TempDir(), "workspace-root", "cursor-project")
		cursorFallback := t.TempDir()
		removedCursorCwd := filepath.Join(t.TempDir(), "removed-cwd")
		require.NoError(t, os.Mkdir(removedCursorCwd, 0o755))
		require.NoError(t, os.Remove(removedCursorCwd))
		cursorClean := filepath.Clean(cursorWorkspace)
		cursorTokens := []string{}
		if volume := filepath.VolumeName(cursorClean); volume != "" {
			cursorTokens = append(cursorTokens, strings.TrimSuffix(volume, ":"))
			cursorClean = strings.TrimPrefix(cursorClean, volume)
		}
		for part := range strings.SplitSeq(cursorClean, string(filepath.Separator)) {
			if part == "" {
				continue
			}
			cursorTokens = append(cursorTokens, strings.FieldsFunc(part, func(r rune) bool {
				return r == '-' || r == '.' || r == '_'
			})...)
		}
		cursorEncoded := strings.Join(cursorTokens, "-")
		cursorTranscript := filepath.Join(
			t.TempDir(), ".cursor", "projects",
			cursorEncoded,
			"agent-transcripts", "cursor-order", "cursor-order.jsonl",
		)
		require.NoError(t, os.MkdirAll(filepath.Dir(cursorTranscript), 0o755))
		require.NoError(t, os.MkdirAll(cursorWorkspace, 0o755))
		require.NoError(t, os.WriteFile(cursorTranscript, []byte("{}\n"), 0o600))
		te.seedSession(t, "dir-order-cursor", cursorFallback, 1, func(s *db.Session) {
			s.Agent = "cursor"
			s.Cwd = removedCursorCwd
			s.FilePath = &cursorTranscript
		})
		w = te.get(t, "/api/v1/sessions/dir-order-cursor/directory")
		assertStatus(t, w, http.StatusOK)
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("cursor precedence status=%d path=%q", w.Code, resp.Path)
		assert.Equal(t, removedCursorCwd, resp.Path)

		te.seedSession(t, "dir-order-cursor-workspace", cursorFallback, 1, func(s *db.Session) {
			s.Agent = "cursor"
			s.FilePath = &cursorTranscript
		})
		w = te.get(t, "/api/v1/sessions/dir-order-cursor-workspace/directory")
		assertStatus(t, w, http.StatusOK)
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("cursor workspace status=%d path=%q", w.Code, resp.Path)
		assertSamePath(t, "path", resp.Path, cursorWorkspace)
	})

	t.Run("relative_metadata_falls_back", func(t *testing.T) {
		projectDir := t.TempDir()
		relativeSource := filepath.Join(t.TempDir(), "relative.jsonl")
		relativeJSON, err := json.Marshal(filepath.Join("relative", "cwd"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			relativeSource,
			[]byte(`{"cwd":`+string(relativeJSON)+"}\n"),
			0o600,
		))

		cases := []struct {
			name  string
			id    string
			setup func(*db.Session)
		}{
			{
				name: "relative_embedded_cwd",
				id:   "dir-relative-embedded",
				setup: func(s *db.Session) {
					s.FilePath = &relativeSource
				},
			},
			{
				name: "nested_relative_cached_cwd",
				id:   "dir-relative-cached",
				setup: func(s *db.Session) {
					s.Cwd = filepath.Join("nested", "relative")
				},
			},
			{
				name: "empty_metadata",
				id:   "dir-relative-empty",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var opts []func(*db.Session)
				if tc.setup != nil {
					opts = append(opts, tc.setup)
				}
				te.seedSession(t, tc.id, projectDir, 1, opts...)
				w := te.get(t, "/api/v1/sessions/"+tc.id+"/directory")
				assertStatus(t, w, http.StatusOK)
				var resp struct {
					Path string `json:"path"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				t.Logf("status=%d path=%q want=%q", w.Code, resp.Path, projectDir)
				assert.Equal(t, projectDir, resp.Path)
			})
		}

		te.seedSession(t, "dir-relative-label", "project-label", 1, func(s *db.Session) {
			s.Cwd = "relative/cwd"
		})
		w := te.get(t, "/api/v1/sessions/dir-relative-label/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("label fallback status=%d path=%q", w.Code, resp.Path)
		assert.Empty(t, resp.Path)
	})

	t.Run("absolute_file_path", func(t *testing.T) {
		recordedFile := filepath.Join(t.TempDir(), "recorded.txt")
		require.NoError(t, os.WriteFile(recordedFile, []byte("synthetic"), 0o600))
		projectDir := t.TempDir()
		te.seedSession(t, "dir-regular-file", projectDir, 1, func(s *db.Session) {
			s.Cwd = recordedFile
		})
		w := te.get(t, "/api/v1/sessions/dir-regular-file/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("status=%d path=%q want=%q", w.Code, resp.Path, recordedFile)
		assert.Equal(t, recordedFile, resp.Path)
	})

	t.Run("stored_cwd_fallback", func(t *testing.T) {
		cachedDir := t.TempDir()
		missingSource := filepath.Join(t.TempDir(), "missing.jsonl")
		virtualPath := filepath.Join(t.TempDir(), "data.sqlite3") + "#sqlite-session"

		hashRoot := filepath.Join(t.TempDir(), "project#dev")
		hashCwd := filepath.Join(hashRoot, "workspace")
		hashSource := filepath.Join(hashRoot, "session.jsonl")
		require.NoError(t, os.MkdirAll(hashCwd, 0o755))
		hashJSON, err := json.Marshal(hashCwd)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			hashSource,
			[]byte(`{"cwd":`+string(hashJSON)+"}\n"),
			0o600,
		))

		cases := []struct {
			name          string
			id            string
			setup         func(*db.Session)
			sourceMissing bool
			want          string
		}{
			{
				name:          "source_missing",
				id:            "dir-cached-source-missing",
				sourceMissing: true,
				setup: func(s *db.Session) {
					s.Cwd = cachedDir
					s.FilePath = &missingSource
				},
				want: cachedDir,
			},
			{
				name: "missing_source",
				id:   "dir-cached-missing-source",
				setup: func(s *db.Session) {
					s.Cwd = cachedDir
					s.FilePath = &missingSource
				},
				want: cachedDir,
			},
			{
				name: "virtual_source",
				id:   "dir-cached-virtual-source",
				setup: func(s *db.Session) {
					s.Cwd = cachedDir
					s.FilePath = &virtualPath
				},
				want: cachedDir,
			},
			{
				name: "real_hash_filename",
				id:   "dir-hash-source",
				setup: func(s *db.Session) {
					s.Cwd = t.TempDir()
					s.FilePath = &hashSource
				},
				want: hashCwd,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				te.seedSession(t, tc.id, t.TempDir(), 1, tc.setup)
				if tc.sourceMissing {
					require.NoError(t, te.db.BaselineActiveSessionSourcePaths(
						t.Context(), "test", []db.SessionSourcePath{{
							Agent: "claude", FilePath: missingSource,
						}},
					))
					changed, err := te.db.MarkSessionSourceMissing(
						t.Context(), "test", "claude", tc.id, missingSource,
					)
					require.NoError(t, err)
					assert.True(t, changed)
				}
				before, err := te.db.GetSessionFull(t.Context(), tc.id)
				require.NoError(t, err)
				require.NotNil(t, before)

				w := te.get(t, "/api/v1/sessions/"+tc.id+"/directory")
				assertStatus(t, w, http.StatusOK)
				var resp struct {
					Path string `json:"path"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				t.Logf("status=%d path=%q want=%q", w.Code, resp.Path, tc.want)
				assert.Equal(t, tc.want, resp.Path)

				after, err := te.db.GetSessionFull(t.Context(), tc.id)
				require.NoError(t, err)
				require.NotNil(t, after)
				assert.Equal(t, before.Cwd, after.Cwd)
				assert.Equal(t, before.Project, after.Project)
				assert.Equal(t, before.FilePath, after.FilePath)
				assert.Equal(t, before.SourceMissingAt, after.SourceMissingAt)
				assert.Equal(t, before.DeletedAt, after.DeletedAt)
			})
		}
	})

	t.Run("not found", func(t *testing.T) {
		w := te.get(t, "/api/v1/sessions/nonexistent/directory")
		assertStatus(t, w, http.StatusNotFound)
	})

	t.Run("cursor directory returns workspace root", func(t *testing.T) {
		projectDir := t.TempDir()
		runDir := filepath.Join(projectDir, "frontend")
		require.NoError(t, os.MkdirAll(runDir, 0o755))
		runDirJSON, _ := json.Marshal(runDir)
		sessionFile := filepath.Join(t.TempDir(), "cursor.jsonl")
		content := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"pwd","working_directory":` +
			string(runDirJSON) + `}}]}}` + "\n"
		require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))
		te.seedSession(t, "dir-cursor", projectDir, 3, func(s *db.Session) {
			s.Agent = "cursor"
			s.FilePath = &sessionFile
		})

		w := te.get(t, "/api/v1/sessions/dir-cursor/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assertSamePath(t, "path", resp.Path, projectDir)
	})

	t.Run("lookup_boundaries", func(t *testing.T) {
		te.seedSession(t, "dir-trashed", t.TempDir(), 1)
		require.NoError(t, te.db.SoftDeleteSession(t.Context(), "dir-trashed"))
		w := te.get(t, "/api/v1/sessions/dir-trashed/directory")
		t.Logf("trashed status=%d body=%s", w.Code, w.Body.String())
		assertStatus(t, w, http.StatusNotFound)
		assert.Contains(t, w.Body.String(), "session not found")

		readOnly := setupPGMode(t)
		readOnly.seedSession(t, "dir-read-only", t.TempDir(), 1)
		w = readOnly.get(t, "/api/v1/sessions/dir-read-only/directory")
		t.Logf("read-only status=%d body=%s", w.Code, w.Body.String())
		assertStatus(t, w, http.StatusNotImplemented)
		assert.Contains(t, w.Body.String(), "not available in remote mode")
	})
}

func TestOpenSessionDirectoryValidation(t *testing.T) {
	te := setup(t)

	t.Run("unusable_candidates", func(t *testing.T) {
		removedParent := t.TempDir()
		removedDir := filepath.Join(removedParent, "removed")
		require.NoError(t, os.Mkdir(removedDir, 0o755))
		require.NoError(t, os.Remove(removedDir))

		embeddedSource := filepath.Join(t.TempDir(), "embedded.jsonl")
		embeddedJSON, err := json.Marshal(removedDir)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			embeddedSource,
			[]byte(`{"cwd":`+string(embeddedJSON)+"}\n"),
			0o600,
		))

		recordedFile := filepath.Join(t.TempDir(), "recorded.txt")
		require.NoError(t, os.WriteFile(recordedFile, []byte("synthetic"), 0o600))

		cases := []struct {
			name  string
			id    string
			setup func(*db.Session)
		}{
			{
				name: "removed_directory",
				id:   "open-removed-directory",
				setup: func(s *db.Session) {
					s.FilePath = &embeddedSource
					s.Project = "relative-project"
				},
			},
			{
				name: "regular_file",
				id:   "open-regular-file",
				setup: func(s *db.Session) {
					s.Cwd = recordedFile
					s.Project = "relative-project"
				},
			},
			{
				name: "relative_only",
				id:   "open-relative-only",
				setup: func(s *db.Session) {
					s.Cwd = filepath.Join("relative", "cwd")
					s.Project = "relative-project"
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				te.seedSession(t, tc.id, "relative-project", 1, func(s *db.Session) {
					s.Agent = "claude"
					tc.setup(s)
				})
				w := te.post(t, "/api/v1/sessions/"+tc.id+"/open",
					`{"opener_id":"__agentsview_missing_opener__"}`)
				t.Logf("status=%d body=%s", w.Code, w.Body.String())
				assertStatus(t, w, http.StatusBadRequest)
				assert.Contains(t, w.Body.String(), "session has no project directory")
			})
		}
	})

	t.Run("existing_fallback", func(t *testing.T) {
		removedParent := t.TempDir()
		removedDir := filepath.Join(removedParent, "removed")
		require.NoError(t, os.Mkdir(removedDir, 0o755))
		require.NoError(t, os.Remove(removedDir))
		embeddedSource := filepath.Join(t.TempDir(), "embedded.jsonl")
		embeddedJSON, err := json.Marshal(removedDir)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			embeddedSource,
			[]byte(`{"cwd":`+string(embeddedJSON)+"}\n"),
			0o600,
		))

		cachedDir := t.TempDir()
		projectDir := t.TempDir()
		cases := []struct {
			name  string
			id    string
			setup func(*db.Session)
		}{
			{
				name: "cached_directory",
				id:   "open-existing-cached",
				setup: func(s *db.Session) {
					s.FilePath = &embeddedSource
					s.Cwd = cachedDir
				},
			},
			{
				name: "project_directory",
				id:   "open-existing-project",
				setup: func(s *db.Session) {
					s.FilePath = &embeddedSource
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				te.seedSession(t, tc.id, projectDir, 1, func(s *db.Session) {
					s.Agent = "claude"
					tc.setup(s)
				})
				w := te.post(t, "/api/v1/sessions/"+tc.id+"/open",
					`{"opener_id":"__agentsview_missing_opener__"}`)
				t.Logf("status=%d body=%s", w.Code, w.Body.String())
				assertStatus(t, w, http.StatusBadRequest)
				assert.Contains(t, w.Body.String(), "opener")
				assert.NotContains(t, w.Body.String(), "session has no project directory")
			})
		}
	})

	t.Run("lookup_boundaries", func(t *testing.T) {
		readOnly := setupPGMode(t)
		readOnly.seedSession(t, "open-read-only", t.TempDir(), 1)
		w := readOnly.post(t, "/api/v1/sessions/open-read-only/open",
			`{"opener_id":"__agentsview_missing_opener__"}`)
		t.Logf("read-only status=%d body=%s", w.Code, w.Body.String())
		assertStatus(t, w, http.StatusNotImplemented)
		assert.Contains(t, w.Body.String(), "not available in remote mode")

		te.seedSession(t, "open-trashed", t.TempDir(), 1)
		require.NoError(t, te.db.SoftDeleteSession(t.Context(), "open-trashed"))
		w = te.post(t, "/api/v1/sessions/open-trashed/open",
			`{"opener_id":"__agentsview_missing_opener__"}`)
		t.Logf("trashed status=%d body=%s", w.Code, w.Body.String())
		assertStatus(t, w, http.StatusNotFound)
		assert.Contains(t, w.Body.String(), "session not found")
	})
}

func TestResumeSessionDirectoryValidation(t *testing.T) {
	te := setup(t)

	t.Run("unusable_candidates", func(t *testing.T) {
		removedParent := t.TempDir()
		removedDir := filepath.Join(removedParent, "removed")
		require.NoError(t, os.Mkdir(removedDir, 0o755))
		require.NoError(t, os.Remove(removedDir))
		recordedFile := filepath.Join(t.TempDir(), "recorded.txt")
		require.NoError(t, os.WriteFile(recordedFile, []byte("synthetic"), 0o600))

		cases := []struct {
			name string
			id   string
			cwd  string
		}{
			{name: "removed_directory", id: "resume-removed", cwd: removedDir},
			{name: "regular_file", id: "resume-regular-file", cwd: recordedFile},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				te.seedSession(t, tc.id, "relative-project", 1, func(s *db.Session) {
					s.Agent = "claude"
					s.Cwd = tc.cwd
				})
				w := te.post(t, "/api/v1/sessions/"+tc.id+"/resume",
					`{"command_only":true}`)
				assertStatus(t, w, http.StatusOK)
				var resp struct {
					Launched bool   `json:"launched"`
					Command  string `json:"command"`
					Cwd      string `json:"cwd"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				t.Logf("status=%d cwd=%q command=%q", w.Code, resp.Cwd, resp.Command)
				assert.False(t, resp.Launched)
				assert.Empty(t, resp.Cwd)
				assert.Equal(t, "claude --resume "+tc.id, resp.Command)
				assert.NotContains(t, resp.Command, "cd ")
			})
		}
	})

	t.Run("existing_fallback", func(t *testing.T) {
		removedParent := t.TempDir()
		removedDir := filepath.Join(removedParent, "removed")
		require.NoError(t, os.Mkdir(removedDir, 0o755))
		require.NoError(t, os.Remove(removedDir))
		projectDir := t.TempDir()
		te.seedSession(t, "resume-existing-fallback", projectDir, 1, func(s *db.Session) {
			s.Agent = "claude"
			s.Cwd = removedDir
		})
		w := te.post(t, "/api/v1/sessions/resume-existing-fallback/resume",
			`{"command_only":true}`)
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Launched bool   `json:"launched"`
			Command  string `json:"command"`
			Cwd      string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		t.Logf("status=%d cwd=%q command=%q", w.Code, resp.Cwd, resp.Command)
		assert.False(t, resp.Launched)
		assert.Equal(t, projectDir, resp.Cwd)
		assert.Equal(t, "cd '"+projectDir+"' && claude --resume resume-existing-fallback", resp.Command)
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
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// The response should always be an array (possibly empty),
	// never null.
	assert.NotNil(t, resp.Openers, "openers should be [] not null")
}

func TestGetTerminalConfig(t *testing.T) {
	te := setup(t)

	t.Run("default config", func(t *testing.T) {
		w := te.get(t, "/api/v1/config/terminal")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Mode string `json:"mode"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "auto", resp.Mode)
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "clipboard", resp.Mode)
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

func TestSetTerminalConfigExpandsHomeBeforeImmediateResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX executable script")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	binDir := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(binDir, "test-terminal"),
		[]byte("#!/bin/sh\nexit 0\n"),
		0o755,
	))

	te := setup(t)
	projectDir := t.TempDir()
	te.seedSession(t, "live-terminal-config", projectDir, 1, func(s *db.Session) {
		s.Agent = "claude"
	})

	w := te.post(t,
		"/api/v1/config/terminal",
		`{"mode":"custom","custom_bin":"~/bin/test-terminal"}`,
	)
	assertStatus(t, w, http.StatusOK)

	w = te.post(t, "/api/v1/sessions/live-terminal-config/resume", `{}`)
	assertStatus(t, w, http.StatusOK)
	var resp struct {
		Launched bool   `json:"launched"`
		Terminal string `json:"terminal"`
		Error    string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Launched)
	assert.Equal(t, "test-terminal", resp.Terminal)
	assert.Empty(t, resp.Error)
}

func TestResumeTerminalSurvivesRequestCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX executable script")
	}
	home := t.TempDir()
	started := filepath.Join(home, "started")
	completed := filepath.Join(home, "completed")
	release := filepath.Join(home, "release")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RESUME_STARTED", started)
	t.Setenv("RESUME_COMPLETED", completed)
	t.Setenv("RESUME_RELEASE", release)
	binDir := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, exec.CommandContext(t.Context(), "mkfifo", release).Run())
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "test-terminal"),
		[]byte("#!/bin/sh\nprintf started > \"$RESUME_STARTED\"\nIFS= read -r _ < \"$RESUME_RELEASE\"\nprintf completed > \"$RESUME_COMPLETED\"\n"), 0o755))
	releaseFile, err := os.OpenFile(release, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = releaseFile.WriteString("\n")
		_ = releaseFile.Close()
	})

	te := setup(t)
	projectDir := t.TempDir()
	te.seedSession(t, "cancelled-resume", projectDir, 1, func(s *db.Session) {
		s.Agent = "claude"
	})
	w := te.post(t, "/api/v1/config/terminal",
		`{"mode":"custom","custom_bin":"~/bin/test-terminal"}`)
	assertStatus(t, w, http.StatusOK)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/sessions/cancelled-resume/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(response, req)
		close(done)
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "resume request did not return")
	}
	// Cancellation can interrupt the HTTP response; check the terminal itself.
	assert.NoFileExists(t, completed)
	_, err = releaseFile.WriteString("release\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(completed)
		return err == nil
	}, time.Second, 5*time.Millisecond)
}
