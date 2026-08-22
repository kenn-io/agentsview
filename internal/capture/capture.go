package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

const ReportFailureExitCode = 70

type RunOptions struct {
	Provider          Provider
	OccurrenceID      string
	CaptureDir        string
	ResultPath        string
	ProviderRoot      string
	ProviderSessionID string
	ClaudeWorkDir     string
	WorkDir           string
	Command           []string
	Environment       []string
	Streams           Streams
	Limits            Limits
	CustomPricing     map[string]config.CustomModelRate
	AgentsViewVersion string
}

type ReportOptions struct {
	CaptureDir        string
	ResultPath        string
	Stdout            ioWriter
	CustomPricing     map[string]config.CustomModelRate
	LoadCustomPricing func() (map[string]config.CustomModelRate, error)
	AgentsViewVersion string
}

type ioWriter interface {
	Write([]byte) (int, error)
}

type RunOutcome struct {
	ExitCode  int
	Execution ExecutionOutcome
	Reporting ReportingOutcome
}

type capturedChild struct {
	execution       ExecutionOutcome
	exitCode        int
	preReportReason ReasonCode
	preReportErr    error
	postStartErr    error
}

type preparedRun struct {
	limits          Limits
	argv            []string
	sessionID       string
	childWorkDir    string
	providerWorkDir string
	root            string
	resultPath      string
}

func prepareRun(opts RunOptions) (preparedRun, error) {
	if opts.OccurrenceID == "" {
		return preparedRun{}, errors.New("occurrence ID is required")
	}
	limits := opts.Limits
	if limits.MaxOccurrenceBytes == 0 {
		limits = DefaultLimits()
	}
	if err := validateLimits(limits); err != nil {
		return preparedRun{}, err
	}
	if len(opts.OccurrenceID) > limits.MaxOccurrenceBytes {
		return preparedRun{}, errors.New("occurrence ID exceeds byte limit")
	}
	if opts.ResultPath == "" || opts.ResultPath == "-" {
		return preparedRun{}, errors.New("capture run requires a result file path")
	}
	resultPath, err := validateResultPath(opts.CaptureDir, opts.ResultPath)
	if err != nil {
		return preparedRun{}, err
	}
	invocation, err := prepareInvocation(
		opts.Provider, append([]string(nil), opts.Command...), opts.ProviderSessionID)
	if err != nil {
		return preparedRun{}, err
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			return preparedRun{}, fmt.Errorf(
				"determining child working directory: %w", err)
		}
	}
	workDir, err = resolveWorkDir(workDir)
	if err != nil {
		return preparedRun{}, fmt.Errorf("resolving child working directory: %w", err)
	}
	providerWorkDir := workDir
	if opts.ClaudeWorkDir != "" {
		if opts.Provider != ProviderClaude || invocation.direct {
			return preparedRun{}, errors.New(
				"--claude-work-dir is only valid for a Claude wrapper")
		}
		providerWorkDir, err = resolveWorkDir(opts.ClaudeWorkDir)
		if err != nil {
			return preparedRun{}, fmt.Errorf(
				"resolving Claude working directory: %w", err)
		}
	} else if opts.Provider == ProviderClaude && !invocation.direct {
		return preparedRun{}, errors.New(
			"a Claude wrapper requires --claude-work-dir with its actual Claude working directory")
	}
	root, err := producerRoot(opts.Provider, opts.ProviderRoot)
	if err != nil {
		return preparedRun{}, fmt.Errorf("resolving provider session root: %w", err)
	}
	if err := validateProviderRootPath(opts.CaptureDir, root); err != nil {
		return preparedRun{}, err
	}
	if err := validateResultOutsideProviderRoot(root, resultPath); err != nil {
		return preparedRun{}, err
	}
	return preparedRun{
		limits: limits, argv: invocation.argv, sessionID: invocation.sessionID,
		childWorkDir: workDir, providerWorkDir: providerWorkDir,
		root: root, resultPath: resultPath,
	}, nil
}

func resolveWorkDir(dir string) (string, error) {
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if physical, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = physical
	}
	return resolved, nil
}

