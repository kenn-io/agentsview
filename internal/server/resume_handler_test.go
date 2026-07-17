package server_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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

func TestResumeSession(t *testing.T) {
	te := setup(t)

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
		assert.Equal(t,
			"cursor agent --resume chat-1 --workspace '"+wantProjectDir+"'",
			resp.Command)
		assertSamePath(t, "cwd", resp.Cwd, runDir)
	})

	t.Run("cursor command only falls back workspace to cwd", func(t *testing.T) {
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
		wantRunDir := canonicalTestPath(runDir)
		assert.Equal(t,
			"cursor agent --resume chat-2 --workspace '"+wantRunDir+"'",
			resp.Command)
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
			require.Greater(t, idx, 0, "command = %q", resp.Command)
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
		require.NoError(t, te.db.SoftDeleteSession("del-1"))
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
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assertSamePath(t, "path", resp.Path, projectDir)
	})

	t.Run("empty path for relative project", func(t *testing.T) {
		te.seedSession(t, "dir-2", "my-repo", 3)
		w := te.get(t, "/api/v1/sessions/dir-2/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Empty(t, resp.Path)
	})

	t.Run("not found", func(t *testing.T) {
		w := te.get(t, "/api/v1/sessions/nonexistent/directory")
		assertStatus(t, w, http.StatusNotFound)
	})

	t.Run("prefers session file cwd", func(t *testing.T) {
		cwdDir := filepath.Join(t.TempDir(), "nested")
		require.NoError(t, os.Mkdir(cwdDir, 0o755))
		sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
		cwdJSON, _ := json.Marshal(cwdDir)
		content := `{"cwd":` + string(cwdJSON) + "}\n"
		require.NoError(t, os.WriteFile(sessionFile, []byte(content), 0o644))
		te.seedSession(t, "dir-3", projectDir, 3, func(s *db.Session) {
			s.FilePath = &sessionFile
		})
		w := te.get(t, "/api/v1/sessions/dir-3/directory")
		assertStatus(t, w, http.StatusOK)
		var resp struct {
			Path string `json:"path"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assertSamePath(t, "path", resp.Path, cwdDir)
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
