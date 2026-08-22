package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENTSVIEW_CAPTURE_TEST_HELPER") == "1" {
		captureTestHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunClaudeProducesExactResultAndPreservesChildOutcome(t *testing.T) {
	for _, childExit := range []int{0, 7} {
		t.Run(fmt.Sprintf("exit_%d", childExit), func(t *testing.T) {
			root := t.TempDir()
			captureDir := filepath.Join(t.TempDir(), "capture")
			resultPath := filepath.Join(t.TempDir(), "usage.json")
			producer := copyCaptureHelper(t, "claude")
			var stdout, stderr bytes.Buffer
			limits := testLimits()
			outcome, err := Run(context.Background(), RunOptions{
				Provider:          ProviderClaude,
				OccurrenceID:      "job-42-attempt-1",
				CaptureDir:        captureDir,
				ResultPath:        resultPath,
				ProviderRoot:      root,
				WorkDir:           t.TempDir(),
				Command:           []string{producer, "-p", "PROMPT_SENTINEL"},
				Environment:       helperEnvironment(root, "claude-final", childExit),
				Streams:           Streams{Stdout: &stdout, Stderr: &stderr},
				Limits:            limits,
				CustomPricing:     testPricing(),
				AgentsViewVersion: "test-version",
			})
			require.NoError(t, err)
			assert.Equal(t, childExit, outcome.ExitCode)
			assert.Equal(t, "child stdout\n", stdout.String())
			assert.Equal(t, "child stderr\n", stderr.String())

			data, err := os.ReadFile(resultPath)
			require.NoError(t, err)
			assertPrivateMode(t, captureDir, 0o700)
			for _, path := range []string{
				filepath.Join(captureDir, manifestFileName),
				filepath.Join(captureDir, archiveFileName),
				filepath.Join(captureDir, sealedFileName),
				filepath.Join(captureDir, lockFileName), resultPath,
			} {
				assertPrivateMode(t, path, 0o600)
			}
			result, err := DecodeResult(bytes.NewReader(data))
			require.NoError(t, err)
			assert.Equal(t, "job-42-attempt-1", result.OccurrenceID)
			assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
			require.NotNil(t, result.Execution.ExitCode)
			assert.Equal(t, childExit, *result.Execution.ExitCode)
			require.NotNil(t, result.Usage)
			assertIntPointer(t, result.Usage.InputTokens, 100)
			assertIntPointer(t, result.Usage.OutputTokens, 50)
			assertIntPointer(t, result.Usage.CacheCreationInputTokens, 200)
			assertIntPointer(t, result.Usage.CacheReadInputTokens, 300)
			require.NotNil(t, result.Cost)
			assert.Equal(t, int64(650), result.Cost.Amount.Microdollars)
			assert.Equal(t, "USD", result.Cost.Currency)
			assert.Equal(t, []string{"claude-test"}, result.Models)

			text := string(data)
			for _, forbidden := range []string{
				"PROMPT_SENTINEL", "RESPONSE_SENTINEL", "ENV_SECRET_SENTINEL",
				"AUTHORIZATION_SENTINEL", root,
			} {
				assert.NotContains(t, text, forbidden)
			}

			var replay bytes.Buffer
			pricingLoaded := false
			reporting, err := Report(context.Background(), ReportOptions{
				CaptureDir: captureDir, ResultPath: "-", Stdout: &replay,
				LoadCustomPricing: func() (map[string]config.CustomModelRate, error) {
					pricingLoaded = true
					return nil, assert.AnError
				},
				AgentsViewVersion: "changed-version",
			})
			require.NoError(t, err)
			assert.False(t, pricingLoaded, "sealed replay must not reload pricing")
			assert.Equal(t, ReportingComplete, reporting.Outcome)
			assert.Equal(t, data, replay.Bytes(), "sealed replay must be byte-identical")
		})
	}
}

func TestRunInvalidatesExistingResultBeforeStartingProducer(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	require.NoError(t, os.WriteFile(resultPath, []byte("stale result"), 0o600))
	env := append(
		helperEnvironment(root, "claude-final", 0),
		"AGENTSVIEW_CAPTURE_TEST_RESULT_MUST_BE_ABSENT="+resultPath,
	)

	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "current-occurrence",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{copyCaptureHelper(t, "claude"), "-p", "prompt"},
		Environment: env, Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits: testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "current-occurrence", result.OccurrenceID)
}

func TestReportPricingFailureWritesOrReplaysFailureResult(t *testing.T) {
	tests := []struct {
		name        string
		priorReason ReasonCode
		wantReason  ReasonCode
	}{
		{name: "new failure", wantReason: ReasonIngestFailed},
		{
			name:        "prior failure",
			priorReason: ReasonNoSession, wantReason: ReasonNoSession,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now().UTC()
			captureDir := filepath.Join(t.TempDir(), "capture")
			resultPath := filepath.Join(t.TempDir(), "usage.json")
			state, err := createState(captureDir, manifest{
				OccurrenceID:      "pricing-load-failure",
				Provider:          string(ProviderClaude),
				ProviderSessionID: "11111111-1111-4111-8111-111111111111",
				ProviderRoot:      t.TempDir(),
				ProviderWorkDir:   t.TempDir(),
				StartedAt:         started,
				Execution:         ExecutionOutcome{StartedAt: started},
				Invocation:        invocationName(ProviderClaude),
				Limits:            testLimits(),
			})
			require.NoError(t, err)
			var priorData []byte
			if test.priorReason != "" {
				prior := failureResult(state.manifest, test.priorReason, "test")
				priorData, err = encodeResult(
					prior, state.manifest.Limits.MaxResultBytes,
				)
				require.NoError(t, err)
				require.NoError(t, state.storeAttempt(priorData, false))
			}
			state.close()

			reporting, err := Report(t.Context(), ReportOptions{
				CaptureDir: captureDir, ResultPath: resultPath,
				LoadCustomPricing: func() (map[string]config.CustomModelRate, error) {
					return nil, assert.AnError
				},
				AgentsViewVersion: "test",
			})

			require.ErrorContains(t, err, assert.AnError.Error())
			assert.Equal(t, ReportingFailed, reporting.Outcome)
			assert.Equal(t, test.wantReason, reporting.Reason)
			data, readErr := os.ReadFile(resultPath)
			require.NoError(t, readErr)
			result, decodeErr := DecodeResult(bytes.NewReader(data))
			require.NoError(t, decodeErr)
			assert.Equal(t, test.wantReason, result.Reporting.Reason)
			if priorData != nil {
				assert.Equal(t, priorData, data)
			}
		})
	}
}

func TestRunFinalizesUsageAfterPostStartStreamError(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	producer := copyCaptureHelper(t, "claude")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "post-start-stream-error",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p"},
		Environment: helperEnvironment(root, "claude-stdin", 0),
		Streams: Streams{
			Stdin: iotest.ErrReader(assert.AnError), Stdout: io.Discard, Stderr: io.Discard,
		},
		Limits: testLimits(), CustomPricing: testPricing(),
	})

	require.ErrorContains(t, err, assert.AnError.Error())
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	assert.Equal(t, ReportingComplete, outcome.Reporting.Outcome)
	require.NotNil(t, outcome.Execution.ExitCode)
	assert.Zero(t, *outcome.Execution.ExitCode)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 100)
}

func TestRunPersistsProducerLaunchFailure(t *testing.T) {
	for _, test := range []struct {
		provider Provider
		name     string
		args     []string
	}{
		{provider: ProviderClaude, name: "claude", args: []string{"-p", "prompt"}},
		{provider: ProviderCodex, name: "codex", args: []string{"exec", "--json", "prompt"}},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			captureDir := filepath.Join(t.TempDir(), "capture")
			resultPath := filepath.Join(t.TempDir(), "usage.json")
			missingProducer := filepath.Join(t.TempDir(), test.name)

			outcome, err := Run(t.Context(), RunOptions{
				Provider: test.provider, OccurrenceID: "launch-failure-" + test.name,
				CaptureDir: captureDir, ResultPath: resultPath,
				ProviderRoot: t.TempDir(), WorkDir: t.TempDir(),
				Command: append([]string{missingProducer}, test.args...),
				Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:  testLimits(),
			})

			require.ErrorContains(t, err, "starting producer")
			assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
			assert.Equal(t, ReportingFailed, outcome.Reporting.Outcome)
			assert.DirExists(t, captureDir)
			data, readErr := os.ReadFile(resultPath)
			require.NoError(t, readErr)
			result, decodeErr := DecodeResult(bytes.NewReader(data))
			require.NoError(t, decodeErr)
			assert.Equal(t, ReasonChildStartFailed, result.Reporting.Reason)
			assert.Nil(t, result.Usage)
			assert.Nil(t, result.Execution.ExitCode)
			require.NotNil(t, result.Execution.CompletedAt)
		})
	}
}

func TestReportReplaysProducerLaunchFailureWithoutSourceDiscovery(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	initialResultPath := filepath.Join(t.TempDir(), "initial.json")
	missingProducer := filepath.Join(t.TempDir(), "claude")

	_, err := Run(t.Context(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "durable-launch-failure",
		CaptureDir: captureDir, ResultPath: initialResultPath,
		ProviderRoot: root, WorkDir: workDir,
		Command: []string{missingProducer, "-p", "prompt"},
		Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:  testLimits(),
	})
	require.ErrorContains(t, err, "starting producer")
	initialData, err := os.ReadFile(initialResultPath)
	require.NoError(t, err)

	manifestData, err := os.ReadFile(filepath.Join(captureDir, manifestFileName))
	require.NoError(t, err)
	var captured manifest
	require.NoError(t, json.Unmarshal(manifestData, &captured))
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	transcriptPath := filepath.Join(
		root, encodeClaudeWorkDir(physicalWorkDir), captured.ProviderSessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o700))
	require.NoError(t, os.WriteFile(
		transcriptPath,
		[]byte(strings.Join(
			claudeHelperLines(captured.ProviderSessionID, physicalWorkDir, false), "\n",
		)+"\n"),
		0o600,
	))

	pricingLoaded := false
	retryResultPath := filepath.Join(t.TempDir(), "retry.json")
	reporting, reportErr := Report(t.Context(), ReportOptions{
		CaptureDir: captureDir, ResultPath: retryResultPath,
		LoadCustomPricing: func() (map[string]config.CustomModelRate, error) {
			pricingLoaded = true
			return testPricing(), nil
		},
	})

	require.Error(t, reportErr)
	assert.False(t, pricingLoaded)
	assert.Equal(t, ReportingFailed, reporting.Outcome)
	assert.Equal(t, ReasonChildStartFailed, reporting.Reason)
	retryData, err := os.ReadFile(retryResultPath)
	require.NoError(t, err)
	assert.Equal(t, initialData, retryData)
}