func Run(ctx context.Context, opts RunOptions) (RunOutcome, error) {
	prepared, err := prepareRun(opts)
	if err != nil {
		return RunOutcome{}, err
	}
	var reservation *claudeSessionReservation
	if opts.Provider == ProviderClaude {
		reservation, err = reserveClaudeSession(
			ctx, prepared.root, prepared.sessionID,
		)
		if err != nil {
			return RunOutcome{}, err
		}
		defer reservation.close()
		if err := rejectExistingClaudeSources(
			prepared.root, prepared.sessionID, prepared.limits,
		); err != nil {
			return RunOutcome{}, err
		}
	}
	started := time.Now()
	state, err := createState(opts.CaptureDir, manifest{
		OccurrenceID:      opts.OccurrenceID,
		Provider:          string(opts.Provider),
		ProviderSessionID: prepared.sessionID,
		ProviderRoot:      prepared.root,
		ProviderWorkDir:   prepared.providerWorkDir,
		StartedAt:         started,
		Execution:         ExecutionOutcome{StartedAt: started.UTC()},
		Invocation:        invocationName(opts.Provider),
		AgentsViewVersion: bounded(opts.AgentsViewVersion, 128),
		Limits:            prepared.limits,
	})
	if err != nil {
		return RunOutcome{}, err
	}
	defer state.close()
	if err := invalidateResult(prepared.resultPath); err != nil {
		rollbackErr := state.discardNewState()
		return RunOutcome{}, errors.Join(err, rollbackErr)
	}

	streams := defaultStreams(opts.Streams)
	child := runCaptureChild(state, opts, prepared, streams)
	result, reportErr := reportRun(
		ctx, state, opts, child.execution,
		child.preReportErr, child.preReportReason)
	reportErr = errors.Join(reportErr, child.postStartErr)
	return storeRunResult(
		state, prepared.resultPath, streams, child.execution, child.exitCode,
		result, reportErr)
}

func runCaptureChild(
	state *captureState,
	opts RunOptions,
	prepared preparedRun,
	streams Streams,
) capturedChild {
	env := opts.Environment
	if env == nil {
		env = os.Environ()
	}
	var marker *threadMarker
	if opts.Provider == ProviderCodex {
		marker = newCodexThreadMarker(state, prepared.limits.MaxLineBytes)
	}
	execution, childExitCode, started, childRunErr := runChild(
		prepared.argv, env, prepared.childWorkDir, streams, marker)
	state.manifest.Execution = execution
	var preReportErr error
	var postStartErr error
	preReportReason := ReasonCode("")
	if childRunErr != nil {
		if started {
			postStartErr = childRunErr
		} else {
			preReportErr = childRunErr
			preReportReason = ReasonChildStartFailed
			state.manifest.TerminalError = ReasonChildStartFailed
		}
	}
	if marker != nil && started {
		if markerErr := marker.persistenceError(); markerErr != nil {
			preReportErr = errors.Join(preReportErr, markerErr)
			preReportReason = ReasonIngestFailed
		}
		id, markerErr := marker.result()
		if markerErr != nil {
			preReportErr = errors.Join(preReportErr, markerErr)
			preReportReason = reasonForError(
				markerErr, ReasonCorrelationUnavailable)
			state.manifest.ProviderSessionID = ""
			state.manifest.CorrelationError = preReportReason
		} else {
			state.manifest.ProviderSessionID = id
		}
	}
	if err := state.saveManifest(); err != nil {
		preReportErr = errors.Join(preReportErr, err)
		preReportReason = ReasonIngestFailed
	}
	return capturedChild{
		execution: execution, exitCode: childExitCode,
		preReportReason: preReportReason, preReportErr: preReportErr,
		postStartErr: postStartErr,
	}
}

func newCodexThreadMarker(state *captureState, maxLineBytes int) *threadMarker {
	return &threadMarker{
		max: maxLineBytes,
		onID: func(id string) error {
			state.manifest.ProviderSessionID = id
			state.manifest.CorrelationError = ""
			return state.saveManifest()
		},
		onInvalid: func(reason ReasonCode) error {
			state.manifest.ProviderSessionID = ""
			state.manifest.CorrelationError = reason
			return state.saveManifest()
		},
	}
}

func reportRun(
	ctx context.Context,
	state *captureState,
	opts RunOptions,
	execution ExecutionOutcome,
	preReportErr error,
	preReportReason ReasonCode,
) (Result, error) {
	if preReportErr != nil {
		return failureResult(
			state.manifest, preReportReason, opts.AgentsViewVersion), preReportErr
	}
	state.manifest.Execution = execution
	return finalize(ctx, state, opts.CustomPricing, opts.AgentsViewVersion)
}

