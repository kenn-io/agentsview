package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/capture"
)

type lookupFixture struct {
	ID             string `json:"id"`
	AgeHours       int    `json:"age_hours"`
	Title          string `json:"title"`
	Content        string `json:"content"`
	PaddingRepeats int    `json:"padding_repeats,omitempty"`
}

type lookupQuestion struct {
	Name           string            `json:"name"`
	Prompt         string            `json:"prompt"`
	Expected       []lookupItem      `json:"expected"`
	ExpectedAnswer map[string]string `json:"expected_answer"`
}

type lookupAttempt struct {
	Case       string      `json:"case"`
	Attempt    int         `json:"attempt"`
	Score      lookupScore `json:"score"`
	DurationMS int64       `json:"duration_ms"`
	Error      string      `json:"error,omitempty"`
	Artifacts  string      `json:"artifacts"`
}

type lookupSummary struct {
	Case           string  `json:"case"`
	Scheduled      int     `json:"scheduled"`
	Success        int     `json:"success"`
	Failed         int     `json:"failed"`
	Unscored       int     `json:"unscored"`
	OneCallPercent float64 `json:"one_call_percent"`
	TwoCallPercent float64 `json:"two_call_percent"`
	MCP            int     `json:"mcp_calls"`
	CLI            int     `json:"cli_calls"`
	Direct         int     `json:"direct_calls"`
	Other          int     `json:"other_calls"`
	Errors         int     `json:"errors"`
	Empty          int     `json:"empty"`
	Truncated      int     `json:"truncated"`
	MissingCalls   int     `json:"missing_calls"`
}

type lookupClientReport struct {
	Client   clientName      `json:"client"`
	Version  string          `json:"version"`
	Model    string          `json:"model"`
	Effort   string          `json:"effort"`
	Attempts []lookupAttempt `json:"attempts"`
	Summary  []lookupSummary `json:"summary"`
}

type lookupReport struct {
	Revision   string               `json:"revision"`
	StartedAt  time.Time            `json:"started_at"`
	FinishedAt time.Time            `json:"finished_at"`
	Clients    []lookupClientReport `json:"clients"`
}

var errLookupSetup = errors.New("lookup client setup failed")

func readLookupJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func readLookupCorpus(repository string) ([]lookupFixture, []lookupQuestion, error) {
	root := filepath.Join(repository, "scripts", "memory-e2e", "testdata", "lookups")
	var fixtures []lookupFixture
	var questions []lookupQuestion
	if err := readLookupJSON(filepath.Join(root, "corpus.json"), &fixtures); err != nil {
		return nil, nil, err
	}
	if err := readLookupJSON(filepath.Join(root, "questions.json"), &questions); err != nil {
		return nil, nil, err
	}
	return fixtures, questions, nil
}

func selectLookupCorpus(cfg config) ([]lookupFixture, []lookupQuestion, error) {
	fixtures, questions, err := readLookupCorpus(cfg.repository)
	if err != nil {
		return nil, nil, err
	}
	if cfg.caseName != "" {
		selected := questions[:0]
		for _, q := range questions {
			if q.Name == cfg.caseName {
				selected = append(selected, q)
			}
		}
		questions = selected
		if len(questions) == 0 {
			return nil, nil, fmt.Errorf("memory-e2e: unknown --case %q", cfg.caseName)
		}
	}
	return fixtures, questions, nil
}

func runLookupSuite(ctx context.Context, cfg config, bin string, fixtures []lookupFixture, questions []lookupQuestion) error {
	clients, _ := selectedClients(cfg.clients)
	report := lookupReport{Revision: repositoryRevision(ctx, cfg.repository), StartedAt: time.Now().UTC()}
	path := filepath.Join(cfg.outputDir, "lookup-result.json")
	for _, client := range clients {
		result, err := runLookupClient(ctx, cfg, bin, client, fixtures, questions)
		report.Clients = append(report.Clients, result)
		report.FinishedAt = time.Now().UTC()
		if writeErr := writeJSON(path, report); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return err
		}
	}
	fmt.Printf("lookup report: %s\n", path)

	return nil
}

func seedLookupCorpus(
	ctx context.Context, bin, dataDir, projectDir, fixtureRoot string,
	fixtures []lookupFixture,
) error {
	for _, f := range fixtures {
		path, err := writeLookupFixtureTranscript(fixtureRoot, projectDir, f)
		if err != nil {
			return err
		}
		id, err := syncSourceSessionWithEnv(ctx, bin, dataDir, path, []string{"CODEX_SESSIONS_DIR=" + fixtureRoot, "CODEX_HOME=" + filepath.Dir(fixtureRoot)})
		if err != nil {
			return err
		}
		if id != "codex:"+f.ID {
			return fmt.Errorf("memory-e2e: unexpected fixture session id %q", id)
		}
	}
	return nil
}