func TestClaudeCapturePersistsARecoverableProviderShapedBundle(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "persisted-bundle",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: workDir,
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)

	resultData, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(resultData))
	require.NoError(t, err)
	require.Len(t, result.Sources, 2)

	bundlePath := filepath.Join(captureDir, sourcesDirName, bundleFileName)
	bundleFile, err := os.Open(bundlePath)
	require.NoError(t, err)
	bundle, err := DecodeTranscriptBundle(bundleFile)
	require.NoError(t, bundleFile.Close())
	require.NoError(t, err)
	assert.Equal(t, "persisted-bundle", bundle.OccurrenceID)
	assert.Equal(t, ProviderClaude, bundle.Provider)
	require.Len(t, bundle.Sources, 2)

	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	wantRoot := filepath.ToSlash(filepath.Join(
		string(ProviderClaude), "projects", encodeClaudeWorkDir(physicalWorkDir),
		result.Provider.SessionID+".jsonl",
	))
	wantChild := filepath.ToSlash(filepath.Join(
		string(ProviderClaude), "projects", encodeClaudeWorkDir(physicalWorkDir),
		result.Provider.SessionID, "subagents", "agent-abc123.jsonl",
	))
	assert.ElementsMatch(t, []string{wantRoot, wantChild}, []string{
		bundle.Sources[0].RawSource.Path, bundle.Sources[1].RawSource.Path,
	})
	assertPrivateMode(t, filepath.Join(captureDir, sourcesDirName), 0o700)
	assertPrivateMode(t, bundlePath, 0o600)

	resultBySession := make(map[string]SourceProvenance, len(result.Sources))
	for _, source := range result.Sources {
		resultBySession[source.SessionID] = source
	}
	for _, source := range bundle.Sources {
		path := filepath.Join(
			captureDir, sourcesDirName, filepath.FromSlash(source.RawSource.Path))
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assertPrivateMode(t, path, 0o600)
		digest := sha256.Sum256(data)
		assert.Equal(t, fmt.Sprintf("%x", digest), source.RawSource.Hash)
		assert.Equal(t, int64(len(data)), source.RawSource.Size)
		assert.Equal(t, SourceProvenance{
			SessionID: source.SessionID,
			SHA256:    source.RawSource.Hash,
			Bytes:     source.RawSource.Size,
		}, resultBySession[source.SessionID])
	}

	text := string(resultData)
	assert.NotContains(t, text, "projects/")
	assert.NotContains(t, text, physicalWorkDir)
	assertBundleImportsUsage(
		t,
		filepath.Join(captureDir, "sources", "claude", "projects"),
		parser.AgentClaude,
		result.Provider.SessionID,
		result,
	)
}

func TestReportRebuildsFromPersistedSourcesAfterLiveArchiveIsGone(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "ephemeral-runner-recovery",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	originalData, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	original, err := DecodeResult(bytes.NewReader(originalData))
	require.NoError(t, err)

	state, err := openState(captureDir)
	require.NoError(t, err)
	state.manifest.SealedDigest = ""
	require.NoError(t, state.saveManifest())
	state.close()
	require.NoError(t, os.Rename(root, root+"-provider-archive-gone"))
	require.NoError(t, os.Rename(
		filepath.Join(captureDir, archiveFileName),
		filepath.Join(t.TempDir(), "prior-capture.db"),
	))

	var recoveredJSON bytes.Buffer
	reporting, err := Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: "-", Stdout: &recoveredJSON,
		CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, ReportingComplete, reporting.Outcome)
	recovered, err := DecodeResult(bytes.NewReader(recoveredJSON.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, original.Usage, recovered.Usage)
	assert.Equal(t, original.Cost, recovered.Cost)
	assert.Equal(t, original.Sources, recovered.Sources)
	assert.Equal(t, original.Provider.IncludedSessionIDs,
		recovered.Provider.IncludedSessionIDs)
}

func TestRunClaudePassesStandardInputThrough(t *testing.T) {
	root := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	var stdout bytes.Buffer
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "stdin",
		CaptureDir:   filepath.Join(t.TempDir(), "capture"),
		ResultPath:   filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p"},
		Environment: helperEnvironment(root, "claude-stdin", 0),
		Streams: Streams{
			Stdin: strings.NewReader("prompt from stdin"), Stdout: &stdout, Stderr: io.Discard,
		},
		Limits: testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "stdin:prompt from stdin\n")
}

func TestRunRejectsReuseOfCompletedCapture(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	options := RunOptions{
		Provider: ProviderClaude, OccurrenceID: "one-occurrence",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	}
	_, err := Run(context.Background(), options)
	require.NoError(t, err)
	before, err := os.ReadFile(resultPath)
	require.NoError(t, err)

	_, err = Run(context.Background(), options)
	require.ErrorContains(t, err, "capture directory already exists")
	after, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestRunRejectsPreexistingCaptureDirectoryWithoutChangingIt(t *testing.T) {
	captureDir := t.TempDir()
	sentinelPath := filepath.Join(captureDir, "unrelated.txt")
	require.NoError(t, os.WriteFile(sentinelPath, []byte("keep me"), 0o600))
	producer := copyCaptureHelper(t, "claude")

	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "existing-directory",
		CaptureDir: captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: t.TempDir(), WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(t.TempDir(), "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(),
	})

	require.ErrorContains(t, err, "capture directory already exists")
	data, readErr := os.ReadFile(sentinelPath)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(data))
	assert.NoFileExists(t, filepath.Join(captureDir, manifestFileName))
	assert.NoFileExists(t, filepath.Join(captureDir, lockFileName))
}

func TestReportRejectsInvalidStateBeforeCreatingRecoveryFiles(t *testing.T) {
	captureDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(captureDir, manifestFileName), []byte("not json"), 0o600))
	resultPath := filepath.Join(t.TempDir(), "result.json")

	_, err := Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: resultPath,
	})

	require.ErrorContains(t, err, "decoding capture manifest")
	assert.NoFileExists(t, filepath.Join(captureDir, lockFileName))
	assert.NoFileExists(t, resultPath)
}

func TestReportRejectsUnexpectedCaptureContentsWithoutRemovingThem(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "unexpected-recovery-state",
		CaptureDir: captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	unrelated := filepath.Join(captureDir, "unrelated.txt")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep me"), 0o600))

	_, err = Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: "-", Stdout: io.Discard,
	})

	require.ErrorContains(t, err, "unexpected entry")
	data, readErr := os.ReadFile(unrelated)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(data))
}

func TestRunRejectsHistoricalClaudeSessionSources(t *testing.T) {
	for _, existing := range []string{"root", "subagent tree", "other project"} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			workDir := t.TempDir()
			physicalWorkDir, err := filepath.EvalSymlinks(workDir)
			require.NoError(t, err)
			sessionID := "33333333-3333-4333-8333-333333333333"
			rootPath := filepath.Join(
				root, encodeClaudeWorkDir(physicalWorkDir), sessionID+".jsonl")
			if existing == "other project" {
				rootPath = filepath.Join(
					root, "different-project", sessionID+".jsonl")
			}
			if existing == "root" || existing == "other project" {
				require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o700))
				require.NoError(t, os.WriteFile(rootPath, []byte("historical\n"), 0o600))
			} else {
				require.NoError(t, os.MkdirAll(
					strings.TrimSuffix(rootPath, ".jsonl"), 0o700))
			}
			captureDir := filepath.Join(t.TempDir(), "capture")
			producer := copyCaptureHelper(t, "run-claude-ci")

			_, err = Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: "historical-session",
				ProviderSessionID: sessionID,
				CaptureDir:        captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
				ProviderRoot: root, ClaudeWorkDir: workDir,
				WorkDir: workDir, Command: []string{producer},
				Environment: helperEnvironment(root, "claude-final", 0),
				Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:      testLimits(),
			})

			require.ErrorContains(t, err, "already has provider source data")
			assert.NoDirExists(t, captureDir)
		})
	}
}

func TestProviderRootsDoNotRequireHomeWhenConfigured(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	claudeRoot := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeRoot)
	got, err := producerRoot(ProviderClaude, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(claudeRoot, "projects"), got)

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	codexRoot := t.TempDir()
	t.Setenv("CODEX_HOME", codexRoot)
	got, err = producerRoot(ProviderCodex, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(codexRoot, "sessions"), got)

	explicit := t.TempDir()
	got, err = producerRoot(ProviderClaude, explicit)
	require.NoError(t, err)
	assert.Equal(t, explicit, got)
}

func TestCaptureRejectsResultPathsInsideCaptureState(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "result-collision",
		CaptureDir:   captureDir,
		ResultPath:   filepath.Join(captureDir, sourcesDirName, bundleFileName),
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(),
	})
	require.ErrorContains(t, err, "result path must be outside")
	assert.NoDirExists(t, captureDir)

	externalResult := filepath.Join(t.TempDir(), "result.json")
	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "report-result-collision",
		CaptureDir: captureDir, ResultPath: externalResult,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	manifestPath := filepath.Join(captureDir, manifestFileName)
	before, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	_, err = Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: manifestPath,
	})
	require.ErrorContains(t, err, "result path must be outside")
	after, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	alias := filepath.Join(t.TempDir(), "capture-alias")
	if err := os.Symlink(captureDir, alias); err == nil {
		_, err = Report(context.Background(), ReportOptions{
			CaptureDir: captureDir,
			ResultPath: filepath.Join(alias, manifestFileName),
		})
		require.ErrorContains(t, err, "result path must be outside")
	}
}

