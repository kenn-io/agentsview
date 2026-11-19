//go:build linux && (amd64 || arm64) && !race

package rawderive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Race tracing allocates outside Go's heap and exceeds the production
// address/data limits. Run these unchanged limits with the ordinary Linux
// binary; descriptor isolation and seccomp thread checks also run under race.
func TestSandboxKernelControls(t *testing.T) {
	p, err := NewSubprocessParser(5 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		require.ErrorIs(t, err, ErrSandboxUnavailable)
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.FailNow(t, "mandatory kernel isolation unavailable")
		}
		t.Skip("kernel isolation unavailable; positive controls require Linux namespace support")
	}
	t.Setenv("RAW_SANDBOX_SECRET", "must-not-inherit")
	for _, mode := range []string{"denials", "memory", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, "input"), []byte("fixture"), 0o400))
			jail := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(jail, "source"), 0o700))
			outside := filepath.Join(t.TempDir(), "sentinel")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o400))
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			childCtx, stopChild := context.WithCancel(ctx)
			defer stopChild()
			cmd := exec.CommandContext(childCtx, os.Args[0], "--sandbox-kernel-test", mode, source, jail, outside)
			cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=" + parserGoMemoryLimit}
			require.NoError(t, configureParserNamespace(cmd))
			start := time.Now()
			var checks map[string]bool
			err := runParserProcess(ctx, stopChild, cmd, nil, parserOutputLimit, parserErrorLimit, func(src io.Reader) error {
				return json.NewDecoder(src).Decode(&checks)
			})
			require.NotNil(t, cmd.ProcessState)
			if mode == "denials" {
				require.NoError(t, err)
				require.GreaterOrEqual(t, len(checks), 14)
				for key, value := range checks {
					assert.True(t, value, key)
				}
			} else {
				require.Error(t, err)
				require.NoError(t, ctx.Err())
				assert.Less(t, time.Since(start), 38*time.Second)
				if mode == "cpu" {
					status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
					require.True(t, ok)
					assert.Equal(t, syscall.SIGKILL, status.Signal())
					assert.Greater(t, time.Since(start), 20*time.Second)
				} else {
					assert.Equal(t, 2, cmd.ProcessState.ExitCode(), "Go hard allocation failure")
				}
			}
			body, err := os.ReadFile(filepath.Join(source, "input"))
			require.NoError(t, err)
			assert.Equal(t, "fixture", string(body))
			require.NoError(t, p.Preflight(t.Context()), "subsequent jobs remain usable")
		})
	}
}

// This re-executes the test binary under production memory limits. Race
// tracing can exhaust those limits while parsing the SQLite fixture.
func TestSandboxProviderJSONAndSQLite(t *testing.T) {
	p, err := NewSubprocessParser(20 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	fixtures := []struct {
		name      string
		provider  parser.AgentType
		path, key string
		body      []byte
		ids       []string
	}{
		{"json", parser.AgentClaude, "project/session-a.jsonl", "/canonical/project/session-a.jsonl", []byte(`{"type":"user","timestamp":"2026-08-13T12:00:00Z","uuid":"u1","sessionId":"session-a","message":{"content":"hello"},"cwd":"/work/project"}` + "\n" + `{"type":"assistant","timestamp":"2026-08-13T12:00:01Z","uuid":"a1","parentUuid":"u1","sessionId":"session-a","message":{"content":"hi"}}` + "\n"), []string{"session-a"}},
		{"sqlite", parser.AgentForge, parser.ForgeDBFilename, parser.ForgeDBFilename, forgeSnapshotFixture(t, map[string]string{"conv-001": "2026-05-02 09:58:15", "conv-002": "2026-05-03 09:58:15"}), []string{"forge:conv-001", "forge:conv-002"}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			ref := objectRefForBytes(t, f.body)
			m := parserTestManifest(t, f.provider, f.key, []rawsync.Entry{{Path: f.path, Type: "file", Length: int64(len(f.body)), Objects: []rawsync.ObjectRef{ref}}})
			tree, err := (Materializer{Store: &materializerStore{objects: map[rawsync.ObjectRef][]byte{ref: f.body}}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), m)
			require.NoError(t, err)
			defer func() { require.NoError(t, tree.Cleanup()) }()
			got, err := p.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			require.Len(t, got.Outcome.Results, len(f.ids))
			assert.True(t, got.Outcome.ResultSetComplete)
			assert.Empty(t, got.Outcome.SourceErrors)
			for i, id := range f.ids {
				assert.Equal(t, id, got.Outcome.Results[i].Result.Session.ID)
				require.Len(t, got.Outcome.Results[i].Result.Messages, 2)
			}
			plain, err := NewProviderParser(parser.ProviderFactories(), "hosted")
			require.NoError(t, err)
			expected, err := plain.Parse(t.Context(), m, tree)
			require.NoError(t, err)
			assert.Equal(t, expected, got, "kernel boundary preserves complete normalized provider semantics")
		})
	}
}

