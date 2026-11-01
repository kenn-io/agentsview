package rawderive

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	in := ParsedManifest{ReplaceSessionContent: true, Outcome: parser.ParseOutcome{ForceReplace: true, ResultSetComplete: false, ExcludedSessionIDs: []string{"excluded"}, SourceErrors: []parser.SourceError{{SessionID: "failed", SourceKey: "source", Retryable: true, Err: errors.New("private source contents")}}, Results: []parser.ParseResultOutcome{{DataVersion: parser.DataVersionNeedsRetry, RetryReason: "retry", Result: parser.ParseResult{Session: parser.ParsedSession{ID: "good"}}}}}}
	in.Outcome.Results[0].Result.Messages = []parser.ParsedMessage{{ToolCalls: []parser.ParsedToolCall{{ResultEvents: []parser.ParsedToolResultEvent{{RawContentDigest: []byte{1, 2, 3}, Content: "result"}}}}}}
	var encoded bytes.Buffer
	require.NoError(t, encodeParserOutcome(&encoded, in))
	assert.NotContains(t, encoded.String(), "private source contents")
	out, err := decodeParserOutcome(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, []string{"excluded"}, out.Outcome.ExcludedSessionIDs)
	assert.Equal(t, []byte{1, 2, 3}, out.Outcome.Results[0].Result.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest)
	assert.True(t, out.Outcome.ForceReplace)
	assert.True(t, out.ReplaceSessionContent)
	assert.False(t, out.Outcome.ResultSetComplete)
	require.Len(t, out.Outcome.SourceErrors, 1)
	assert.True(t, out.Outcome.SourceErrors[0].Retryable)
	assert.Equal(t, "failed", out.Outcome.SourceErrors[0].SessionID)
	require.Error(t, out.Outcome.SourceErrors[0].Err)
	assert.Equal(t, parser.DataVersionNeedsRetry, out.Outcome.Results[0].DataVersion)
	_, err = decodeParserOutcome(io.MultiReader(bytes.NewReader(encoded.Bytes()), strings.NewReader(" {}")))
	require.Error(t, err)
	_, err = decodeParserOutcome(strings.NewReader(`{"Unexpected":true}`))
	require.ErrorIs(t, err, errParserProtocol)
}

func TestParserProcessHarness(t *testing.T) {
	if os.Getenv("RAW_TEST_CHILD") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "before_ready":
		<-time.After(time.Hour)
	case "blocked", "blocked_stdin":
		os.Stdout.WriteString("READY\n")
		<-time.After(time.Hour)
	case "stdout":
		os.Stdout.WriteString("READY\n")
		for {
			os.Stdout.Write(bytes.Repeat([]byte(" "), 4096))
		}
	case "stderr":
		os.Stdout.WriteString("READY\n")
		for {
			os.Stderr.Write(bytes.Repeat([]byte("x"), 4096))
		}
	case "wrong_ready":
		os.Stdout.WriteString("WRONG\n")
		<-time.After(time.Hour)
	case "malformed":
		os.Stdout.WriteString("READY\n{!")
		<-time.After(time.Hour)
	case "closed_stdout", "nonzero", "success":
		os.Stdout.WriteString("READY\n")
		io.Copy(io.Discard, os.Stdin)
		if err := encodeParserOutcome(os.Stdout, ParsedManifest{Tombstone: true}); err != nil {
			os.Exit(79)
		}
		if os.Args[len(os.Args)-1] == "closed_stdout" {
			os.Stdout.Close()
			<-time.After(time.Hour)
		}
		if os.Args[len(os.Args)-1] == "nonzero" {
			os.Exit(2)
		}
	case "size_exit":
		os.Stdout.WriteString("READY\n")
		io.Copy(io.Discard, os.Stdin)
		os.Exit(parserSizeLimitExitCode)
	case "at_cap", "below_cap", "above_cap":
		os.Stdout.WriteString("READY\n")
		io.Copy(io.Discard, os.Stdin)
		padding := 1022
		if os.Args[len(os.Args)-1] == "below_cap" {
			padding--
		} else if os.Args[len(os.Args)-1] == "above_cap" {
			padding++
		}
		os.Stdout.WriteString("{}" + strings.Repeat(" ", padding))
	}
	os.Exit(0)
}

func TestParserProcessKillsAndReaps(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"before_ready", context.DeadlineExceeded},
		{"blocked", context.DeadlineExceeded},
		{"blocked_stdin", context.DeadlineExceeded},
		{"closed_stdout", context.DeadlineExceeded},
		{"stdout", ErrSizeLimit},
		{"stderr", errParserProtocol},
		{"wrong_ready", errParserProtocol},
		{"malformed", errParserProtocol},
		{"size_exit", ErrSizeLimit},
		{"nonzero", errParserFailed},
		{"success", nil},
		{"below_cap", nil},
		{"at_cap", nil},
		{"above_cap", ErrSizeLimit},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			timeout := 3 * time.Second
			if errors.Is(tc.want, context.DeadlineExceeded) && tc.mode != "closed_stdout" {
				// These cases assert the deadline stopping a deliberately blocked child.
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			childCtx, stopChild := context.WithCancel(ctx)
			defer stopChild()
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestParserProcessHarness$", "--", tc.mode)
			cmd.Env = []string{"RAW_TEST_CHILD=1"}
			input := []byte("{}")
			if tc.mode == "blocked_stdin" {
				input = bytes.Repeat([]byte("x"), 1<<20)
			}
			decodedBeforeDeadline := false
			err := runParserProcess(ctx, stopChild, cmd, input, 1024, 1024, func(src io.Reader) error {
				_, err := decodeParserOutcome(src)
				decodedBeforeDeadline = err == nil && ctx.Err() == nil
				return err
			})
			if tc.mode == "closed_stdout" {
				assert.True(t, decodedBeforeDeadline, "stdout must reach EOF before the deadline kills the still-running child")
			}
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.NotContains(t, err.Error(), strings.Repeat("x", 16))
			} else {
				require.NoError(t, err)
			}
			require.NotNil(t, cmd.ProcessState)
		})
	}
}