func TestRunRejectsResultPathInsideProviderRootBeforeStarting(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	workDir := t.TempDir()
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	sessionID := "11111111-2222-4333-8444-555555555555"
	resultPath := filepath.Join(
		root, encodeClaudeWorkDir(physicalWorkDir), sessionID+".jsonl")
	producer := copyCaptureHelper(t, "claude")
	var stdout bytes.Buffer

	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "provider-result-collision",
		ProviderSessionID: sessionID,
		CaptureDir:        captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: workDir,
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: &stdout, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})

	require.ErrorContains(t, err, "result path must be outside the provider root")
	assert.NoDirExists(t, captureDir)
	assert.NoFileExists(t, resultPath)
	assert.Empty(t, stdout.String())
}

func TestRunRejectsResultDirectoryBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "usage.json")
	require.NoError(t, os.Mkdir(resultPath, 0o700))
	producer := copyCaptureHelper(t, "claude")
	var stdout bytes.Buffer
	opts := RunOptions{
		Provider: ProviderClaude, OccurrenceID: "invalid-result-target",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: &stdout, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	}

	_, err := Run(context.Background(), opts)

	require.ErrorContains(t, err, "not a regular file")
	assert.DirExists(t, resultPath)
	assert.NoDirExists(t, captureDir)
	assert.Empty(t, stdout.String(), "the producer must not start")

	opts.ResultPath = filepath.Join(t.TempDir(), "usage.json")
	_, err = Run(context.Background(), opts)
	require.NoError(t, err)
	assert.FileExists(t, opts.ResultPath)
}

func TestRunRejectsOverlappingCaptureAndProviderRoots(t *testing.T) {
	producer := copyCaptureHelper(t, "claude")
	tests := []struct {
		name         string
		capturePath  string
		providerPath string
	}{
		{
			name:         "provider root inside capture directory",
			capturePath:  "capture",
			providerPath: filepath.Join("capture", "provider"),
		},
		{
			name:         "capture directory inside provider root",
			capturePath:  filepath.Join("provider", "capture"),
			providerPath: "provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			captureDir := filepath.Join(base, tt.capturePath)
			var stdout bytes.Buffer

			_, err := Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: "overlapping-roots",
				CaptureDir:   captureDir,
				ResultPath:   filepath.Join(t.TempDir(), "result.json"),
				ProviderRoot: filepath.Join(base, tt.providerPath),
				WorkDir:      t.TempDir(),
				Command:      []string{producer, "-p", "prompt"},
				Environment:  helperEnvironment(t.TempDir(), "claude-final", 0),
				Streams:      Streams{Stdout: &stdout, Stderr: io.Discard},
				Limits:       testLimits(),
			})

			require.ErrorContains(t, err, "must not overlap")
			assert.Empty(t, stdout.String(), "the producer must not start")
			assert.NoDirExists(t, captureDir)
		})
	}
}

func TestRunClaudeWrapperRequiresAndUsesCallerSessionID(t *testing.T) {
	root := t.TempDir()
	producer := copyCaptureHelper(t, "run-claude-ci")
	claudeWorkDir := t.TempDir()
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "wrapper-missing-id",
		CaptureDir:   filepath.Join(t.TempDir(), "capture"),
		ResultPath:   filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: root, ClaudeWorkDir: claudeWorkDir,
		WorkDir: t.TempDir(), Command: []string{producer},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard}, Limits: testLimits(),
	})
	require.ErrorContains(t, err, "requires --session-id")

	sessionID := "33333333-3333-4333-8333-333333333333"
	env := append(
		helperEnvironment(root, "claude-final", 0),
		"AGENTSVIEW_CAPTURE_TEST_SESSION_ID="+sessionID,
		"AGENTSVIEW_CAPTURE_TEST_CHDIR="+claudeWorkDir,
	)
	missingWorkDirCapture := filepath.Join(t.TempDir(), "capture")
	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "wrapper-missing-work-dir",
		ProviderSessionID: sessionID,
		CaptureDir:        missingWorkDirCapture,
		ResultPath:        filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot:      root, WorkDir: t.TempDir(), Command: []string{producer},
		Environment: env, Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits: testLimits(),
	})
	require.ErrorContains(t, err, "requires --claude-work-dir")
	assert.NoDirExists(t, missingWorkDirCapture)

	resultPath := filepath.Join(t.TempDir(), "result.json")
	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "wrapper",
		ProviderSessionID: sessionID,
		CaptureDir:        filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, ClaudeWorkDir: claudeWorkDir,
		WorkDir: t.TempDir(), Command: []string{producer},
		Environment: env, Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits: testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, sessionID, result.Provider.SessionID)
}

func TestRunRejectsUppercaseClaudeSessionIDsBeforeStarting(t *testing.T) {
	uppercaseID := "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
	for _, test := range []struct {
		name       string
		executable string
		command    func(string) []string
		supplied   string
	}{
		{
			name: "direct argument", executable: "claude",
			command: func(path string) []string {
				return []string{path, "--session-id", uppercaseID, "-p", "prompt"}
			},
		},
		{
			name: "wrapper option", executable: "run-claude-ci",
			command: func(path string) []string {
				return []string{path, uppercaseID}
			},
			supplied: uppercaseID,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			producer := copyCaptureHelper(t, test.executable)
			captureDir := filepath.Join(t.TempDir(), "capture")
			_, err := Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: "uppercase-session",
				ProviderSessionID: test.supplied,
				CaptureDir:        captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
				ProviderRoot: t.TempDir(), WorkDir: t.TempDir(),
				Command: test.command(producer),
				Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:  testLimits(),
			})

			require.ErrorContains(t, err, "must use lowercase hexadecimal")
			assert.NoDirExists(t, captureDir)
		})
	}
}

func TestRunRejectsUnsupportedSessionModesBeforeStarting(t *testing.T) {
	tests := []struct {
		name       string
		provider   Provider
		executable string
		arguments  []string
		wantError  string
	}{
		{
			name: "claude continue", provider: ProviderClaude, executable: "claude",
			arguments: []string{"-p", "--continue", "prompt"}, wantError: "does not support",
		},
		{
			name: "claude resume", provider: ProviderClaude, executable: "claude",
			arguments: []string{
				"-p", "--resume=11111111-1111-4111-8111-111111111111", "prompt",
			}, wantError: "does not support",
		},
		{
			name: "claude fork", provider: ProviderClaude, executable: "claude",
			arguments: []string{"-p", "--fork-session", "prompt"}, wantError: "does not support",
		},
		{
			name: "claude without persistence", provider: ProviderClaude, executable: "claude",
			arguments: []string{"-p", "--no-session-persistence", "prompt"},
			wantError: "requires transcript persistence",
		},
		{
			name: "codex resume", provider: ProviderCodex, executable: "codex",
			arguments: []string{"exec", "resume", "--json", "--last", "prompt"},
			wantError: "does not support",
		},
		{
			name: "codex resume after options", provider: ProviderCodex, executable: "codex",
			arguments: []string{
				"exec", "--json", "-c", `model="gpt-test"`, "resume", "--last", "prompt",
			}, wantError: "does not support",
		},
		{
			name: "codex ephemeral", provider: ProviderCodex, executable: "codex",
			arguments: []string{"exec", "--json", "--ephemeral", "prompt"},
			wantError: "requires transcript persistence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			producer := copyCaptureHelper(t, test.executable)
			captureDir := filepath.Join(t.TempDir(), "capture")
			root := t.TempDir()
			var stdout bytes.Buffer
			_, err := Run(context.Background(), RunOptions{
				Provider: test.provider, OccurrenceID: "unsupported-session-mode",
				CaptureDir: captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
				ProviderRoot: root, WorkDir: t.TempDir(),
				Command:     append([]string{producer}, test.arguments...),
				Environment: helperEnvironment(root, "none", 0),
				Streams:     Streams{Stdout: &stdout, Stderr: io.Discard}, Limits: testLimits(),
			})

			require.ErrorContains(t, err, test.wantError)
			assert.Empty(t, stdout.String(), "the producer must not start")
			assert.NoDirExists(t, captureDir)
		})
	}

	producer := copyCaptureHelper(t, "codex")
	_, err := prepareInvocation(
		ProviderCodex,
		[]string{producer, "exec", "--json", "--", "resume"},
		"",
	)
	require.NoError(t, err, "resume after -- is prompt text, not a subcommand")
}

func TestRunRejectsImpossibleTimingBeforeStarting(t *testing.T) {
	producer := copyCaptureHelper(t, "claude")
	limits := testLimits()
	limits.FinalizationWait = time.Second
	limits.Quiescence = time.Second

	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "invalid-timing",
		CaptureDir:   filepath.Join(t.TempDir(), "capture"),
		ResultPath:   filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: t.TempDir(), WorkDir: t.TempDir(),
		Command: []string{producer, "-p", "prompt"}, Limits: limits,
	})

	require.ErrorContains(t, err, "quiescence must be shorter")
}

func TestPruneUnlistedSourcesRemovesInterruptedAtomicWrite(t *testing.T) {
	state := &captureState{dir: t.TempDir(), manifest: manifest{Limits: testLimits()}}
	require.NoError(t, os.MkdirAll(state.sourcesPath(), 0o700))
	leftover := state.sourcesPath(".agentsview-capture-interrupted")
	require.NoError(t, os.WriteFile(leftover, []byte("partial"), 0o600))

	require.NoError(t, state.pruneUnlistedSources(t.Context()))

	assert.NoFileExists(t, leftover)
}

