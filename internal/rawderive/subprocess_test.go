package rawderive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestSandboxWirePreservesPartialOutcome(t *testing.T) {
	in := ParsedManifest{Outcome: parser.ParseOutcome{ForceReplace: true, ResultSetComplete: false, ExcludedSessionIDs: []string{"excluded"}, SourceErrors: []parser.SourceError{{SessionID: "failed", SourceKey: "source", Retryable: true, Err: errors.New("private source contents")}}, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionNeedsRetry, RetryReason: "retry", Result: parser.ParseResult{Session: parser.ParsedSession{ID: "good"}}}}}}
	in.Outcome.Results[0].Result.Messages = []parser.ParsedMessage{{ToolCalls: []parser.ParsedToolCall{{ResultEvents: []parser.ParsedToolResultEvent{{RawContentDigest: []byte{1, 2, 3}, Content: "result"}}}}}}
	encoded, err := encodeParserOutcome(in)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private source contents")
	out, err := decodeParserOutcome(encoded)
	require.NoError(t, err)
	assert.Equal(t, []string{"excluded"}, out.Outcome.ExcludedSessionIDs)
	assert.Equal(t, []byte{1, 2, 3}, out.Outcome.Results[0].Result.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest)
	assert.True(t, out.Outcome.ForceReplace)
	assert.False(t, out.Outcome.ResultSetComplete)
	require.Len(t, out.Outcome.SourceErrors, 1)
	assert.True(t, out.Outcome.SourceErrors[0].Retryable)
	assert.Equal(t, "failed", out.Outcome.SourceErrors[0].SessionID)
	require.Error(t, out.Outcome.SourceErrors[0].Err)
	assert.Equal(t, parser.DataVersionNeedsRetry, out.Outcome.Results[0].DataVersion)
	_, err = decodeParserOutcome(append(encoded, []byte(" {}")...))
	assert.Error(t, err)
}

func TestParserProcessHarness(t *testing.T) {
	if os.Getenv("RAW_TEST_CHILD") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "blocked":
		os.Stdout.WriteString("READY\n")
		for {
			time.Sleep(time.Hour)
		}
	case "stdout":
		os.Stdout.WriteString("READY\n")
		for {
			os.Stdout.Write(bytes.Repeat([]byte("x"), 4096))
		}
	case "stderr":
		os.Stdout.WriteString("READY\n")
		for {
			os.Stderr.Write(bytes.Repeat([]byte("x"), 4096))
		}
	}
	os.Exit(0)
}
func TestParserProcessKillsAndReaps(t *testing.T) {
	for _, mode := range []string{"blocked", "stdout", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			timeout := 3 * time.Second
			if mode == "blocked" {
				timeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestParserProcessHarness$", "--", mode)
			cmd.Env = []string{"RAW_TEST_CHILD=1"}
			start := time.Now()
			_, err := runParserProcess(ctx, cancel, cmd, []byte("{}"), 1024, 1024)
			if mode == "blocked" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, err, errParserProtocol)
			}
			require.NotNil(t, cmd.ProcessState)
			assert.Less(t, time.Since(start), 3*time.Second)
			assert.NotContains(t, err.Error(), strings.Repeat("x", 16))
		})
	}
}

func TestSandboxProbeRequiresSourceSentinel(t *testing.T) {
	root := t.TempDir()
	assert.Error(t, verifyParserProbe(root))
	require.NoError(t, os.WriteFile(filepath.Join(root, parserProbeFile), []byte("wrong source"), 0400))
	assert.Error(t, verifyParserProbe(root))
	require.NoError(t, os.Remove(filepath.Join(root, parserProbeFile)))
	require.NoError(t, os.WriteFile(filepath.Join(root, parserProbeFile), []byte(parserProbeContent), 0400))
	require.NoError(t, verifyParserProbe(root))
}

func TestSandboxTombstoneRequiresCanonicalValidation(t *testing.T) {
	p, err := NewSubprocessParser(time.Second)
	require.NoError(t, err)
	_, err = p.Parse(t.Context(), rawsync.CanonicalManifest{Manifest: rawsync.Manifest{Kind: rawsync.ManifestTombstone}}, nil)
	require.ErrorIs(t, err, rawsync.ErrInvalid)
}