func storeRunResult(
	state *captureState,
	resultPath string,
	streams Streams,
	execution ExecutionOutcome,
	childExitCode int,
	result Result,
	reportErr error,
) (RunOutcome, error) {
	result, data, reportErr := encodeAndStoreResult(state, result, reportErr)
	if data != nil {
		reportErr = errors.Join(
			reportErr, writeCaptureResult(state, resultPath, streams.Stdout, data))
	}
	exitCode := executionExitCode(execution, childExitCode)
	if exitCode == 0 && reportErr != nil {
		exitCode = ReportFailureExitCode
	}
	return RunOutcome{
		ExitCode: exitCode, Execution: execution, Reporting: result.Reporting,
	}, reportErr
}

func Report(ctx context.Context, opts ReportOptions) (ReportingOutcome, error) {
	if opts.ResultPath == "" {
		return ReportingOutcome{}, errors.New("result path is required")
	}
	resultPath, err := validateResultPath(opts.CaptureDir, opts.ResultPath)
	if err != nil {
		return ReportingOutcome{}, err
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	state, err := openState(opts.CaptureDir)
	if err != nil {
		return ReportingOutcome{}, err
	}
	defer state.close()
	if err := validateResultOutsideProviderRoot(
		state.manifest.ProviderRoot, resultPath,
	); err != nil {
		return ReportingOutcome{}, err
	}
	if state.manifest.SealedDigest != "" {
		data, err := state.readSealed()
		if err != nil {
			return ReportingOutcome{}, err
		}
		result, err := DecodeResult(strings.NewReader(string(data)))
		if err != nil {
			return ReportingOutcome{}, err
		}
		return result.Reporting, writeCaptureResult(state, resultPath, stdout, data)
	}
	if state.manifest.TerminalError != "" {
		result, terminalErr := terminalManifestFailure(
			state.manifest, opts.AgentsViewVersion,
		)
		return storeReportResult(state, resultPath, stdout, result, terminalErr)
	}
	customPricing := opts.CustomPricing
	if opts.LoadCustomPricing != nil {
		customPricing, err = opts.LoadCustomPricing()
		if err != nil {
			return storeReportResult(
				state, resultPath, stdout,
				failureResult(
					state.manifest, ReasonIngestFailed, opts.AgentsViewVersion,
				),
				err,
			)
		}
	}
	result, reportErr := finalize(ctx, state, customPricing, opts.AgentsViewVersion)
	return storeReportResult(state, resultPath, stdout, result, reportErr)
}

func storeReportResult(
	state *captureState,
	resultPath string,
	stdout ioWriter,
	result Result,
	reportErr error,
) (ReportingOutcome, error) {
	result, data, reportErr := encodeAndStoreResult(state, result, reportErr)
	if data != nil {
		reportErr = errors.Join(
			reportErr, writeCaptureResult(state, resultPath, stdout, data))
	}
	return result.Reporting, reportErr
}

func encodeAndStoreResult(
	state *captureState,
	result Result,
	reportErr error,
) (Result, []byte, error) {
	data, err := encodeResult(result, state.manifest.Limits.MaxResultBytes)
	if err != nil {
		reportErr = errors.Join(reportErr, err)
		if reasonForError(err, "") == ReasonResultSizeLimit {
			return storeResultSizeFailure(state, result, reportErr)
		}
		return result, nil, reportErr
	}
	complete := result.Reporting.Outcome == ReportingComplete
	previous, previousData, err := existingFailedAttempt(state)
	if err != nil {
		return result, nil, errors.Join(reportErr, err)
	}
	if err := state.storeAttempt(data, complete); err == nil {
		return result, data, reportErr
	} else if errors.Is(err, errAttemptExists) {
		return preservedFailedAttempt(state, reportErr)
	} else if complete && previousData != nil {
		restoreErr := state.restoreAttempt(previousData)
		return previous, previousData, errors.Join(reportErr, err, restoreErr)
	} else if !complete || !errors.Is(err, context.DeadlineExceeded) {
		return result, nil, errors.Join(reportErr, err)
	} else {
		reportErr = errors.Join(reportErr, err)
	}
	result = failureResult(
		state.manifest, ReasonFinalizationTimeout,
		result.Producer.AgentsViewVersion,
	)
	data, err = encodeResult(result, state.manifest.Limits.MaxResultBytes)
	if err == nil {
		err = state.storeAttempt(data, false)
	}
	if errors.Is(err, errAttemptExists) {
		return preservedFailedAttempt(state, reportErr)
	}
	if err != nil {
		return result, nil, errors.Join(reportErr, err)
	}
	return result, data, reportErr
}

func storeResultSizeFailure(
	state *captureState,
	oversized Result,
	reportErr error,
) (Result, []byte, error) {
	previous, previousData, err := existingFailedAttempt(state)
	if err != nil {
		return oversized, nil, errors.Join(reportErr, err)
	}
	if previousData != nil {
		return previous, previousData, reportErr
	}
	result := failureResult(
		state.manifest, ReasonResultSizeLimit,
		oversized.Producer.AgentsViewVersion,
	)
	data, err := encodeResult(result, state.manifest.Limits.MaxResultBytes)
	if err == nil {
		err = state.storeAttempt(data, false)
	}
	if errors.Is(err, errAttemptExists) {
		return preservedFailedAttempt(state, reportErr)
	}
	if err != nil {
		return result, nil, errors.Join(reportErr, err)
	}
	return result, data, reportErr
}

func preservedFailedAttempt(
	state *captureState, reportErr error,
) (Result, []byte, error) {
	result, data, err := existingFailedAttempt(state)
	if err != nil {
		return Result{}, nil, errors.Join(reportErr, err)
	}
	if data == nil {
		err = errors.New("existing capture result is not a failure envelope")
		return Result{}, nil, errors.Join(reportErr, err)
	}
	return result, data, reportErr
}

func existingFailedAttempt(
	state *captureState,
) (Result, []byte, error) {
	data, err := state.readAttempt()
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, nil, nil
	}
	if err != nil {
		return Result{}, nil, err
	}
	result, err := DecodeResult(bytes.NewReader(data))
	if err != nil {
		return Result{}, nil, err
	}
	if result.Reporting.Outcome != ReportingFailed {
		return Result{}, nil, nil
	}
	if result.OccurrenceID != state.manifest.OccurrenceID ||
		result.Provider.Name != state.manifest.Provider ||
		result.Provider.SessionID != state.manifest.ProviderSessionID {
		return Result{}, nil, errors.New(
			"existing capture failure does not match the occurrence")
	}
	return result, data, nil
}