func TestResetPersistedSourcesRemovesIncompleteAttemptCopies(t *testing.T) {
	state := &captureState{
		dir: t.TempDir(),
		manifest: manifest{
			Limits:          testLimits(),
			SourcesComplete: true,
			Sources: []TranscriptSource{{
				SessionID: "old-session",
				RawSource: artifact.RawSourceRef{
					Path: "claude/projects/project-a/old-session.jsonl",
				},
			}},
		},
	}
	recorded := state.sourcesPath(
		"claude", "projects", "project-a", "old-session.jsonl",
	)
	uncommitted := state.sourcesPath(
		"claude", "projects", "project-b", "new-session.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(recorded), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(uncommitted), 0o700))
	require.NoError(t, os.WriteFile(recorded, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(uncommitted, []byte("new\n"), 0o600))
	require.NoError(t, os.WriteFile(state.bundlePath(), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(state.archivePath(), []byte("db"), 0o600))

	require.NoError(t, state.resetPersistedSources(t.Context()))

	assert.NoDirExists(t, state.sourcesPath())
	assert.NoFileExists(t, state.archivePath())
	assert.Empty(t, state.manifest.Sources)
	assert.False(t, state.manifest.SourcesComplete)
	data, err := os.ReadFile(state.manifestPath())
	require.NoError(t, err)
	var persisted manifest
	require.NoError(t, json.Unmarshal(data, &persisted))
	assert.Empty(t, persisted.Sources)
	assert.False(t, persisted.SourcesComplete)
}

func TestRunClaudeReportingFailuresAreDistinctAndWriteResults(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		limits     Limits
		wantReason ReasonCode
	}{
		{
			name: "no session", mode: "none",
			limits: func() Limits {
				limits := testLimits()
				limits.FinalizationWait = time.Second
				return limits
			}(),
			wantReason: ReasonNoSession,
		},
		{
			name: "finalization timeout", mode: "claude-final",
			limits: func() Limits {
				limits := testLimits()
				limits.FinalizationWait = 40 * time.Millisecond
				limits.Quiescence = 20 * time.Millisecond
				return limits
			}(),
			wantReason: ReasonFinalizationTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			resultPath := filepath.Join(t.TempDir(), "result.json")
			producer := copyCaptureHelper(t, "claude")
			outcome, err := Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: "failure-case",
				CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
				ProviderRoot: root, WorkDir: t.TempDir(),
				Command:     []string{producer, "-p", "ignored"},
				Environment: helperEnvironment(root, tt.mode, 0),
				Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:      tt.limits, CustomPricing: testPricing(),
			})
			require.Error(t, err)
			assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
			data, readErr := os.ReadFile(resultPath)
			require.NoError(t, readErr)
			result, decodeErr := DecodeResult(bytes.NewReader(data))
			require.NoError(t, decodeErr)
			assert.Equal(t, ReportingFailed, result.Reporting.Outcome)
			assert.Equal(t, tt.wantReason, result.Reporting.Reason)
			assert.Nil(t, result.Usage)
		})
	}
}

func TestFailedReportRetryPreservesFirstFailureUntilSuccess(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	captureDir := filepath.Join(t.TempDir(), "capture")
	firstResultPath := filepath.Join(t.TempDir(), "first.json")
	limits := testLimits()
	limits.FinalizationWait = 5 * time.Second
	limits.MaxSourceBytes = 8 << 10
	producer := copyCaptureHelper(t, "claude")

	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "stable-failure",
		CaptureDir: captureDir, ResultPath: firstResultPath,
		ProviderRoot: root, WorkDir: workDir,
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "none", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.Error(t, err)
	firstData, err := os.ReadFile(firstResultPath)
	require.NoError(t, err)
	first, err := DecodeResult(bytes.NewReader(firstData))
	require.NoError(t, err)
	require.Equal(t, ReasonNoSession, first.Reporting.Reason)

	manifestData, err := os.ReadFile(filepath.Join(captureDir, manifestFileName))
	require.NoError(t, err)
	var captured manifest
	require.NoError(t, json.Unmarshal(manifestData, &captured))
	sourcePath := filepath.Join(
		root, encodeClaudeWorkDir(physicalWorkDir), captured.ProviderSessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
	require.NoError(t, os.WriteFile(
		sourcePath, bytes.Repeat([]byte("x"), int(limits.MaxSourceBytes)+1), 0o600))

	retryPath := filepath.Join(t.TempDir(), "retry.json")
	reporting, err := Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: retryPath,
		CustomPricing: testPricing(),
	})
	require.Error(t, err)
	assert.Equal(t, first.Reporting, reporting)
	retryData, err := os.ReadFile(retryPath)
	require.NoError(t, err)
	assert.Equal(t, firstData, retryData)
	storedData, err := os.ReadFile(filepath.Join(captureDir, sealedFileName))
	require.NoError(t, err)
	assert.Equal(t, firstData, storedData)

	validData := []byte(strings.Join(
		claudeHelperLines(captured.ProviderSessionID, physicalWorkDir, false), "\n") + "\n")
	require.Less(t, int64(len(validData)), limits.MaxSourceBytes)
	require.NoError(t, os.WriteFile(sourcePath, validData, 0o600))
	successPath := filepath.Join(t.TempDir(), "success.json")
	reporting, err = Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: successPath,
		CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, ReportingComplete, reporting.Outcome)
	successData, err := os.ReadFile(successPath)
	require.NoError(t, err)
	assert.NotEqual(t, firstData, successData)
	storedData, err = os.ReadFile(filepath.Join(captureDir, sealedFileName))
	require.NoError(t, err)
	assert.Equal(t, successData, storedData)
}

func TestReportRefusesQuiescentSourceWithoutDurableExecutionCompletion(
	t *testing.T,
) {
	root := t.TempDir()
	workDir := t.TempDir()
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	sessionID := "11111111-1111-4111-8111-111111111111"
	started := time.Now().UTC()
	state, err := createState(captureDir, manifest{
		OccurrenceID:      "unfinished-wrapper",
		Provider:          string(ProviderClaude),
		ProviderSessionID: sessionID,
		ProviderRoot:      root,
		ProviderWorkDir:   physicalWorkDir,
		StartedAt:         started,
		Execution:         ExecutionOutcome{StartedAt: started},
		Invocation:        invocationName(ProviderClaude),
		Limits:            testLimits(),
	})
	require.NoError(t, err)
	state.close()
	sourcePath := filepath.Join(
		root, encodeClaudeWorkDir(physicalWorkDir), sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
	initial := []byte(strings.Join(
		claudeHelperLines(sessionID, physicalWorkDir, false), "\n") + "\n")
	require.NoError(t, os.WriteFile(sourcePath, initial, 0o600))
	time.Sleep(testLimits().Quiescence + time.Millisecond)

	reporting, reportErr := Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: resultPath,
		CustomPricing: testPricing(),
	})

	require.Error(t, reportErr)
	assert.Equal(t, ReportingFailed, reporting.Outcome)
	assert.Equal(t, ReasonUnfinishedSession, reporting.Reason)
	manifestData, err := os.ReadFile(filepath.Join(captureDir, manifestFileName))
	require.NoError(t, err)
	var captured manifest
	require.NoError(t, json.Unmarshal(manifestData, &captured))
	assert.Empty(t, captured.SealedDigest)
	assert.False(t, captured.SourcesComplete)
	require.NoError(t, os.WriteFile(
		sourcePath, append(initial, []byte(`{"type":"assistant"}`+"\n")...), 0o600))
	retry, retryErr := Report(context.Background(), ReportOptions{
		CaptureDir:    captureDir,
		ResultPath:    filepath.Join(t.TempDir(), "retry.json"),
		CustomPricing: testPricing(),
	})
	require.Error(t, retryErr)
	assert.Equal(t, ReportingFailed, retry.Outcome)
	assert.Equal(t, ReasonUnfinishedSession, retry.Reason)
	manifestData, err = os.ReadFile(filepath.Join(captureDir, manifestFileName))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(manifestData, &captured))
	assert.Empty(t, captured.SealedDigest)
	assert.False(t, captured.SourcesComplete)
}

func TestRunClaudeQuiescentUnfinishedSessionSealsPartialUsage(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "interrupted-usage",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-unfinished", 7),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, 7, outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
	assert.Empty(t, result.Reporting.Reason)
	assert.Equal(t, AssurancePartial, result.Assurance.State)
	assert.Contains(t, result.Assurance.Reasons, ReasonUnfinishedSession)
	assert.Contains(t, result.Assurance.Reasons, ReasonCostUnavailable)
	assert.Nil(t, result.Cost)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 100)
	assertIntPointer(t, result.Usage.OutputTokens, 50)

	var replay bytes.Buffer
	_, err = Report(context.Background(), ReportOptions{
		CaptureDir: captureDir, ResultPath: "-", Stdout: &replay,
		CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, data, replay.Bytes())
}

func TestRunClaudeMalformedMiddleRecordSealsPartialUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{name: "root", mode: "claude-malformed"},
		{name: "descendant", mode: "claude-subagent-malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			resultPath := filepath.Join(t.TempDir(), "result.json")
			producer := copyCaptureHelper(t, "claude")
			_, err := Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: "malformed-" + tc.name,
				CaptureDir: filepath.Join(t.TempDir(), "capture"),
				ResultPath: resultPath, ProviderRoot: root, WorkDir: t.TempDir(),
				Command:     []string{producer, "-p", "prompt"},
				Environment: helperEnvironment(root, tc.mode, 0),
				Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:      testLimits(), CustomPricing: testPricing(),
			})
			require.NoError(t, err)

			data, err := os.ReadFile(resultPath)
			require.NoError(t, err)
			result, err := DecodeResult(bytes.NewReader(data))
			require.NoError(t, err)
			assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
			assert.Equal(t, AssurancePartial, result.Assurance.State)
			assert.Contains(t, result.Assurance.Reasons, ReasonMalformedTranscript)
			assert.Contains(t, result.Assurance.Reasons, ReasonCostUnavailable)
			assert.Nil(t, result.Cost)
			require.NotNil(t, result.Usage)
			require.NotNil(t, result.Usage.OutputTokens)
			assert.Positive(t, *result.Usage.OutputTokens)
		})
	}
}

func TestRunCodexMalformedRecordSealsPartialUsage(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		outputTokens int
	}{
		{name: "root middle", mode: "codex-malformed", outputTokens: 10},
		{name: "root EOF tail", mode: "codex-malformed-tail", outputTokens: 10},
		{name: "descendant", mode: "codex-subagent-malformed", outputTokens: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			resultPath := filepath.Join(t.TempDir(), "result.json")
			producer := copyCaptureHelper(t, "codex")
			_, err := Run(context.Background(), RunOptions{
				Provider: ProviderCodex, OccurrenceID: "codex-malformed-" + tc.name,
				CaptureDir: filepath.Join(t.TempDir(), "capture"),
				ResultPath: resultPath, ProviderRoot: root, WorkDir: t.TempDir(),
				Command:     []string{producer, "exec", "--json", "prompt"},
				Environment: helperEnvironment(root, tc.mode, 0),
				Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:      testLimits(), CustomPricing: testPricing(),
			})
			require.NoError(t, err)

			data, err := os.ReadFile(resultPath)
			require.NoError(t, err)
			result, err := DecodeResult(bytes.NewReader(data))
			require.NoError(t, err)
			assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
			assert.Equal(t, AssurancePartial, result.Assurance.State)
			assert.Contains(t, result.Assurance.Reasons, ReasonMalformedTranscript)
			assert.Contains(t, result.Assurance.Reasons, ReasonCostUnavailable)
			assert.Nil(t, result.Cost)
			require.NotNil(t, result.Usage)
			assertIntPointer(t, result.Usage.OutputTokens, tc.outputTokens)
		})
	}
}

