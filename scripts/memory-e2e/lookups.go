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
	ParentID       string `json:"parent_id,omitempty"`
	SpawnedChildID string `json:"spawned_child_id,omitempty"`
	ToolResult     string `json:"tool_result,omitempty"`
	PaddingRepeats int    `json:"padding_repeats,omitempty"`
}

type lookupSource struct {
	Provider  clientName `json:"provider"`
	Path      string     `json:"path"`
	SessionID string     `json:"session_id,omitempty"`
}

type lookupManifest struct {
	Fixtures  []lookupFixture  `json:"fixtures,omitempty"`
	Sources   []lookupSource   `json:"sources,omitempty"`
	Questions []lookupQuestion `json:"questions"`
}

type lookupQuestion struct {
	Name     string       `json:"name"`
	Prompt   string       `json:"prompt"`
	Expected []lookupItem `json:"expected"`
	Phrases  []string     `json:"phrases"`
}

type lookupAttempt struct {
	Case       string      `json:"case"`
	Attempt    int         `json:"attempt"`
	Score      lookupScore `json:"score"`
	DurationMS int64       `json:"duration_ms"`
	Error      string      `json:"error,omitempty"`
	Artifacts  string      `json:"artifacts"`
	Sources    int         `json:"sources"`
}

type lookupSummary struct {
	Case            string         `json:"case"`
	Scheduled       int            `json:"scheduled"`
	Success         int            `json:"success"`
	Failed          int            `json:"failed"`
	Unscored        int            `json:"unscored"`
	OneCallPercent  float64        `json:"one_call_percent"`
	TwoCallPercent  float64        `json:"two_call_percent"`
	ScoredSuccesses int            `json:"scored_successes"`
	P50             *int           `json:"p50_calls"`
	P90             *int           `json:"p90_calls"`
	MCP             int            `json:"mcp_calls"`
	CLI             int            `json:"cli_calls"`
	Direct          int            `json:"direct_calls"`
	Other           int            `json:"other_calls"`
	Errors          int            `json:"errors"`
	Empty           int            `json:"empty"`
	Truncated       int            `json:"truncated"`
	MissingCalls    int            `json:"missing_calls"`
	Routes          map[string]int `json:"routes"`
}

type lookupClientReport struct {
	Client           clientName      `json:"client"`
	Version          string          `json:"version"`
	Model            string          `json:"model"`
	Effort           string          `json:"effort"`
	ToolDeliveryMode string          `json:"tool_delivery_mode"`
	Supported        bool            `json:"supported"`
	PreflightError   string          `json:"preflight_error,omitempty"`
	Attempts         []lookupAttempt `json:"attempts"`
	Summary          []lookupSummary `json:"summary"`
}

type lookupReport struct {
	Revision   string               `json:"revision"`
	StartedAt  time.Time            `json:"started_at"`
	FinishedAt time.Time            `json:"finished_at"`
	Clients    []lookupClientReport `json:"clients"`
}

func readLookupJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func readLookupCorpus(repository, manifestPath string) ([]lookupFixture, []lookupSource, []lookupQuestion, string, error) {
	if manifestPath == "" {
		root := filepath.Join(repository, "scripts", "memory-e2e", "testdata", "lookups")
		var fixtures []lookupFixture
		var questions []lookupQuestion
		if err := readLookupJSON(filepath.Join(root, "corpus.json"), &fixtures); err != nil {
			return nil, nil, nil, "", err
		}
		if err := readLookupJSON(filepath.Join(root, "questions.json"), &questions); err != nil {
			return nil, nil, nil, "", err
		}
		return fixtures, nil, questions, root, nil
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, nil, "", err
	}
	var manifest lookupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, nil, "", fmt.Errorf("memory-e2e: decode lookup manifest: %w", err)
	}
	if len(manifest.Questions) == 0 {
		return nil, nil, nil, "", errors.New("memory-e2e: lookup manifest must contain questions")
	}
	if len(manifest.Fixtures)+len(manifest.Sources) == 0 {
		return nil, nil, nil, "", errors.New("memory-e2e: lookup manifest must contain fixtures or sources")
	}
	return manifest.Fixtures, manifest.Sources, manifest.Questions, filepath.Dir(manifestPath), nil
}