func finalize(
	ctx context.Context,
	state *captureState,
	customPricing map[string]config.CustomModelRate,
	agentsViewVersion string,
) (Result, error) {
	if state.manifest.TerminalError != "" {
		return terminalManifestFailure(state.manifest, agentsViewVersion)
	}
	if state.manifest.CorrelationError != "" {
		err := errorWithReason(
			state.manifest.CorrelationError,
			"provider session correlation failed and cannot be recovered",
		)
		return failureResult(
			state.manifest, state.manifest.CorrelationError, agentsViewVersion), err
	}
	if state.manifest.Execution.CompletedAt == nil {
		err := errorWithReason(
			ReasonUnfinishedSession,
			"producer completion was not durably recorded; refusing exact recovery",
		)
		return failureResult(
			state.manifest, ReasonUnfinishedSession, agentsViewVersion), err
	}
	deadline := time.Now().Add(state.manifest.Limits.FinalizationWait)
	state.finalizationDeadline = deadline
	finalCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	persistedPaths, complete, err := state.loadPersistedSources(finalCtx)
	if err != nil {
		reason := reasonForError(err, ReasonIngestFailed)
		return failureResult(state.manifest, reason, agentsViewVersion), err
	}
	if complete {
		return ingestCaptureResult(
			finalCtx, state, persistedPaths, nil, customPricing,
			deadline, false, agentsViewVersion)
	}
	for {
		livePaths, err := awaitQuiescentSources(finalCtx, state, deadline)
		if err != nil {
			return captureFailure(state, err, agentsViewVersion)
		}
		if err := state.resetPersistedSources(finalCtx); err != nil {
			return captureFailure(state, err, agentsViewVersion)
		}
		persisted, err := state.persistSourcePaths(finalCtx, livePaths)
		if err != nil {
			if errors.Is(err, errSourceChanged) {
				if waitErr := waitForPoll(finalCtx, deadline); waitErr != nil {
					return captureFailure(state, waitErr, agentsViewVersion)
				}
				continue
			}
			return captureFailure(state, err, agentsViewVersion)
		}
		ingested, err := ingest(
			finalCtx, state, persisted.Paths, persisted.Snapshots,
			customPricing, deadline,
			Provider(state.manifest.Provider) == ProviderCodex,
		)
		if err != nil {
			if errors.Is(err, errSourceChanged) {
				if waitErr := waitForPoll(finalCtx, deadline); waitErr != nil {
					return captureFailure(state, waitErr, agentsViewVersion)
				}
				continue
			}
			return captureFailure(state, err, agentsViewVersion)
		}
		unchanged, verifyErr := liveSourcesUnchanged(
			finalCtx, state, ingested.LiveSnapshots, ingested.CodexSources)
		if verifyErr != nil {
			_ = ingested.close(finalCtx)
			return captureFailure(state, verifyErr, agentsViewVersion)
		}
		if !unchanged {
			_ = ingested.close(finalCtx)
			if err := waitForPoll(finalCtx, deadline); err != nil {
				return captureFailure(state, err, agentsViewVersion)
			}
			continue
		}
		if err := state.completeTranscriptBundle(
			finalCtx, ingested.Root.SourceVersion); err != nil {
			_ = ingested.close(finalCtx)
			return captureFailure(state, err, agentsViewVersion)
		}
		return finishIngestedResult(finalCtx, state, ingested, agentsViewVersion)
	}
}