func TestRunCodexRequiresJSONAndUsesExactThreadMarker(t *testing.T) {
	producer := copyCaptureHelper(t, "codex")
	_, err := prepareInvocation(ProviderCodex, []string{producer, "exec", "prompt"}, "")
	require.ErrorContains(t, err, "--json")

	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	var stdout bytes.Buffer
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-run",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "PROMPT_SENTINEL"},
		Environment: helperEnvironment(root, "codex-final", 0),
		Streams:     Streams{Stdout: &stdout, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Zero(t, outcome.ExitCode)
	assert.Contains(t, stdout.String(), `"type":"thread.started"`)
	assert.Contains(t, stdout.String(), `"type":"turn.completed"`)

	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, ProviderCodex, Provider(result.Provider.Name))
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", result.Provider.SessionID)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 40)
	assertIntPointer(t, result.Usage.OutputTokens, 10)
	assertIntPointer(t, result.Usage.CacheReadInputTokens, 60)
	assert.Nil(t, result.Usage.CacheCreationInputTokens)
	assert.Contains(t, result.Assurance.Reasons, ReasonCodexCacheWriteAbsent)
	assert.Contains(t, result.Assurance.Reasons, ReasonReasoningAbsent)
}

func TestRunCodexQuiescentUnfinishedSessionSealsPartialUsage(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "codex")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-interrupted",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "prompt"},
		Environment: helperEnvironment(root, "codex-unfinished", 9),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	assert.Equal(t, 9, outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
	assert.Equal(t, AssurancePartial, result.Assurance.State)
	assert.Contains(t, result.Assurance.Reasons, ReasonUnfinishedSession)
	assert.Contains(t, result.Assurance.Reasons, ReasonCostUnavailable)
	assert.Nil(t, result.Cost)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.OutputTokens, 10)
}

func TestRunCodexPersistsDiscoveredSubagentSources(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "codex")
	limits := testLimits()
	limits.FinalizationWait = 5 * time.Second
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-delegated",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "prompt"},
		Environment: helperEnvironment(root, "codex-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}, result.Provider.IncludedSessionIDs)
	require.Len(t, result.Sources, 2)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 45)
	assertIntPointer(t, result.Usage.OutputTokens, 12)

	bundleFile, err := os.Open(filepath.Join(captureDir, sourcesDirName, bundleFileName))
	require.NoError(t, err)
	bundle, err := DecodeTranscriptBundle(bundleFile)
	require.NoError(t, bundleFile.Close())
	require.NoError(t, err)
	require.Len(t, bundle.Sources, 2)
	for _, source := range bundle.Sources {
		assert.Contains(t, source.RawSource.Path, "codex/sessions/")
	}
	assertBundleImportsUsage(
		t,
		filepath.Join(captureDir, "sources", "codex", "sessions"),
		parser.AgentCodex,
		"codex:"+result.Provider.SessionID,
		result,
	)
}

func TestRunCodexFindsChildInSpawnDayShard(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "codex")
	limits := testLimits()
	limits.FinalizationWait = 5 * time.Second
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-late-child",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "prompt"},
		Environment: helperEnvironment(root, "codex-late-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}, result.Provider.IncludedSessionIDs)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 45)
	assertIntPointer(t, result.Usage.OutputTokens, 12)
}

func TestRunCodexRetriesWhenChildChangesDuringFinalization(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "codex")
	limits := testLimits()
	limits.FinalizationWait = 10 * time.Second
	type runResponse struct {
		outcome RunOutcome
		err     error
	}
	done := make(chan runResponse, 1)
	go func() {
		outcome, runErr := Run(context.Background(), RunOptions{
			Provider: ProviderCodex, OccurrenceID: "codex-changing-child",
			CaptureDir: captureDir, ResultPath: resultPath,
			ProviderRoot: root, WorkDir: t.TempDir(),
			Command:     []string{producer, "exec", "--json", "prompt"},
			Environment: helperEnvironment(root, "codex-changing-subagent", 0),
			Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:      limits, CustomPricing: testPricing(),
		})
		done <- runResponse{outcome: outcome, err: runErr}
	}()

	childID := "22222222-2222-4222-8222-222222222222"
	rootID := "11111111-1111-4111-8111-111111111111"
	day := filepath.FromSlash(time.Now().UTC().Format("2006/01/02"))
	childPath := filepath.Join(
		root, day,
		"rollout-child-"+childID+".jsonl",
	)
	rootCopyPath := filepath.Join(
		captureDir, sourcesDirName, filepath.FromSlash(bundleSourcePrefix(ProviderCodex)),
		day, "rollout-test-"+rootID+".jsonl",
	)
	require.Eventually(t, func() bool {
		info, err := os.Lstat(rootCopyPath)
		return err == nil && info.Mode().IsRegular()
	}, 20*time.Second, 10*time.Millisecond)
	changed := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(childPath, changed, changed))

	response := <-done
	require.NoError(t, response.err)
	assert.Zero(t, response.outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, ReportingComplete, result.Reporting.Outcome)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 45)
	assertIntPointer(t, result.Usage.OutputTokens, 12)
}

func TestRunCodexConflictingMarkersReportCorrelationConflict(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	producer := copyCaptureHelper(t, "codex")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-marker-conflict",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "prompt"},
		Environment: helperEnvironment(root, "codex-conflicting-markers", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(),
	})
	require.Error(t, err)
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonCorrelationConflict, result.Reporting.Reason)

	firstID := "11111111-1111-4111-8111-111111111111"
	dayDir := filepath.Join(root, filepath.FromSlash(time.Now().UTC().Format("2006/01/02")))
	require.NoError(t, os.MkdirAll(dayDir, 0o700))
	lines := []string{
		testjsonl.CodexSessionMetaJSON(
			firstID, "/workspace", "codex_exec", "2026-08-16T10:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON(
			"gpt-test", "root-turn", "2026-08-16T10:00:01Z"),
		testjsonl.CodexTokenCountJSON("2026-08-16T10:00:02Z", 100, 10, 0),
		`{"type":"event_msg","timestamp":"2026-08-16T10:00:03Z","payload":{"type":"task_complete"}}`,
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(dayDir, "rollout-test-"+firstID+".jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600,
	))
	recovered, reportErr := Report(context.Background(), ReportOptions{
		CaptureDir:    captureDir,
		ResultPath:    filepath.Join(t.TempDir(), "recovered.json"),
		CustomPricing: testPricing(),
	})
	require.Error(t, reportErr)
	assert.Equal(t, ReportingFailed, recovered.Outcome)
	assert.Equal(t, ReasonCorrelationConflict, recovered.Reason)
}

func TestCodexCorrelationFailureIsDurableBeforeChildWaitReturns(t *testing.T) {
	firstID := "11111111-1111-4111-8111-111111111111"
	secondID := "22222222-2222-4222-8222-222222222222"
	firstMarker := []byte(
		`{"type":"thread.started","thread_id":"` + firstID + `"}` + "\n")
	for _, test := range []struct {
		name   string
		writes [][]byte
		reason ReasonCode
	}{
		{
			name: "conflict",
			writes: [][]byte{firstMarker, []byte(
				`{"type":"thread.started","thread_id":"` + secondID + `"}` + "\n")},
			reason: ReasonCorrelationConflict,
		},
		{
			name: "overflow", writes: [][]byte{
				firstMarker, bytes.Repeat([]byte("x"), 129),
			},
			reason: ReasonCorrelationUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			captureDir := filepath.Join(t.TempDir(), "capture")
			started := time.Now()
			limits := testLimits()
			limits.MaxLineBytes = 128
			state, err := createState(captureDir, manifest{
				OccurrenceID: "durable-correlation-" + test.name,
				Provider:     string(ProviderCodex), ProviderRoot: root,
				ProviderWorkDir: t.TempDir(), StartedAt: started,
				Execution:  ExecutionOutcome{StartedAt: started},
				Invocation: invocationName(ProviderCodex), Limits: limits,
			})
			require.NoError(t, err)
			defer state.close()
			marker := newCodexThreadMarker(state, limits.MaxLineBytes)
			for _, write := range test.writes {
				_, err = marker.Write(write)
				require.NoError(t, err)
			}
			require.NoError(t, marker.persistenceError())
			state.close()

			dayDir := filepath.Join(
				root, filepath.FromSlash(started.UTC().Format("2006/01/02")))
			require.NoError(t, os.MkdirAll(dayDir, 0o700))
			lines := []string{
				testjsonl.CodexSessionMetaJSON(
					firstID, "/workspace", "codex_exec", "2026-08-16T10:00:00Z"),
				testjsonl.CodexTurnContextWithIDJSON(
					"gpt-test", "root-turn", "2026-08-16T10:00:01Z"),
				testjsonl.CodexTokenCountJSON("2026-08-16T10:00:02Z", 100, 10, 0),
				`{"type":"event_msg","timestamp":"2026-08-16T10:00:03Z","payload":{"type":"task_complete"}}`,
			}
			require.NoError(t, os.WriteFile(
				filepath.Join(dayDir, "rollout-test-"+firstID+".jsonl"),
				[]byte(strings.Join(lines, "\n")+"\n"), 0o600,
			))

			reporting, reportErr := Report(context.Background(), ReportOptions{
				CaptureDir: captureDir, ResultPath: filepath.Join(t.TempDir(), "result.json"),
				CustomPricing: testPricing(),
			})
			require.Error(t, reportErr)
			assert.Equal(t, ReportingFailed, reporting.Outcome)
			assert.Equal(t, test.reason, reporting.Reason)
		})
	}
}