func runLookupSuite(ctx context.Context, cfg config, bin string) error {
	fixtures, sources, questions, sourceRoot, err := readLookupCorpus(cfg.repository, cfg.lookupManifest)
	if err != nil {
		return err
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
			return fmt.Errorf("memory-e2e: unknown --case %q", cfg.caseName)
		}
	}
	clients, _ := selectedClients(cfg.clients)
	report := lookupReport{Revision: repositoryRevision(ctx, cfg.repository), StartedAt: time.Now().UTC()}
	path := filepath.Join(cfg.outputDir, "lookup-result.json")
	for _, client := range clients {
		result, err := runLookupClient(ctx, cfg, bin, client, fixtures, sources, sourceRoot, questions)
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
	for _, c := range report.Clients {
		if !c.Supported {
			return fmt.Errorf("memory-e2e: %s lookup preflight unsupported: %s", c.Client, c.PreflightError)
		}
	}
	return nil
}

func seedLookupCorpus(
	ctx context.Context, bin, dataDir, projectDir string,
	fixtures []lookupFixture, sources []lookupSource, sourceRoot string,
) error {
	root := filepath.Join(projectDir, "transcripts")
	for _, f := range fixtures {
		when := time.Now().UTC().Add(-time.Duration(f.AgeHours) * time.Hour)
		at := when.Format(time.RFC3339)
		dir := filepath.Join(root, when.Format("2006"), when.Format("01"), when.Format("02"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		meta := map[string]any{"id": f.ID, "cwd": projectDir, "originator": "codex_exec", "source": "exec"}
		if f.ParentID != "" {
			meta["thread_source"] = "subagent"
			meta["parent_thread_id"] = f.ParentID
		}
		content := f.Content
		if f.PaddingRepeats > 0 {
			content = f.Title + " examined the archive. " + strings.Repeat("Background detail without a decision. ", f.PaddingRepeats) + f.Content
		}
		lines := []any{
			map[string]any{"timestamp": at, "type": "session_meta", "payload": meta},
			map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": f.Title}}}},
			map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}}},
		}
		lines = append(lines,
			map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Record this update for the project."}}}},
			map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Recorded."}}}},
		)
		if f.SpawnedChildID != "" {
			lines = append(lines,
				map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "spawn-" + f.ID, "arguments": "{}"}},
				map[string]any{"timestamp": at, "type": "event_msg", "payload": map[string]any{"type": "collab_agent_spawn_end", "call_id": "spawn-" + f.ID, "new_thread_id": f.SpawnedChildID}},
				map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "spawn-" + f.ID, "output": "{\"agent_id\":\"" + f.SpawnedChildID + "\"}"}},
			)
		}
		if f.ToolResult != "" {
			lines = append(lines,
				map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "call_id": "result-" + f.ID, "arguments": "{\"cmd\":\"check release\"}"}},
				map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "result-" + f.ID, "output": f.ToolResult}},
			)
		}
		var body bytes.Buffer
		for _, line := range lines {
			b, err := json.Marshal(line)
			if err != nil {
				return err
			}
			body.Write(b)
			body.WriteByte('\n')
		}
		path := filepath.Join(dir, "rollout-"+when.Format("2006-01-02T15-04-05")+"-"+f.ID+".jsonl")
		if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
			return err
		}
		id, err := syncSourceSessionWithEnv(ctx, bin, dataDir, path, []string{"CODEX_SESSIONS_DIR=" + root, "CODEX_HOME=" + filepath.Join(projectDir, "fixture-codex")})
		if err != nil {
			return err
		}
		if id != "codex:"+f.ID {
			return fmt.Errorf("memory-e2e: unexpected fixture session id %q", id)
		}
	}
	return seedLookupSources(ctx, bin, dataDir, projectDir, sources, sourceRoot)
}

func seedLookupSources(
	ctx context.Context, bin, dataDir, projectDir string,
	sources []lookupSource, sourceRoot string,
) error {
	for i, source := range sources {
		if source.Provider != clientClaude && source.Provider != clientCodex {
			return fmt.Errorf("memory-e2e: lookup source %d has unknown provider %q", i+1, source.Provider)
		}
		path := source.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(sourceRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("memory-e2e: read lookup source %d: %w", i+1, err)
		}
		name := filepath.Base(path)
		if name == "." || name == string(filepath.Separator) {
			return fmt.Errorf("memory-e2e: lookup source %d has invalid path", i+1)
		}
		codexHome := filepath.Join(projectDir, "fixture-codex")
		claudeHome := filepath.Join(projectDir, "fixture-claude")
		var destination string
		var env []string
		if source.Provider == clientCodex {
			root := filepath.Join(codexHome, "sessions")
			destination = filepath.Join(root, "2026", "11", "24", fmt.Sprintf("source-%d-%s", i+1, name))
			env = []string{"CODEX_SESSIONS_DIR=" + root, "CODEX_HOME=" + codexHome}
		} else {
			destination = filepath.Join(claudeHome, "projects", "lookup-sample", fmt.Sprintf("source-%d-%s", i+1, name))
			env = []string{"CLAUDE_CONFIG_DIR=" + claudeHome}
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return err
		}
		id, err := syncSourceSessionWithEnv(ctx, bin, dataDir, destination, env)
		if err != nil {
			return fmt.Errorf("memory-e2e: sync lookup source %d: %w", i+1, err)
		}
		if source.SessionID != "" && id != source.SessionID {
			return fmt.Errorf("memory-e2e: lookup source %d synced as %q, want %q", i+1, id, source.SessionID)
		}
	}
	return nil
}