func terminalManifestFailure(
	m manifest,
	agentsViewVersion string,
) (Result, error) {
	err := errorWithReason(
		m.TerminalError,
		"producer failed before execution started and cannot be recovered",
	)
	return failureResult(m, m.TerminalError, agentsViewVersion), err
}

func awaitQuiescentSources(
	ctx context.Context,
	state *captureState,
	deadline time.Time,
) ([]string, error) {
	var previous []sourceSnapshot
	var stableSince time.Time
	foundRoot := state.manifest.SourceObserved
	missingRoot := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, finalizationError(foundRoot, missingRoot, err)
		}
		roots, err := locateRoot(ctx, state.manifest)
		if err != nil {
			return nil, err
		}
		if len(roots) > 1 {
			return nil, errorWithReason(
				ReasonMultipleSessions,
				"multiple exact provider session sources found",
			)
		}
		if len(roots) == 0 {
			missingRoot = foundRoot
			previous = nil
			stableSince = time.Time{}
		} else {
			missingRoot = false
			foundRoot = true
			if !state.manifest.SourceObserved {
				state.manifest.SourceObserved = true
				if err := state.saveManifest(); err != nil {
					return nil, err
				}
			}
			paths, sourceErr := captureSourcePaths(ctx, state.manifest, roots[0])
			if sourceErr != nil {
				return nil, sourceErr
			}
			snapshot, snapshotErr := snapshotSources(ctx, paths, state.manifest.Limits)
			if snapshotErr != nil {
				if errors.Is(snapshotErr, os.ErrNotExist) {
					return nil, errorWithReason(
						ReasonSourceUnavailable,
						"observed capture source is unavailable",
					)
				}
				return nil, snapshotErr
			}
			if !sameSnapshots(previous, snapshot) {
				previous = snapshot
				stableSince = time.Now()
			} else if time.Since(stableSince) >= state.manifest.Limits.Quiescence {
				return paths, nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, finalizationError(
				foundRoot, missingRoot, context.DeadlineExceeded)
		}
		if err := waitForPoll(ctx, deadline); err != nil {
			return nil, finalizationError(foundRoot, missingRoot, err)
		}
	}
}

func finalizationError(foundRoot, missingRoot bool, err error) error {
	reason := ReasonFinalizationTimeout
	switch {
	case !foundRoot:
		reason = ReasonNoSession
	case missingRoot:
		reason = ReasonSourceUnavailable
	}
	return &reasonError{
		reason: reason,
		err:    fmt.Errorf("capture reporting failed: %s: %w", reason, err),
	}
}