func writeLookupFixtureTranscript(root, projectDir string, f lookupFixture) (string, error) {
	when := time.Now().UTC().Add(-time.Duration(f.AgeHours) * time.Hour)
	at := when.Format(time.RFC3339)
	dir := filepath.Join(root, when.Format("2006"), when.Format("01"), when.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	meta := map[string]any{"id": f.ID, "cwd": projectDir, "originator": "codex_cli_rs", "source": "cli"}
	content := f.Content
	if f.PaddingRepeats > 0 {
		content = f.Title + " examined the archive. " + strings.Repeat("Background detail without a decision. ", f.PaddingRepeats) + f.Content
	}
	lines := []any{
		map[string]any{"timestamp": at, "type": "session_meta", "payload": meta},
		map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": f.Title}}}},
		map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}}},
		map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Record this update for the project."}}}},
		map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Recorded."}}}},
	}
	var body bytes.Buffer
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			return "", err
		}
		body.Write(b)
		body.WriteByte('\n')
	}
	path := filepath.Join(dir, "rollout-"+when.Format("2006-01-02T15-04-05")+"-"+f.ID+".jsonl")
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func runLookupClient(
	ctx context.Context, cfg config, bin string, client clientName,
	fixtures []lookupFixture, questions []lookupQuestion,
) (lookupClientReport, error) {
	dir := filepath.Join(cfg.outputDir, string(client))
	project, data := filepath.Join(dir, "lookup-project"), filepath.Join(dir, "agentsview-data")
	for _, path := range []string{project, data} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return lookupClientReport{}, err
		}
	}
	if err := initFixtureProject(ctx, project); err != nil {
		return lookupClientReport{}, err
	}
	fixtureRoot := filepath.Join(dir, "fixture-sessions")
	if err := seedLookupCorpus(ctx, bin, data, project, fixtureRoot, fixtures); err != nil {
		return lookupClientReport{}, err
	}
	server, err := startFixtureServer(ctx, bin, data, dir)
	if err != nil {
		return lookupClientReport{}, err
	}
	defer server.stop()
	model := cfg.claudeModel
	if client == clientCodex {
		model = cfg.codexModel
	}
	report := lookupClientReport{Client: client, Version: commandOutput(ctx, project, string(client), "--version"), Model: model, Effort: cfg.effort}
	for _, q := range questions {
		for i := 1; i <= cfg.runs; i++ {
			report.Attempts = append(report.Attempts, lookupAttempt{Case: q.Name, Attempt: i, Score: lookupScore{Verdict: "unscored"}})
		}
	}
	index := 0
	for _, q := range questions {
		for i := 1; i <= cfg.runs; i++ {
			runDir := filepath.Join(dir, q.Name, fmt.Sprintf("run-%d", i))
			started := time.Now()
			calls, answer, runErr := captureLookup(ctx, cfg, bin, client, project, model, server.url, lookupPrompt(q), runDir)
			if !lookupClientVersionSupported(client, report.Version) {
				runErr = errors.Join(runErr, fmt.Errorf("%w: %s %q", errLookupCaptureUnsupported, client, report.Version))
			}
			if errors.Is(runErr, errLookupSetup) {
				return report, runErr
			}
			a := &report.Attempts[index]
			a.DurationMS = time.Since(started).Milliseconds()
			a.Error = scoreError(runErr)
			a.Artifacts = relativeArtifact(cfg.outputDir, runDir)
			a.Score = scoreLookupQuestion(q, answer, calls, runErr)
			fmt.Printf("%s %-16s run %d: %s\n", client, q.Name, i, a.Score.Verdict)
			index++
		}
	}
	report.Summary = summarizeLookups(report.Attempts)
	return report, nil
}