func runLookupClient(
	ctx context.Context, cfg config, bin string, client clientName,
	fixtures []lookupFixture, sources []lookupSource, sourceRoot string, questions []lookupQuestion,
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
	if err := seedLookupCorpus(ctx, bin, data, project, fixtures, sources, sourceRoot); err != nil {
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
			report.Attempts = append(report.Attempts, lookupAttempt{Case: q.Name, Attempt: i, Score: lookupScore{Verdict: "unscored", Route: "none"}})
		}
	}
	if err := writeJSON(filepath.Join(dir, "scheduled.json"), report.Attempts); err != nil {
		return report, err
	}
	preflight, err := lookupPreflightPrompt(questions)
	if err != nil {
		return report, err
	}
	calls, _, _, preErr := captureLookup(ctx, cfg, bin, client, project, model, server.url, preflight, filepath.Join(dir, "preflight"))
	report.ToolDeliveryMode = lookupToolDeliveryMode(client, calls)
	if preErr != nil {
		report.PreflightError = fmt.Sprintf("direct search and read preflight failed: %v", preErr)
		report.Summary = summarizeLookups(report.Attempts)
		return report, nil
	}
	if !lookupPreflightComplete(calls) {
		report.PreflightError = "capture did not identify completed search and read results"
		report.Summary = summarizeLookups(report.Attempts)
		return report, nil
	}
	report.Supported = true
	index := 0
	for _, q := range questions {
		for i := 1; i <= cfg.runs; i++ {
			runDir := filepath.Join(dir, q.Name, fmt.Sprintf("run-%d", i))
			started := time.Now()
			calls, answer, sources, runErr := captureLookup(ctx, cfg, bin, client, project, model, server.url, q.Prompt, runDir)
			a := &report.Attempts[index]
			a.DurationMS = time.Since(started).Milliseconds()
			a.Error = scoreError(runErr)
			a.Artifacts = relativeArtifact(cfg.outputDir, runDir)
			a.Sources = sources
			a.Score = scoreLookup(answer, q.Expected, q.Phrases, calls, runErr)
			if err := writeJSON(filepath.Join(dir, "attempts.json"), report.Attempts); err != nil {
				return report, err
			}
			fmt.Printf("%s %-16s run %d: %s\n", client, q.Name, i, a.Score.Verdict)
			index++
		}
	}
	report.Summary = summarizeLookups(report.Attempts)
	return report, nil
}

func lookupPreflightPrompt(questions []lookupQuestion) (string, error) {
	for _, question := range questions {
		for _, item := range question.Expected {
			if item.SessionID != "" {
				return fmt.Sprintf("Use the AgentsView MCP search_sessions tool with session_id %q, then use get_messages to read that session. Cite its session ID.", item.SessionID), nil
			}
		}
	}
	return "", errors.New("memory-e2e: lookup questions must name an expected session for preflight")
}

func captureLookup(ctx context.Context, cfg config, bin string, client clientName, project, model, serverURL, prompt, dir string) ([]lookupCall, string, int, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", 0, err
	}
	isolation, err := prepareLookupIsolation(ctx, client, filepath.Dir(project), project, bin, dir)
	if err != nil {
		return nil, "", 0, err
	}
	defer isolation.removeCredentials()
	var args []string
	if client == clientClaude {
		mcpConfig, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"agentsview": map[string]any{"command": bin, "args": []string{"mcp", "--profile", "full", "--server", serverURL}}}})
		if err != nil {
			return nil, "", 0, err
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
		return nil, "", 0, writeErr
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "stderr.txt"), stderr.Bytes(), 0o600); writeErr != nil {
		return nil, "", 0, writeErr
	}
	parsed, parseErr := parseAgentTrace(client, stdout.Bytes())
	calls, sources, captureErr := readLookupTranscriptCalls(client, dir, stdout.Bytes())
	var runErr error
	if runCtx.Err() != nil {
		runErr = runCtx.Err()
	} else if outcome.Execution.ExitCode == nil || *outcome.Execution.ExitCode != 0 {
		runErr = fmt.Errorf("%w: exit %d: %v", errLookupClientFailed, outcome.ExitCode, err)
	} else if parseErr != nil {
		runErr = fmt.Errorf("%w: parse trace: %v", errCaptureIncomplete, parseErr)
	} else if err != nil {
		runErr = fmt.Errorf("%w: %v", errCaptureIncomplete, err)
	}
	return calls, parsed.FinalAnswer, sources, errors.Join(runErr, captureErr)
}