func TestRunCodexRejectsSeveralExactCandidates(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "codex")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderCodex, OccurrenceID: "codex-conflict",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "exec", "--json", "prompt"},
		Environment: helperEnvironment(root, "codex-multiple", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.Error(t, err)
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonMultipleSessions, result.Reporting.Reason)
}

func TestCodexExactLookupCoversLocalAndUTCDateShards(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	zone := time.FixedZone("capture-test", -5*60*60)
	started := time.Date(2026, 8, 16, 23, 30, 0, 0, zone)
	for _, day := range []string{
		started.Format("2006/01/02"), started.UTC().Format("2006/01/02"),
	} {
		t.Run(day, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, filepath.FromSlash(day))
			require.NoError(t, os.MkdirAll(dir, 0o700))
			path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaJSON(
				id, "/workspace", "codex_exec", "2026-08-17T04:30:00Z")
			require.NoError(t, os.WriteFile(path, []byte(meta+"\n"), 0o600))

			matches, err := locateCodexRoot(
				t.Context(), root, id, started, testLimits())
			require.NoError(t, err)
			assert.Equal(t, []string{path}, matches)
		})
	}
}

func TestCodexExactLookupStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	matches, err := locateCodexRoot(
		ctx,
		t.TempDir(),
		"11111111-1111-4111-8111-111111111111",
		time.Now(),
		testLimits(),
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, matches)
}

func TestConcurrentClaudeCapturesCannotSelectEachOthersSessions(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	type response struct {
		result Result
		err    error
	}
	responses := make(chan response, 2)
	for i := range 2 {
		go func() {
			resultPath := filepath.Join(t.TempDir(), fmt.Sprintf("result-%d.json", i))
			_, runErr := Run(context.Background(), RunOptions{
				Provider: ProviderClaude, OccurrenceID: fmt.Sprintf("parallel-%d", i),
				CaptureDir: filepath.Join(t.TempDir(), fmt.Sprintf("capture-%d", i)),
				ResultPath: resultPath, ProviderRoot: root, WorkDir: workDir,
				Command:     []string{producer, "-p", "prompt"},
				Environment: helperEnvironment(root, "claude-final", 0),
				Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
				Limits:      testLimits(), CustomPricing: testPricing(),
			})
			if runErr != nil {
				responses <- response{err: runErr}
				return
			}
			data, readErr := os.ReadFile(resultPath)
			if readErr != nil {
				responses <- response{err: readErr}
				return
			}
			result, decodeErr := DecodeResult(bytes.NewReader(data))
			responses <- response{result: result, err: decodeErr}
		}()
	}
	first, second := <-responses, <-responses
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.NotEqual(t, first.result.Provider.SessionID, second.result.Provider.SessionID)
	assert.ElementsMatch(t, []string{"parallel-0", "parallel-1"},
		[]string{first.result.OccurrenceID, second.result.OccurrenceID})
}

func TestConcurrentClaudeCapturesCannotReserveSameSession(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	sessionID := "44444444-4444-4444-8444-444444444444"
	marker := filepath.Join(t.TempDir(), "producer-started")
	release := filepath.Join(t.TempDir(), "release-producer")
	firstCaptureDir := filepath.Join(t.TempDir(), "capture-first")
	firstResultPath := filepath.Join(t.TempDir(), "result-first.json")

	type response struct {
		outcome RunOutcome
		err     error
	}
	firstDone := make(chan response, 1)
	firstStopped := make(chan struct{})
	go func() {
		defer close(firstStopped)
		outcome, err := Run(t.Context(), RunOptions{
			Provider: ProviderClaude, OccurrenceID: "reserved-first",
			ProviderSessionID: sessionID,
			CaptureDir:        firstCaptureDir,
			ResultPath:        firstResultPath,
			ProviderRoot:      root, WorkDir: workDir,
			Command: []string{
				producer, "--session-id", sessionID, "-p", "prompt",
			},
			Environment: append(
				helperEnvironment(root, "claude-block-before-source", 0),
				"AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER="+marker,
				"AGENTSVIEW_CAPTURE_TEST_RELEASE_MARKER="+release,
			),
			Streams: Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:  testLimits(), CustomPricing: testPricing(),
		})
		firstDone <- response{outcome: outcome, err: err}
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release"), 0o600)
		select {
		case <-firstStopped:
		case <-time.After(20 * time.Second):
			t.Error("reserved capture did not stop during cleanup")
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 20*time.Second, 10*time.Millisecond)
	secondCaptureDir := filepath.Join(t.TempDir(), "capture-second")
	secondWorkDir := t.TempDir()
	var secondOutput bytes.Buffer
	secondLimits := testLimits()
	secondLimits.FinalizationWait = 200 * time.Millisecond
	_, err := Run(t.Context(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "reserved-second",
		ProviderSessionID: sessionID,
		CaptureDir:        secondCaptureDir,
		ResultPath:        filepath.Join(t.TempDir(), "result-second.json"),
		ProviderRoot:      root, WorkDir: secondWorkDir,
		Command: []string{
			producer, "--session-id", sessionID, "-p", "prompt",
		},
		Environment: helperEnvironment(root, "none", 0),
		Streams:     Streams{Stdout: &secondOutput, Stderr: io.Discard},
		Limits:      secondLimits,
	})
	require.ErrorContains(t, err, "already reserved")
	assert.Empty(t, secondOutput.String())
	assert.NoDirExists(t, secondCaptureDir)

	require.NoError(t, os.WriteFile(release, []byte("release"), 0o600))
	resolvedWorkDir, err := resolveWorkDir(workDir)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(
			root, encodeClaudeWorkDir(resolvedWorkDir), sessionID+".jsonl",
		))
		return err == nil
	}, 20*time.Second, 10*time.Millisecond)
	select {
	case first := <-firstDone:
		require.NoError(t, first.err)
		assert.Equal(t, 0, first.outcome.ExitCode)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "reserved capture did not finish after release")
	}
}

func TestClaudeCaptureUsesCanonicalDelegatedUsageWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "delegated",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, result.Provider.IncludedSessionIDs, 2)
	assert.Contains(t, result.Provider.IncludedSessionIDs, "agent-abc123")
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 30)
	assertIntPointer(t, result.Usage.OutputTokens, 15)
}

func TestClaudeCaptureIncludesChildWithoutFlushedLinkMetadata(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "unlinked-child",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-unlinked-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Contains(t, result.Provider.IncludedSessionIDs, "agent-abc123")
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 30)
	assertIntPointer(t, result.Usage.OutputTokens, 15)
}

func TestClaudeCaptureRejectsMissingReferencedChild(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "missing-child",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-missing-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.Error(t, err)
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonSourceUnavailable, result.Reporting.Reason)
	assert.Nil(t, result.Usage)
}

func TestClaudeCaptureRetryDropsRemovedSubagentUsage(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	type runResponse struct {
		outcome RunOutcome
		err     error
	}
	done := make(chan runResponse, 1)
	go func() {
		outcome, runErr := Run(context.Background(), RunOptions{
			Provider: ProviderClaude, OccurrenceID: "subagent-removed",
			CaptureDir: captureDir, ResultPath: resultPath,
			ProviderRoot: root, WorkDir: workDir,
			Command:     []string{producer, "-p", "prompt"},
			Environment: helperEnvironment(root, "claude-subagent", 0),
			Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:      testLimits(), CustomPricing: testPricing(),
		})
		done <- runResponse{outcome: outcome, err: runErr}
	}()

	var sessionID string
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(filepath.Join(captureDir, manifestFileName))
		if readErr != nil {
			return false
		}
		var captured manifest
		if json.Unmarshal(data, &captured) != nil || captured.SourcesComplete ||
			len(captured.Sources) == 0 {
			return false
		}
		sessionID = captured.ProviderSessionID
		return sessionID != ""
	}, 20*time.Second, time.Millisecond)

	rootPath := filepath.Join(root, encodeClaudeWorkDir(physicalWorkDir), sessionID+".jsonl")
	rootData := []byte(strings.Join(
		claudeHelperLines(sessionID, physicalWorkDir, false), "\n") + "\n")
	require.NoError(t, os.WriteFile(rootPath, rootData, 0o600))
	childPath := filepath.Join(
		strings.TrimSuffix(rootPath, ".jsonl"), "subagents", "agent-abc123.jsonl")
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(filepath.Join(captureDir, manifestFileName))
		if readErr != nil {
			return false
		}
		var captured manifest
		return json.Unmarshal(data, &captured) == nil &&
			!captured.SourcesComplete && len(captured.Sources) == 2
	}, 20*time.Second, time.Millisecond)
	require.NoError(t, os.Rename(childPath, filepath.Join(t.TempDir(), "removed.jsonl")))

	response := <-done
	require.NoError(t, response.err)
	assert.Zero(t, response.outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, []string{sessionID}, result.Provider.IncludedSessionIDs)
	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.InputTokens, 100)
	assertIntPointer(t, result.Usage.OutputTokens, 50)
	assert.Len(t, result.Sources, 1)
}

func TestCaptureSourceByteLimitIsActionable(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	limits := testLimits()
	limits.MaxSourceBytes = 32
	outcome, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "bounded",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.Error(t, err)
	assert.Equal(t, ReportFailureExitCode, outcome.ExitCode)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonSourceBytesLimit, result.Reporting.Reason)
}

func TestCaptureAggregateSourceByteLimitIsActionable(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	physicalWorkDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	rootLines, childLines := claudeSubagentHelperLines(
		"11111111-1111-4111-8111-111111111111", physicalWorkDir)
	rootBytes := int64(len(strings.Join(rootLines, "\n") + "\n"))
	childBytes := int64(len(strings.Join(childLines, "\n") + "\n"))
	limits := testLimits()
	limits.MaxSourceBytes = max(rootBytes, childBytes) + 1
	limits.MaxTotalBytes = rootBytes + childBytes - 1
	captureDir := filepath.Join(t.TempDir(), "capture")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")

	_, err = Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "bounded-total",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: workDir,
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-subagent", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.Error(t, err)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonSourceBytesLimit, result.Reporting.Reason)
	jsonlCopies := 0
	walkErr := filepath.WalkDir(
		filepath.Join(captureDir, sourcesDirName),
		func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
				jsonlCopies++
			}
			return err
		},
	)
	if !errors.Is(walkErr, os.ErrNotExist) {
		require.NoError(t, walkErr)
	}
	assert.Zero(t, jsonlCopies, "aggregate limit must fail before copying")
}