func awaitStablePaths(
	ctx context.Context,
	paths []string,
	deadline time.Time,
	limits Limits,
) ([]string, error) {
	var previous []sourceSnapshot
	var stableSince time.Time
	for {
		snapshots, err := snapshotSources(ctx, paths, limits)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, errorWithReason(
					ReasonSourceUnavailable, "observed capture source is unavailable")
			}
			return nil, err
		}
		if !sameSnapshots(previous, snapshots) {
			previous = snapshots
			stableSince = time.Now()
		} else if time.Since(stableSince) >= limits.Quiescence {
			return paths, nil
		}
		if err := waitForPoll(ctx, deadline); err != nil {
			return nil, err
		}
	}
}

func captureSourcePaths(
	ctx context.Context,
	m manifest,
	root string,
) ([]string, error) {
	if Provider(m.Provider) != ProviderClaude {
		return []string{root}, nil
	}
	return claudeSources(ctx, root, m.Limits)
}

func liveSourcesUnchanged(
	ctx context.Context,
	state *captureState,
	expected []sourceSnapshot,
	codexSources []codexSourceSelection,
) (bool, error) {
	var paths []string
	if Provider(state.manifest.Provider) == ProviderClaude {
		roots, err := locateRoot(ctx, state.manifest)
		if err != nil {
			return false, err
		}
		if len(roots) == 0 {
			return false, errorWithReason(
				ReasonSourceUnavailable, "observed capture source is unavailable")
		}
		if len(roots) > 1 {
			return false, errorWithReason(
				ReasonMultipleSessions, "multiple exact provider session sources found")
		}
		paths, err = captureSourcePaths(ctx, state.manifest, roots[0])
		if err != nil {
			return false, err
		}
	} else {
		if len(codexSources) != len(expected) {
			return false, errorWithReason(
				ReasonSourceUnavailable,
				"selected Codex source set is incomplete",
			)
		}
		var err error
		paths, err = verifyCodexSourceSelections(ctx, state, codexSources)
		if err != nil {
			return false, err
		}
	}
	current, err := snapshotSources(ctx, paths, state.manifest.Limits)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, errorWithReason(
				ReasonSourceUnavailable, "observed capture source is unavailable")
		}
		return false, err
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].Path < expected[j].Path })
	sort.Slice(current, func(i, j int) bool { return current[i].Path < current[j].Path })
	return sameSnapshots(expected, current), nil
}

func verifyCodexSourceSelections(
	ctx context.Context,
	state *captureState,
	selections []codexSourceSelection,
) ([]string, error) {
	paths := make([]string, 0, len(selections))
	for _, selection := range selections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches, err := locateCodexRoot(
			ctx, state.manifest.ProviderRoot, selection.ID,
			selection.Anchor, state.manifest.Limits,
		)
		if err != nil {
			return nil, err
		}
		if len(matches) > 1 {
			return nil, errorWithReason(
				ReasonMultipleSessions,
				"multiple exact Codex session sources found",
			)
		}
		if len(matches) == 0 || matches[0] != selection.LivePath {
			return nil, errorWithReason(
				ReasonSourceUnavailable,
				"selected Codex session source is unavailable",
			)
		}
		paths = append(paths, matches[0])
	}
	return paths, nil
}

func ingestCaptureResult(
	ctx context.Context,
	state *captureState,
	paths []string,
	liveSnapshots []sourceSnapshot,
	customPricing map[string]config.CustomModelRate,
	deadline time.Time,
	discoverCodexChildren bool,
	agentsViewVersion string,
) (Result, error) {
	ingested, err := ingest(
		ctx, state, paths, liveSnapshots, customPricing,
		deadline, discoverCodexChildren)
	if err != nil {
		return captureFailure(state, err, agentsViewVersion)
	}
	return finishIngestedResult(ctx, state, ingested, agentsViewVersion)
}

func finishIngestedResult(
	ctx context.Context,
	state *captureState,
	ingested *ingestedCapture,
	agentsViewVersion string,
) (Result, error) {
	if err := ingested.close(ctx); err != nil {
		return captureFailure(state, err, agentsViewVersion)
	}
	if err := ctx.Err(); err != nil {
		return captureFailure(state, err, agentsViewVersion)
	}
	result, err := resultFromIngest(
		ctx, state.manifest, ingested, agentsViewVersion,
	)
	if err != nil {
		return captureFailure(state, err, agentsViewVersion)
	}
	return result, nil
}