func TestSandboxWirePreservesAbsentAndEmptyCollections(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ParsedManifest
	}{
		{"absent", ParsedManifest{}},
		{"empty_results", ParsedManifest{Outcome: parser.ParseOutcome{Results: []parser.ParseResultOutcome{}, ExcludedSessionIDs: []string{}}}},
		{"absent_messages", ParsedManifest{Outcome: parser.ParseOutcome{Results: []parser.ParseResultOutcome{{}}}}},
		{"empty_messages", ParsedManifest{Outcome: parser.ParseOutcome{Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{Messages: []parser.ParsedMessage{}}}}}}},
		{"empty_calls", ParsedManifest{Outcome: parser.ParseOutcome{Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{Messages: []parser.ParsedMessage{{ToolCalls: []parser.ParsedToolCall{}}}}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			require.NoError(t, encodeParserOutcome(&wire, tc.in))
			out, err := decodeParserOutcome(&wire)
			require.NoError(t, err)
			assert.Equal(t, tc.in, out)
		})
	}
}

func TestParserProcessPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	childCtx, stopChild := context.WithCancel(ctx)
	defer stopChild()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestParserProcessHarness$", "--", "blocked")
	cmd.Env = []string{"RAW_TEST_CHILD=1"}
	err := runParserProcess(ctx, stopChild, cmd, []byte("{}"), 1024, 1024, func(src io.Reader) error {
		// The callback starts only after READY; cancellation interrupts a read
		// from an otherwise live child, independently of protocol rejection.
		cancel()
		_, err := io.Copy(io.Discard, src)
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, cmd.ProcessState)
}

func TestSandboxWireRejectsInvalidOutcome(t *testing.T) {
	for _, body := range []string{
		`{"Unexpected":true}`, `{} {}`, `{} garbage`, `{"Outcome":`,
		`{"Outcome":{"SourceErrors":[{}]}}`,
		`{"Outcome":{"Results":[{}]},"Digests":[[]]}`,
	} {
		t.Run(body, func(t *testing.T) {
			out, err := decodeParserOutcome(strings.NewReader(body))
			require.ErrorIs(t, err, errParserProtocol)
			assert.Equal(t, ParsedManifest{}, out)
		})
	}
}

func TestSandboxWirePreservesSizeFailure(t *testing.T) {
	var wire bytes.Buffer
	in := ParsedManifest{Outcome: parser.ParseOutcome{SourceErrors: []parser.SourceError{{Err: errors.Join(rawsync.ErrInvalid, ErrSizeLimit, errors.New("private path and content"))}}}}
	require.NoError(t, encodeParserOutcome(&wire, in))
	assert.NotContains(t, wire.String(), "private path")
	out, err := decodeParserOutcome(&wire)
	require.NoError(t, err)
	require.Len(t, out.Outcome.SourceErrors, 1)
	assert.ErrorIs(t, out.Outcome.SourceErrors[0].Err, ErrSizeLimit)
}

func TestParserOutputWriterEnforcesByteBoundary(t *testing.T) {
	for _, size := range []int{7, 8, 9} {
		t.Run(string(rune('0'+size)), func(t *testing.T) {
			var dst bytes.Buffer
			w := &parserOutputWriter{dst: &dst, remaining: 8}
			n, err := w.Write(bytes.Repeat([]byte("x"), size))
			if size > 8 {
				require.ErrorIs(t, err, ErrSizeLimit)
				assert.Zero(t, n)
				assert.Empty(t, dst.String())
			} else {
				require.NoError(t, err)
				assert.Equal(t, size, n)
				assert.Equal(t, strings.Repeat("x", size), dst.String())
			}
		})
	}
	t.Run("short_write", func(t *testing.T) {
		w := &parserOutputWriter{dst: parserShortWriter{}, remaining: 8}
		n, err := w.Write([]byte("xx"))
		require.ErrorIs(t, err, io.ErrShortWrite)
		assert.Equal(t, 1, n)
	})
}

type parserShortWriter struct{}

func (parserShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestSandboxProbeRequiresSourceSentinel(t *testing.T) {
	root := t.TempDir()
	require.Error(t, verifyParserProbe(root))
	require.NoError(t, os.WriteFile(filepath.Join(root, parserProbeFile), []byte("wrong source"), 0o400))
	require.Error(t, verifyParserProbe(root))
	require.NoError(t, os.Remove(filepath.Join(root, parserProbeFile)))
	require.NoError(t, os.WriteFile(filepath.Join(root, parserProbeFile), []byte(parserProbeContent), 0o400))
	require.NoError(t, verifyParserProbe(root))
}

func TestSandboxTombstoneRequiresCanonicalValidation(t *testing.T) {
	p, err := NewSubprocessParser(time.Second)
	require.NoError(t, err)
	_, err = p.Parse(t.Context(), rawsync.CanonicalManifest{Manifest: rawsync.Manifest{Kind: rawsync.ManifestTombstone}}, nil)
	require.ErrorIs(t, err, rawsync.ErrInvalid)
}