func TestCaptureSourceCountLimitIsActionable(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	limits := testLimits()
	limits.MaxSources = 2
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "bounded-count",
		CaptureDir: filepath.Join(t.TempDir(), "capture"), ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-many-sources", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      limits, CustomPricing: testPricing(),
	})
	require.Error(t, err)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	result, decodeErr := DecodeResult(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, ReasonSourceLimit, result.Reporting.Reason)
}

func TestCaptureDistinguishesAnObservedSourceThatDisappears(t *testing.T) {
	root := t.TempDir()
	captureDir := filepath.Join(t.TempDir(), "capture")
	workDir := t.TempDir()
	resultPath := filepath.Join(t.TempDir(), "result.json")
	producer := copyCaptureHelper(t, "claude")
	limits := testLimits()
	limits.FinalizationWait = 5 * time.Second
	limits.Quiescence = 2 * time.Second
	type runResponse struct {
		outcome RunOutcome
		err     error
	}
	done := make(chan runResponse, 1)
	go func() {
		outcome, runErr := Run(context.Background(), RunOptions{
			Provider: ProviderClaude, OccurrenceID: "source-disappeared",
			CaptureDir: captureDir, ResultPath: resultPath,
			ProviderRoot: root, WorkDir: workDir,
			Command:     []string{producer, "-p", "prompt"},
			Environment: helperEnvironment(root, "claude-final", 0),
			Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
			Limits:      limits, CustomPricing: testPricing(),
		})
		done <- runResponse{outcome: outcome, err: runErr}
	}()

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(captureDir, manifestFileName))
		if err != nil {
			return false
		}
		var captured manifest
		return json.Unmarshal(data, &captured) == nil && captured.SourceObserved
	}, 20*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Rename(root, root+"-gone"))
	response := <-done
	require.Error(t, response.err)
	assert.Equal(t, ReportFailureExitCode, response.outcome.ExitCode)
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	result, err := DecodeResult(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, ReasonSourceUnavailable, result.Reporting.Reason)
}

func TestCaptureLeavesNormalRuntimeStateUntouched(t *testing.T) {
	normalDataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", normalDataDir)
	root := t.TempDir()
	producer := copyCaptureHelper(t, "claude")
	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "isolated",
		CaptureDir:   filepath.Join(t.TempDir(), "capture"),
		ResultPath:   filepath.Join(t.TempDir(), "result.json"),
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(), CustomPricing: testPricing(),
	})
	require.NoError(t, err)
	entries, err := os.ReadDir(normalDataDir)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"one-shot capture must not create an archive, runtime record, log, or service state")
}