func lookupPrompt(q lookupQuestion) string {
	fields := make([]string, 0, len(q.ExpectedAnswer)+1)
	for field := range q.ExpectedAnswer {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	fields = append(fields, "source_session_id")
	fieldList, _ := json.Marshal(fields)
	return fmt.Sprintf("%s\n\nReturn only a JSON object with string fields %s. Copy each answer value exactly from the source, and include no other fields.", q.Prompt, fieldList)
}

func lookupClientVersionSupported(client clientName, version string) bool {
	switch client {
	case clientClaude:
		return version == "2.1.332 (Claude Code)"
	case clientCodex:
		return version == "codex-cli 0.172.0"
	default:
		return false
	}
}

func captureLookup(ctx context.Context, cfg config, bin string, client clientName, project, model, serverURL, prompt, dir string) ([]lookupCall, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("%w: create attempt: %w", errLookupSetup, err)
	}
	if err := copyLookupTranscriptsForClient(ctx, client, filepath.Join(filepath.Dir(project), "fixture-sessions"), filepath.Join(dir, "home"), project); err != nil {
		return nil, "", fmt.Errorf("%w: copy fixture transcripts: %w", errLookupSetup, err)
	}
	isolation, err := prepareLookupIsolation(ctx, client, filepath.Dir(project), cfg.repository, project, bin, dir)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s: %w", errLookupSetup, client, err)
	}
	defer isolation.removeCredentials()
	var args []string
	if client == clientClaude {
		mcpConfig, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"agentsview": map[string]any{"command": bin, "args": []string{"mcp", "--profile", "full", "--server", serverURL}}}})
		if err != nil {
			return nil, "", err
		}
		args = []string{"claude", "-p", prompt, "--model", model, "--effort", cfg.effort, "--output-format", "stream-json", "--verbose", "--setting-sources", "project", "--strict-mcp-config", "--mcp-config", string(mcpConfig), "--permission-mode", "dontAsk", "--allowedTools", "Read,Grep,Glob,Bash,mcp__agentsview__*"}
	} else {
		args = []string{"codex", "exec", "--json", "--ignore-user-config", "--enable", "skip_host_skill_discovery", "--disable", "plugins", "--disable", "recommended_plugins", "--disable", "mcp_2026_07_28", "--model", model, "-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.effort), "-c", fmt.Sprintf(`mcp_servers.agentsview.command=%q`, bin), "-c", fmt.Sprintf(`mcp_servers.agentsview.args=["mcp","--profile","full","--server",%q]`, serverURL), "--sandbox", "danger-full-access", "--skip-git-repo-check", prompt}
	}
	args[0] = isolation.command
	var stdout, stderr bytes.Buffer
	runCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	outcome, err := capture.Run(runCtx, capture.RunOptions{Provider: capture.Provider(client), OccurrenceID: newUUID(), CaptureDir: filepath.Join(dir, "capture"), ResultPath: filepath.Join(dir, "capture-result.json"), ProviderRoot: isolation.providerRoot, WorkDir: project, Command: args, Environment: isolation.env, Streams: capture.Streams{Stdout: &stdout, Stderr: &stderr}})
	if writeErr := os.WriteFile(filepath.Join(dir, "trace.jsonl"), stdout.Bytes(), 0o600); writeErr != nil {
		return nil, "", fmt.Errorf("%w: write trace: %w", errLookupSetup, writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "stderr.txt"), stderr.Bytes(), 0o600); writeErr != nil {
		return nil, "", fmt.Errorf("%w: write stderr: %w", errLookupSetup, writeErr)
	}
	parsed, parseErr := parseAgentTrace(client, stdout.Bytes())
	calls, captureErr := readLookupTranscriptCalls(ctx, client, dir, stdout.Bytes())
	var runErr error
	if runCtx.Err() != nil {
		runErr = runCtx.Err()
	} else if len(calls) == 0 && lookupTraceSetupError(stdout.Bytes()) {
		runErr = fmt.Errorf("%w: %s: %s %s", errLookupSetup, client, parsed.FinalAnswer, strings.TrimSpace(stderr.String()))
	} else if outcome.Execution.ExitCode == nil || *outcome.Execution.ExitCode != 0 {
		runErr = fmt.Errorf("%w: exit %d", errLookupClientFailed, outcome.ExitCode)
		if err != nil {
			runErr = fmt.Errorf("%w: %w", runErr, err)
		}
		if len(calls) == 0 && parsed.FinalAnswer == "" {
			runErr = fmt.Errorf("%w: %s: exit %d: %s", errLookupSetup, client, outcome.ExitCode, strings.TrimSpace(stderr.String()))
			if err != nil {
				runErr = fmt.Errorf("%w: %w", runErr, err)
			}
		}
	} else if parseErr != nil {
		runErr = fmt.Errorf("%w: parse trace: %w", errCaptureIncomplete, parseErr)
	} else if err != nil {
		runErr = fmt.Errorf("%w: %w", errCaptureIncomplete, err)
	}
	return calls, parsed.FinalAnswer, errors.Join(runErr, captureErr)
}