func TestSandboxCodexOutcomesAboveFormerLimit(t *testing.T) {
	p, err := NewSubprocessParser(30 * time.Second)
	require.NoError(t, err)
	if err = p.Preflight(t.Context()); err != nil {
		if os.Getenv("RAW_SANDBOX_REQUIRED") == "1" {
			require.NoError(t, err)
		}
		t.Skip("kernel isolation unavailable")
	}
	for _, tc := range []struct {
		name           string
		messages, size int
	}{
		{"large_value", 1, 33 << 20},
		{"many_messages", 2048, 17 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "33333333-3333-4333-8333-333333333333"
			root := t.TempDir()
			path := filepath.Join(root, "rollout-2026-09-08T10-00-00-"+id+".jsonl")
			file, err := os.Create(path)
			require.NoError(t, err)
			_, err = io.WriteString(file, testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-09-08T10:00:00Z")+"\n")
			require.NoError(t, err)
			content := strings.Repeat("x", tc.size)
			for i := range tc.messages {
				role := "user"
				if i%2 != 0 {
					role = "assistant"
				}
				_, err = io.WriteString(file, testjsonl.CodexMsgJSON(role, content, "2026-09-08T10:00:01Z")+"\n")
				require.NoError(t, err)
			}
			require.NoError(t, file.Close())
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + id})
			require.NoError(t, err)
			require.True(t, found)
			manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
			tree, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: MaxMaterializedBytes}).Materialize(t.Context(), manifest)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, tree.Cleanup()) })
			out, err := p.Parse(t.Context(), manifest, tree)
			require.NoError(t, err)
			require.Len(t, out.Outcome.Results, 1)
			assert.True(t, out.Outcome.ResultSetComplete)
			assert.Empty(t, out.Outcome.SourceErrors)
			parsed := out.Outcome.Results[0]
			assert.Equal(t, parser.DataVersionCurrent, parsed.DataVersion)
			require.Len(t, parsed.Result.Messages, tc.messages)
			contentDigest := sha256.Sum256([]byte(content))
			for _, message := range parsed.Result.Messages {
				require.Equal(t, contentDigest, sha256.Sum256([]byte(message.Content)))
			}
			var wire byteCountWriter
			require.NoError(t, encodeParserOutcome(&wire, out))
			assert.Greater(t, wire.bytes, int64(32<<20), "the complete provider outcome exceeds the former IPC ceiling")
			unchanged, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, manifest.Manifest.Entries[0].Objects[0].SHA256, fmt.Sprintf("%x", sha256.Sum256(unchanged)))
			require.NoError(t, p.Preflight(t.Context()), "a large import leaves the next job usable")
		})
	}
}

type byteCountWriter struct{ bytes int64 }

func (w *byteCountWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	return len(p), nil
}