func copyCaptureHelper(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	source, err := os.Executable()
	require.NoError(t, err)
	in, err := os.Open(source)
	require.NoError(t, err)
	defer in.Close()
	path := filepath.Join(t.TempDir(), name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	return path
}

func helperEnvironment(root, mode string, exitCode int) []string {
	secureTestProviderRoot(root)
	return append(os.Environ(),
		"AGENTSVIEW_CAPTURE_TEST_HELPER=1",
		"AGENTSVIEW_CAPTURE_TEST_ROOT="+root,
		"AGENTSVIEW_CAPTURE_TEST_MODE="+mode,
		fmt.Sprintf("AGENTSVIEW_CAPTURE_TEST_EXIT=%d", exitCode),
		"PRIVATE_TEST_VALUE=ENV_SECRET_SENTINEL",
		"AUTHORIZATION=AUTHORIZATION_SENTINEL",
	)
}

func captureTestHelper() {
	root := os.Getenv("AGENTSVIEW_CAPTURE_TEST_ROOT")
	if len(os.Args) == 2 && os.Args[1] == "write-delayed-codex-child" {
		time.Sleep(200 * time.Millisecond)
		writeCodexChildHelper(root)
		os.Exit(0)
	}
	mode := os.Getenv("AGENTSVIEW_CAPTURE_TEST_MODE")
	exitCode := 0
	_, _ = fmt.Sscanf(os.Getenv("AGENTSVIEW_CAPTURE_TEST_EXIT"), "%d", &exitCode)
	if path := os.Getenv("AGENTSVIEW_CAPTURE_TEST_RESULT_MUST_BE_ABSENT"); path != "" {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "result existed when producer started")
			os.Exit(97)
		}
	}
	if mode == "none" {
		fmt.Fprintln(os.Stdout, "child stdout")
		fmt.Fprintln(os.Stderr, "child stderr")
		os.Exit(exitCode)
	}
	if dir := os.Getenv("AGENTSVIEW_CAPTURE_TEST_CHDIR"); dir != "" {
		if err := os.Chdir(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
	}
	if strings.HasPrefix(mode, "claude-") {
		sessionID, _ := optionValue(os.Args[1:], "--session-id")
		if sessionID == "" {
			sessionID = os.Getenv("AGENTSVIEW_CAPTURE_TEST_SESSION_ID")
		}
		if mode == "claude-block-before-source" {
			marker := os.Getenv("AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER")
			release := os.Getenv("AGENTSVIEW_CAPTURE_TEST_RELEASE_MARKER")
			_ = os.WriteFile(marker, []byte("started"), 0o600)
			for {
				if _, err := os.Stat(release); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		cwd, _ := os.Getwd()
		cwd, _ = resolveWorkDir(cwd)
		path := filepath.Join(root, encodeClaudeWorkDir(cwd), sessionID+".jsonl")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		lines := claudeHelperLines(sessionID, cwd, mode == "claude-unfinished")
		if mode == "claude-malformed" {
			lines = []string{lines[0], `{"type":"assistant"`, lines[1]}
		}
		if mode == "claude-subagent" || mode == "claude-subagent-malformed" ||
			mode == "claude-missing-subagent" ||
			mode == "claude-unlinked-subagent" {
			var childLines []string
			lines, childLines = claudeSubagentHelperLines(sessionID, cwd)
			if mode == "claude-subagent-malformed" {
				childLines = []string{
					childLines[0], `{"type":"assistant"`, childLines[1],
				}
			}
			if mode == "claude-unlinked-subagent" {
				lines = append(lines[:2], lines[3:]...)
			}
			if mode != "claude-missing-subagent" {
				childPath := filepath.Join(
					strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-abc123.jsonl",
				)
				_ = os.MkdirAll(filepath.Dir(childPath), 0o700)
				_ = os.WriteFile(
					childPath, []byte(strings.Join(childLines, "\n")+"\n"), 0o600)
			}
		}
		_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		if mode == "claude-many-sources" {
			children := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
			_ = os.MkdirAll(children, 0o700)
			for i := range 3 {
				_ = os.WriteFile(
					filepath.Join(children, fmt.Sprintf("agent-%d.jsonl", i)),
					[]byte(strings.Join(lines, "\n")+"\n"), 0o600,
				)
			}
		}
		if mode == "claude-stdin" {
			input, _ := io.ReadAll(os.Stdin)
			fmt.Fprintf(os.Stdout, "stdin:%s\n", input)
		}
		fmt.Fprintln(os.Stdout, "child stdout")
		fmt.Fprintln(os.Stderr, "child stderr")
		if mode == "claude-wait-signal" || mode == "claude-trap-signal" ||
			mode == "claude-ignore-signal" {
			captureHelperWaitForSignal(
				mode, os.Getenv("AGENTSVIEW_CAPTURE_TEST_SIGNAL_MARKER"),
			)
		}
		os.Exit(exitCode)
	}
	if mode == "codex-conflicting-markers" {
		first, _ := json.Marshal(map[string]string{
			"type": "thread.started", "thread_id": "11111111-1111-4111-8111-111111111111",
		})
		second, _ := json.Marshal(map[string]string{
			"type": "thread.started", "thread_id": "22222222-2222-4222-8222-222222222222",
		})
		fmt.Fprintln(os.Stdout, string(first))
		fmt.Fprintln(os.Stdout, string(second))
		os.Exit(exitCode)
	}
	if mode == "codex-final" || mode == "codex-unfinished" ||
		mode == "codex-multiple" || mode == "codex-subagent" ||
		mode == "codex-late-subagent" || mode == "codex-changing-subagent" ||
		mode == "codex-malformed" || mode == "codex-malformed-tail" ||
		mode == "codex-subagent-malformed" {
		id := "11111111-1111-4111-8111-111111111111"
		childID := "22222222-2222-4222-8222-222222222222"
		marker, _ := json.Marshal(map[string]string{"type": "thread.started", "thread_id": id})
		completed, _ := json.Marshal(map[string]any{
			"type": "turn.completed", "usage": map[string]int{"input_tokens": 100, "output_tokens": 10},
		})
		fmt.Fprintln(os.Stdout, string(marker))
		day := time.Now().UTC().Format("2006/01/02")
		dir := filepath.Join(root, filepath.FromSlash(day))
		path := filepath.Join(dir, "rollout-test-"+id+".jsonl")
		_ = os.MkdirAll(dir, 0o700)
		lines := []string{
			testjsonl.CodexSessionMetaJSON(id, "/workspace", "codex_exec", "2026-08-16T10:00:00Z"),
			testjsonl.CodexTurnContextWithIDJSON("gpt-test", "root-turn", "2026-08-16T10:00:01Z"),
			testjsonl.CodexMsgJSON("user", "PROMPT_SENTINEL", "2026-08-16T10:00:02Z"),
			`{"type":"event_msg","timestamp":"2026-08-16T10:00:03Z","payload":{"type":"task_started"}}`,
			testjsonl.CodexMsgJSON("assistant", "RESPONSE_SENTINEL", "2026-08-16T10:00:04Z"),
			testjsonl.CodexTokenCountJSON("2026-08-16T10:00:05Z", 100, 10, 60),
		}
		if mode == "codex-subagent" || mode == "codex-late-subagent" ||
			mode == "codex-changing-subagent" || mode == "codex-subagent-malformed" {
			spawnedAt := time.Now().UTC()
			if mode == "codex-late-subagent" {
				spawnedAt = spawnedAt.AddDate(0, 0, 3)
			}
			lines = append(lines,
				testjsonl.CodexFunctionCallWithCallIDJSON(
					"spawn_agent", "spawn-child",
					map[string]any{"task_name": "worker"}, spawnedAt.Format(time.RFC3339Nano),
				),
				testjsonl.CodexSubagentActivityJSON(
					"started", "spawn-child", childID, "/workspace/subtask",
					spawnedAt.Add(time.Millisecond).Format(time.RFC3339Nano),
				),
			)
			if mode == "codex-subagent" {
				writer := exec.Command(os.Args[0], "write-delayed-codex-child")
				writer.Env = os.Environ()
				if writer.Start() != nil {
					os.Exit(3)
				}
				_ = writer.Process.Release()
			}
		}
		if mode != "codex-unfinished" {
			lines = append(lines,
				`{"type":"event_msg","timestamp":"2026-08-16T10:00:06Z","payload":{"type":"task_complete"}}`)
		}
		if mode == "codex-malformed" {
			lines = append(lines[:1], append([]string{`{"type":"event_msg"`}, lines[1:]...)...)
		}
		data := []byte(strings.Join(lines, "\n") + "\n")
		if mode == "codex-malformed-tail" {
			data = append(data, `{"type":"event_msg"`...)
		}
		_ = os.WriteFile(path, data, 0o600)
		if mode == "codex-subagent-malformed" {
			writeCodexChildHelperOnDay(root, time.Now().UTC(), false, true)
		}
		if mode == "codex-late-subagent" {
			writeCodexChildHelperOnDay(
				root, time.Now().UTC().AddDate(0, 0, 3), false, false)
		}
		if mode == "codex-changing-subagent" {
			writeCodexChildHelperOnDay(root, time.Now().UTC(), true, false)
		}
		if mode == "codex-multiple" {
			_ = os.WriteFile(
				filepath.Join(dir, "rollout-conflict-"+id+".jsonl"),
				[]byte(strings.Join(lines, "\n")+"\n"), 0o600,
			)
		}
		fmt.Fprintln(os.Stdout, string(completed))
		os.Exit(exitCode)
	}
	os.Exit(2)
}

func writeCodexChildHelper(root string) {
	writeCodexChildHelperOnDay(root, time.Now().UTC(), false, false)
}

func writeCodexChildHelperOnDay(
	root string, day time.Time, large, malformed bool,
) {
	parentID := "11111111-1111-4111-8111-111111111111"
	childID := "22222222-2222-4222-8222-222222222222"
	dir := filepath.Join(root, filepath.FromSlash(day.UTC().Format("2006/01/02")))
	_ = os.MkdirAll(dir, 0o700)
	lines := []string{
		testjsonl.CodexSubagentSessionMetaJSON(
			childID, parentID, "/workspace", "codex_exec", "2026-08-16T10:00:05.3Z"),
		testjsonl.CodexTurnContextWithIDJSON(
			"gpt-test", "child-turn", "2026-08-16T10:00:05.4Z"),
		testjsonl.CodexMsgJSON("user", "child task", "2026-08-16T10:00:05.5Z"),
		testjsonl.CodexMsgJSON("assistant", "child answer", "2026-08-16T10:00:05.6Z"),
		testjsonl.CodexTokenCountJSON("2026-08-16T10:00:05.7Z", 5, 2, 0),
		`{"type":"event_msg","timestamp":"2026-08-16T10:00:05.8Z","payload":{"type":"task_complete"}}`,
	}
	if malformed {
		lines = append(lines[:1], append([]string{`{"type":"event_msg"`}, lines[1:]...)...)
	}
	if large {
		padding := strings.Repeat("x", 8<<10)
		for range 4096 {
			lines = append(lines, fmt.Sprintf(
				`{"type":"capture_test_padding","padding":%q}`, padding))
		}
	}
	_ = os.WriteFile(
		filepath.Join(dir, "rollout-child-"+childID+".jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func claudeHelperLines(sessionID, cwd string, unfinished bool) []string {
	stopReason := "end_turn"
	content := []map[string]any{{"type": "text", "text": "RESPONSE_SENTINEL"}}
	if unfinished {
		stopReason = ""
		content = []map[string]any{{
			"type": "tool_use", "id": "tool-pending", "name": "Read",
			"input": map[string]string{"file_path": "private.txt"},
		}}
	}
	user, _ := json.Marshal(map[string]any{
		"type": "user", "uuid": "user-1", "sessionId": sessionID,
		"timestamp": "2026-08-16T10:00:00Z", "cwd": cwd,
		"message": map[string]any{"role": "user", "content": "PROMPT_SENTINEL"},
	})
	assistant, _ := json.Marshal(map[string]any{
		"type": "assistant", "uuid": "assistant-1", "parentUuid": "user-1",
		"sessionId": sessionID, "timestamp": "2026-08-16T10:00:01Z", "cwd": cwd,
		"message": map[string]any{
			"role": "assistant", "model": "claude-test", "stop_reason": stopReason,
			"content": content,
			"usage": map[string]int{
				"input_tokens": 100, "output_tokens": 50,
				"cache_creation_input_tokens": 200, "cache_read_input_tokens": 300,
			},
		},
	})
	return []string{string(user), string(assistant)}
}

func claudeSubagentHelperLines(sessionID, cwd string) ([]string, []string) {
	marshal := func(value any) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	rootUser := map[string]any{
		"type": "user", "uuid": "u1", "sessionId": sessionID,
		"timestamp": "2026-08-16T10:00:00Z", "cwd": cwd,
		"message": map[string]any{"role": "user", "content": "delegate"},
	}
	rootTool := map[string]any{
		"type": "assistant", "uuid": "a1", "parentUuid": "u1", "sessionId": sessionID,
		"timestamp": "2026-08-16T10:00:01Z", "cwd": cwd,
		"message": map[string]any{
			"id": "root-message", "role": "assistant", "model": "claude-test",
			"content": []map[string]any{{
				"type": "tool_use", "id": "toolu-agent", "name": "Agent",
				"input": map[string]string{"prompt": "inspect", "description": "inspect"},
			}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		},
	}
	rootResult := map[string]any{
		"type": "user", "uuid": "u2", "parentUuid": "a1", "sessionId": sessionID,
		"timestamp": "2026-08-16T10:00:02Z", "cwd": cwd,
		"message": map[string]any{
			"role": "user", "content": []map[string]any{{
				"type": "tool_result", "tool_use_id": "toolu-agent", "content": "done",
			}},
		},
		"toolUseResult": map[string]string{"status": "completed", "agentId": "abc123"},
	}
	shared := func(session, uuid, parent string) map[string]any {
		return map[string]any{
			"type": "assistant", "uuid": uuid, "parentUuid": parent, "sessionId": session,
			"timestamp": "2026-08-16T10:00:03Z", "cwd": cwd, "requestId": "shared-request",
			"message": map[string]any{
				"id": "shared-message", "role": "assistant", "model": "claude-test",
				"stop_reason": "end_turn",
				"content":     []map[string]any{{"type": "text", "text": "done"}},
				"usage":       map[string]int{"input_tokens": 20, "output_tokens": 10},
			},
		}
	}
	childUser := map[string]any{
		"type": "user", "uuid": "cu1", "sessionId": sessionID,
		"timestamp": "2026-08-16T10:00:02Z", "cwd": cwd,
		"message": map[string]any{"role": "user", "content": "inspect"},
	}
	return []string{
		marshal(rootUser), marshal(rootTool), marshal(rootResult),
		marshal(shared(sessionID, "a2", "u2")),
	}, []string{
		marshal(childUser), marshal(shared(sessionID, "ca1", "cu1")),
	}
}

func testLimits() Limits {
	limits := DefaultLimits()
	limits.FinalizationWait = 15 * time.Second
	limits.Quiescence = 10 * time.Millisecond
	return limits
}

func testPricing() map[string]config.CustomModelRate {
	return map[string]config.CustomModelRate{
		"claude-test": {
			InputMicrodollarsPerMTok: 1_000_000, OutputMicrodollarsPerMTok: 1_000_000,
			CacheCreationMicrodollarsPerMTok: 1_000_000, CacheReadMicrodollarsPerMTok: 1_000_000,
		},
		"gpt-test": {
			InputMicrodollarsPerMTok: 1_000_000, OutputMicrodollarsPerMTok: 1_000_000,
			CacheReadMicrodollarsPerMTok: 1_000_000,
		},
	}
}

func assertBundleImportsUsage(
	t *testing.T,
	providerRoot string,
	agent parser.AgentType,
	rootID string,
	sealed Result,
) {
	t.Helper()
	database, err := db.OpenIsolated(filepath.Join(t.TempDir(), "import.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	database.SetCustomPricing(testPricing())
	disabled := make([]parser.AgentType, 0, len(parser.Registry)-1)
	for _, definition := range parser.Registry {
		if definition.Type != agent {
			disabled = append(disabled, definition.Type)
		}
	}
	engine := syncer.NewEngine(database, syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			agent: {providerRoot},
		},
		DisabledAgents: disabled,
		Machine:        "bundle-import",
		Ephemeral:      true,
	})
	stats := engine.SyncAll(t.Context(), nil)
	engine.Close()
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)

	imported, err := service.SessionUsageWithSubagents(
		t.Context(), database, rootID, true)
	require.NoError(t, err)
	require.NotNil(t, imported)
	importedTotals, complete, err := service.SessionUsageTokenTotals(
		t.Context(), imported,
	)
	require.NoError(t, err)
	require.True(t, complete)
	require.NotNil(t, sealed.Usage)
	assertIntPointer(t, sealed.Usage.InputTokens, importedTotals.InputTokens)
	assertIntPointer(t, sealed.Usage.OutputTokens, importedTotals.OutputTokens)
	assertIntPointer(t, sealed.Usage.CacheReadInputTokens, importedTotals.CacheReadTokens)
	if sealed.Usage.CacheCreationInputTokens != nil {
		assertIntPointer(
			t, sealed.Usage.CacheCreationInputTokens,
			importedTotals.CacheCreationTokens,
		)
	}
	require.NotNil(t, sealed.Cost)
	assert.True(t, imported.HasCost)
	assert.Equal(t, sealed.Cost.Amount, imported.Cost)
	assert.Equal(t, sealed.Cost.Source, imported.CostSource)
}

func assertIntPointer(t *testing.T, value *int, want int) {
	t.Helper()
	require.NotNil(t, value)
	assert.Equal(t, want, *value)
}