func captureFailure(
	state *captureState,
	err error,
	agentsViewVersion string,
) (Result, error) {
	reason := reasonForError(err, ReasonIngestFailed)
	if reason == ReasonIngestFailed && errors.Is(err, context.DeadlineExceeded) {
		reason = ReasonFinalizationTimeout
	}
	return failureResult(state.manifest, reason, agentsViewVersion), err
}

func waitForPoll(ctx context.Context, deadline time.Time) error {
	wait := min(100*time.Millisecond, time.Until(deadline))
	if wait <= 0 {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sessionsFinal(root *db.Session, descendants []db.Session) bool {
	if !sessionFinal(root) {
		return false
	}
	for i := range descendants {
		if !sessionFinal(&descendants[i]) {
			return false
		}
	}
	return true
}

func sessionFinal(session *db.Session) bool {
	if session == nil || session.TerminationStatus == nil {
		return false
	}
	status := parser.TerminationStatus(*session.TerminationStatus)
	return status == parser.TerminationAwaitingUser || status == parser.TerminationClean
}

func sessionsHaveMalformedLines(root *db.Session, descendants []db.Session) bool {
	if root != nil && root.ParserMalformedLines > 0 {
		return true
	}
	for i := range descendants {
		if descendants[i].ParserMalformedLines > 0 {
			return true
		}
	}
	return false
}

func resultFromIngest(
	ctx context.Context,
	m manifest,
	ingested *ingestedCapture,
	agentsViewVersion string,
) (Result, error) {
	result := baseResult(m, agentsViewVersion)
	result.Reporting.Outcome = ReportingComplete
	result.Assurance.State = AssuranceComplete
	if !sessionsFinal(ingested.Root, ingested.Descendants) {
		result.Assurance.State = AssurancePartial
		result.Assurance.Reasons = append(
			result.Assurance.Reasons, ReasonUnfinishedSession)
	}
	if sessionsHaveMalformedLines(ingested.Root, ingested.Descendants) {
		result.Assurance.State = AssurancePartial
		result.Assurance.Reasons = append(
			result.Assurance.Reasons, ReasonMalformedTranscript)
	}
	result.Provider.SessionID = m.ProviderSessionID
	result.Provider.IncludedSessionIDs = []string{m.ProviderSessionID}
	for _, child := range ingested.Descendants {
		id := strings.TrimPrefix(child.ID, "codex:")
		result.Provider.IncludedSessionIDs = append(result.Provider.IncludedSessionIDs, id)
	}
	result.Provider.StartedAt = parseTime(ingested.Root.StartedAt)
	result.Provider.CompletedAt = parseTime(ingested.Root.EndedAt)
	for _, source := range m.Sources {
		result.Sources = append(result.Sources, SourceProvenance{
			SessionID: source.SessionID,
			SHA256:    source.RawSource.Hash,
			Bytes:     source.RawSource.Size,
		})
	}
	sort.Slice(result.Sources, func(i, j int) bool {
		return result.Sources[i].SessionID < result.Sources[j].SessionID
	})
	result.Producer.SourceVersion = bounded(ingested.Root.SourceVersion, 128)
	if result.Producer.SourceVersion != ingested.Root.SourceVersion {
		result.Assurance.State = AssurancePartial
		result.Assurance.Reasons = append(result.Assurance.Reasons, ReasonMetadataTruncated)
	}
	result.Models = append([]string(nil), ingested.Usage.Models...)
	if len(result.Models) > 32 {
		result.Models = result.Models[:32]
		result.Assurance.State = AssurancePartial
		result.Assurance.Reasons = append(result.Assurance.Reasons, ReasonMetadataTruncated)
	}
	for i := range result.Models {
		boundedModel := bounded(result.Models[i], 128)
		if boundedModel != result.Models[i] {
			result.Assurance.State = AssurancePartial
			result.Assurance.Reasons = append(result.Assurance.Reasons, ReasonMetadataTruncated)
		}
		result.Models[i] = boundedModel
	}
	if err := applyUsageResult(ctx, m.Provider, ingested.Usage, &result); err != nil {
		return Result{}, err
	}
	slices.Sort(result.Assurance.Reasons)
	result.Assurance.Reasons = compactReasons(result.Assurance.Reasons)
	return result, nil
}

func applyUsageResult(
	ctx context.Context, provider string, usageData *db.SessionUsage, result *Result,
) error {
	if usageData.HasTokenData {
		totals, complete, err := service.SessionUsageTokenTotals(
			ctx, usageData,
		)
		if err != nil {
			return err
		}
		usage := &TokenUsage{OutputTokens: &totals.OutputTokens}
		if complete {
			usage.InputTokens = &totals.InputTokens
			usage.CacheReadInputTokens = &totals.CacheReadTokens
			if Provider(provider) == ProviderClaude {
				usage.CacheCreationInputTokens = &totals.CacheCreationTokens
			}
		} else {
			result.Assurance.State = AssurancePartial
			result.Assurance.Reasons = append(result.Assurance.Reasons, ReasonUsageUnavailable)
		}
		if Provider(provider) == ProviderCodex {
			result.Assurance.State = AssurancePartial
			result.Assurance.Reasons = append(
				result.Assurance.Reasons,
				ReasonCodexCacheWriteAbsent, ReasonReasoningAbsent,
			)
		}
		result.Usage = usage
	} else {
		result.Assurance.State = AssuranceUnavailable
		result.Assurance.Reasons = append(result.Assurance.Reasons, ReasonUsageUnavailable)
	}
	transcriptCostIncomplete :=
		slices.Contains(result.Assurance.Reasons, ReasonUnfinishedSession) ||
			slices.Contains(result.Assurance.Reasons, ReasonMalformedTranscript)
	if usageData.HasCost && (!transcriptCostIncomplete ||
		usageData.CostSource == export.CostSourceReported) {
		result.Cost = &Cost{
			Amount: usageData.Cost, Currency: "USD", Source: usageData.CostSource,
		}
	} else if result.Usage != nil || transcriptCostIncomplete {
		if result.Assurance.State == AssuranceComplete {
			result.Assurance.State = AssurancePartial
		}
		result.Assurance.Reasons = append(
			result.Assurance.Reasons, ReasonCostUnavailable)
		if len(usageData.UnpricedModels) > 0 {
			result.Assurance.Reasons = append(
				result.Assurance.Reasons, ReasonUnpricedModel)
		}
	}
	return nil
}

func compactReasons(reasons []ReasonCode) []ReasonCode {
	if len(reasons) < 2 {
		return reasons
	}
	out := reasons[:1]
	for _, reason := range reasons[1:] {
		if reason != out[len(out)-1] {
			out = append(out, reason)
		}
	}
	return out
}

func baseResult(m manifest, agentsViewVersion string) Result {
	return Result{
		Schema:       Schema{Name: ResultSchemaName, Version: ResultSchemaVersion},
		OccurrenceID: m.OccurrenceID,
		Provider: ProviderIdentity{
			Name: m.Provider, IncludedSessionIDs: []string{},
		},
		Execution: m.Execution,
		Models:    []string{},
		Sources:   []SourceProvenance{},
		Assurance: Assurance{Reasons: []ReasonCode{}},
		Producer: ProducerMetadata{
			AgentsViewVersion: bounded(agentsViewVersion, 128),
			ParserDataVersion: db.CurrentDataVersion(),
			Invocation:        m.Invocation,
		},
	}
}

func failureResult(m manifest, reason ReasonCode, agentsViewVersion string) Result {
	result := baseResult(m, agentsViewVersion)
	result.Provider.SessionID = m.ProviderSessionID
	if m.ProviderSessionID != "" {
		result.Provider.IncludedSessionIDs = []string{m.ProviderSessionID}
	}
	result.Assurance = Assurance{State: AssuranceUnavailable, Reasons: []ReasonCode{reason}}
	result.Reporting = ReportingOutcome{Outcome: ReportingFailed, Reason: reason}
	return result
}

func encodeResult(result Result, maxBytes int) ([]byte, error) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > maxBytes {
		return nil, errorWithReason(
			ReasonResultSizeLimit, "capture result exceeds size limit")
	}
	return data, nil
}

func parseTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func bounded(value string, max int) string {
	if len(value) <= max {
		return value
	}
	for max > 0 && !utf8.ValidString(value[:max]) {
		max--
	}
	return value[:max]
}

func executionExitCode(outcome ExecutionOutcome, childExitCode int) int {
	if outcome.ExitCode == nil && outcome.Signal == "" {
		return ReportFailureExitCode
	}
	return childExitCode
}