func readLookupTranscriptCalls(
	client clientName, dir string, trace []byte,
) ([]lookupCall, int, error) {
	traceCalls, traceErr := extractLookupCalls(client, [][]byte{trace})
	captureErr := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w: %v", errCaptureIncomplete, err)
	}
	resultFile, err := os.Open(filepath.Join(dir, "capture-result.json"))
	if err != nil {
		return traceCalls, 0, errors.Join(captureErr(err), traceErr)
	}
	result, resultErr := capture.DecodeResult(resultFile)
	closeErr := resultFile.Close()
	if resultErr != nil {
		return traceCalls, 0, errors.Join(captureErr(resultErr), traceErr)
	}
	if closeErr != nil {
		return traceCalls, 0, errors.Join(captureErr(closeErr), traceErr)
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
		return traceCalls, len(result.Sources), errors.Join(captureQualityErr, captureErr(err), traceErr)
	}
	bundle, err := capture.DecodeTranscriptBundle(f)
	closeErr = f.Close()
	if err != nil {
		return traceCalls, len(result.Sources), errors.Join(captureQualityErr, captureErr(err), traceErr)
	}
	if closeErr != nil {
		return traceCalls, len(result.Sources), errors.Join(captureQualityErr, captureErr(closeErr), traceErr)
	}
	var transcripts [][]byte
	for _, source := range bundle.Sources {
		b, err := os.ReadFile(filepath.Join(dir, "capture", "sources", filepath.FromSlash(source.RawSource.Path)))
		if err != nil {
			return traceCalls, len(transcripts), errors.Join(captureQualityErr, captureErr(err), traceErr)
		}
		transcripts = append(transcripts, b)
	}
	calls, err := extractLookupCalls(client, transcripts)
	if err != nil {
		captureQualityErr = errors.Join(captureQualityErr, captureErr(err))
	}
	if len(calls) == 0 && len(traceCalls) > 0 {
		calls = traceCalls
	}
	return calls, len(transcripts), errors.Join(captureQualityErr, traceErrIfNoTranscript(traceErr, len(transcripts)))
}

func traceErrIfNoTranscript(err error, transcriptCount int) error {
	if transcriptCount > 0 {
		return nil
	}
	return err
}

func summarizeLookups(attempts []lookupAttempt) []lookupSummary {
	all := make([]lookupAttempt, 0, len(attempts)*2)
	all = append(all, attempts...)
	for _, attempt := range attempts {
		attempt.Case = "all"
		all = append(all, attempt)
	}
	attempts = all
	var result []lookupSummary
	for _, a := range attempts {
		if len(result) == 0 || result[len(result)-1].Case != a.Case {
			result = append(result, lookupSummary{Case: a.Case, Routes: map[string]int{}})
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
		r.Routes[a.Score.Route]++
		switch a.Score.Verdict {
		case "success":
			r.Success++
		case "failed":
			r.Failed++
		default:
			r.Unscored++
		}
	}
	for i := range result {
		r := &result[i]
		var counts []int
		one, two := 0, 0
		for _, a := range attempts {
			if a.Case != r.Case || a.Score.Verdict != "success" || a.Score.CallsToHit == nil {
				continue
			}
			n := *a.Score.CallsToHit
			counts = append(counts, n)
			if n <= 1 {
				one++
			}
			if n <= 2 {
				two++
			}
		}
		r.OneCallPercent = 100 * float64(one) / float64(r.Scheduled)
		r.TwoCallPercent = 100 * float64(two) / float64(r.Scheduled)
		r.ScoredSuccesses = len(counts)
		if len(counts) > 0 {
			sort.Ints(counts)
			p50 := counts[(len(counts)-1)/2]
			p90 := counts[(len(counts)*9+9)/10-1]
			r.P50 = &p50
			r.P90 = &p90
		}
	}
	return result
}