func lookupTraceSetupError(trace []byte) bool {
	found := false
	_ = scanJSONLEvents(trace, 4*1024*1024, false, func(event map[string]any) {
		if event["type"] == "result" && event["is_error"] == true {
			message := strings.ToLower(rawText(event["result"]))
			for _, term := range []string{"model", "auth", "credential", "log in", "login", "sign in", "unauthorized"} {
				found = found || strings.Contains(message, term)
			}
		}
	})
	return found
}

func readLookupTranscriptCalls(
	ctx context.Context, client clientName, dir string, trace []byte,
) ([]lookupCall, error) {
	traceCalls, traceErr := extractLookupCalls(ctx, client, [][]byte{trace})
	captureErr := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w: %w", errCaptureIncomplete, err)
	}
	resultFile, err := os.Open(filepath.Join(dir, "capture-result.json"))
	if err != nil {
		return traceCalls, errors.Join(captureErr(err), traceErr)
	}
	result, resultErr := capture.DecodeResult(resultFile)
	closeErr := resultFile.Close()
	if resultErr != nil {
		return traceCalls, errors.Join(captureErr(resultErr), traceErr)
	}
	if closeErr != nil {
		return traceCalls, errors.Join(captureErr(closeErr), traceErr)
	}
	var captureQualityErr error
	if result.Reporting.Outcome != capture.ReportingComplete {
		captureQualityErr = errors.Join(captureQualityErr,
			captureErr(fmt.Errorf("capture reporting is %s", result.Reporting.Outcome)))
	}
	if slices.Contains(result.Assurance.Reasons, capture.ReasonUnfinishedSession) || slices.Contains(result.Assurance.Reasons, capture.ReasonMalformedTranscript) {
		captureQualityErr = errors.Join(captureQualityErr,
			captureErr(fmt.Errorf("capture assurance: %v", result.Assurance.Reasons)))
	}
	f, err := os.Open(filepath.Join(dir, "capture", "sources", "bundle.json"))
	if err != nil {
		return traceCalls, errors.Join(captureQualityErr, captureErr(err), traceErr)
	}
	bundle, err := capture.DecodeTranscriptBundle(f)
	closeErr = f.Close()
	if err != nil {
		return traceCalls, errors.Join(captureQualityErr, captureErr(err), traceErr)
	}
	if closeErr != nil {
		return traceCalls, errors.Join(captureQualityErr, captureErr(closeErr), traceErr)
	}
	var transcripts [][]byte
	for _, source := range bundle.Sources {
		b, err := os.ReadFile(filepath.Join(dir, "capture", "sources", filepath.FromSlash(source.RawSource.Path)))
		if err != nil {
			return traceCalls, errors.Join(captureQualityErr, captureErr(err), traceErr)
		}
		transcripts = append(transcripts, b)
	}
	calls, err := extractLookupCalls(ctx, client, transcripts)
	if err != nil {
		captureQualityErr = errors.Join(captureQualityErr, captureErr(err))
	}
	if len(calls) == 0 && len(traceCalls) > 0 {
		calls = traceCalls
	}
	return calls, errors.Join(captureQualityErr, traceErrIfNoTranscript(traceErr, len(transcripts)))
}

func traceErrIfNoTranscript(err error, transcriptCount int) error {
	if transcriptCount > 0 {
		return nil
	}
	return err
}

func summarizeLookups(attempts []lookupAttempt) []lookupSummary {
	var result []lookupSummary
	for _, a := range attempts {
		if len(result) == 0 || result[len(result)-1].Case != a.Case {
			result = append(result, lookupSummary{Case: a.Case})
		}
		r := &result[len(result)-1]
		r.Scheduled++
		r.MCP += a.Score.MCP
		r.CLI += a.Score.CLI
		r.Direct += a.Score.Direct
		r.Other += a.Score.Other
		r.Errors += a.Score.Errors
		r.Empty += a.Score.Empty
		r.Truncated += a.Score.Truncated
		r.MissingCalls += a.Score.MissingCalls
		switch a.Score.Verdict {
		case "success":
			r.Success++
			if *a.Score.CallsToHit <= 1 {
				r.OneCallPercent++
			}
			if *a.Score.CallsToHit <= 2 {
				r.TwoCallPercent++
			}
		case "failed":
			r.Failed++
		default:
			r.Unscored++
		}
	}
	for i := range result {
		r := &result[i]
		r.OneCallPercent = 100 * r.OneCallPercent / float64(r.Scheduled)
		r.TwoCallPercent = 100 * r.TwoCallPercent / float64(r.Scheduled)
	}
	return result
}
