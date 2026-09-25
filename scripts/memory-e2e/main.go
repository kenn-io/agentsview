// Command memory-e2e runs opt-in live conversation-memory behavior checks.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
)

type clientName string

const (
	clientClaude clientName = "claude"
	clientCodex  clientName = "codex"
)

type config struct {
	live        bool
	clients     string
	runs        int
	caseName    string
	outputDir   string
	agentsview  string
	claudeModel string
	codexModel  string
	effort      string
	timeout     time.Duration
	repository  string
}

type behaviorCase struct {
	Name             string
	Prompt           string
	ExpectedPhrases  []string
	RequiresHistory  bool
	ForbidsHistory   bool
	RequiresCitation bool
}

type parsedTrace struct {
	SessionID        string   `json:"session_id,omitempty"`
	Model            string   `json:"model,omitempty"`
	FinalAnswer      string   `json:"final_answer,omitempty"`
	Tools            []string `json:"tools,omitempty"`
	InputTokens      int64    `json:"input_tokens,omitempty"`
	CacheReadTokens  int64    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64    `json:"cache_write_tokens,omitempty"`
	OutputTokens     int64    `json:"output_tokens,omitempty"`
	DurationMS       int64    `json:"duration_ms,omitempty"`
	CostUSD          float64  `json:"cost_usd,omitempty"`
}

type runResult struct {
	Case      string      `json:"case"`
	Attempt   int         `json:"attempt"`
	Passed    bool        `json:"passed"`
	Failures  []string    `json:"failures,omitempty"`
	TraceFile string      `json:"trace_file"`
	ErrorFile string      `json:"error_file"`
	Parsed    parsedTrace `json:"parsed"`
}

type clientResult struct {
	Client          clientName  `json:"client"`
	ClientVersion   string      `json:"client_version"`
	ConfiguredModel string      `json:"configured_model"`
	ReasoningEffort string      `json:"reasoning_effort"`
	SourceSessionID string      `json:"source_session_id"`
	Runs            []runResult `json:"runs"`
}

type verificationReport struct {
	SchemaVersion     int            `json:"schema_version"`
	StartedAt         time.Time      `json:"started_at"`
	FinishedAt        time.Time      `json:"finished_at"`
	Revision          string         `json:"revision"`
	AgentsViewVersion string         `json:"agentsview_version"`
	Passed            bool           `json:"passed"`
	Clients           []clientResult `json:"clients"`
}

var sourceRangePattern = regexp.MustCompile(
	`(?i)(?:ordinals?|ordinal\s+range|messages?)\s*:?\s*(\d+)\s*[-–]\s*(\d+)`,
)
var phraseSeparatorPattern = regexp.MustCompile(`[^\pL\pN]+`)

const (
	sourceFirstOrdinal = 0
	sourceLastOrdinal  = 3
)

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	repo, err := repositoryRoot()
	if err != nil {
		return config{}, err
	}
	cfg := config{repository: repo}
	fs := flag.NewFlagSet("memory-e2e", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&cfg.live, "live", false,
		"acknowledge that this runs authenticated Claude Code and Codex sessions")
	fs.StringVar(&cfg.clients, "client", "all", "client to run: claude, codex, or all")
	fs.IntVar(&cfg.runs, "runs", 3, "runs per behavioral case")
	fs.StringVar(&cfg.caseName, "case", "", "run one named case")
	fs.StringVar(&cfg.outputDir, "output", "", "artifact directory")
	fs.StringVar(&cfg.agentsview, "agentsview-bin", "", "prebuilt agentsview binary")
	fs.StringVar(&cfg.claudeModel, "claude-model", "claude-sonnet-5", "Claude Code model")
	fs.StringVar(&cfg.codexModel, "codex-model", "gpt-5.6-luna", "Codex model")
	fs.StringVar(&cfg.effort, "effort", "medium", "reasoning effort for both clients")
	fs.DurationVar(&cfg.timeout, "timeout", 5*time.Minute, "timeout per agent session")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if !cfg.live {
		return config{}, errors.New("memory-e2e: --live is required; authenticated agent runs may consume quota")
	}
	if cfg.runs < 1 {
		return config{}, errors.New("memory-e2e: --runs must be at least 1")
	}
	if cfg.timeout < time.Second {
		return config{}, errors.New("memory-e2e: --timeout must be at least 1s")
	}
	if _, err := selectedClients(cfg.clients); err != nil {
		return config{}, err
	}
	if cfg.outputDir == "" {
		cfg.outputDir = filepath.Join(repo,
			".test-data-memory-e2e-"+time.Now().UTC().Format("20060102T150405Z"))
	} else if !filepath.IsAbs(cfg.outputDir) {
		cfg.outputDir = filepath.Join(repo, cfg.outputDir)
	}
	return cfg, nil
}

func repositoryRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("memory-e2e: resolve repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func selectedClients(raw string) ([]clientName, error) {
	switch strings.TrimSpace(raw) {
	case "all":
		return []clientName{clientClaude, clientCodex}, nil
	case string(clientClaude):
		return []clientName{clientClaude}, nil
	case string(clientCodex):
		return []clientName{clientCodex}, nil
	default:
		return nil, fmt.Errorf("memory-e2e: unknown --client %q", raw)
	}
}

func behaviorCases() []behaviorCase {
	return []behaviorCase{
		{
			Name: "explicit-decision",
			Prompt: "Search prior conversation evidence for the synthetic memory-e2e project. " +
				"What cache namespace was chosen, and why was the alternative rejected? " +
				"Cite the full source session and ordinal range.",
			ExpectedPhrases: []string{"ember-lantern-731", "lock contention"},
			RequiresHistory: true, RequiresCitation: true,
		},
		{
			Name: "implicit-failure",
			Prompt: "Continue the existing concurrency plan for the synthetic memory-e2e worker. " +
				"State the already-chosen strategy and the prior failure it avoids. " +
				"No code changes are needed; proceed without asking follow-up questions.",
			ExpectedPhrases: []string{"violet-keel-884", "lease timeout"},
			RequiresHistory: true, RequiresCitation: true,
		},
		{
			Name: "later-correction",
			Prompt: "Which deployment marker should the synthetic memory-e2e release use? " +
				"Account for any correction and cite the full source session and ordinal range.",
			ExpectedPhrases: []string{"southstar-82"},
			RequiresHistory: true, RequiresCitation: true,
		},
		{
			Name: "current-context",
			Prompt: "The answer is already in this message: current-saffron-512. " +
				"Return that token exactly, with no extra context.",
			ExpectedPhrases: []string{"current-saffron-512"},
			ForbidsHistory:  true,
		},
	}
}

func sourcePrompt() string {
	return `This is a synthetic verification conversation for the memory-e2e project. Record these user-decided facts and acknowledge them without using tools:

1. The accepted cache namespace is ember-lantern-731. The rejected copper-delta-204 namespace caused cross-worker lock contention.
2. The accepted concurrency strategy is violet-keel-884 with at most two workers. The earlier quartz-harbor-219 strategy failed because lease timeouts caused duplicate work.
3. northstar-41 was only a proposal for the deployment marker. It was later corrected and explicitly accepted as southstar-82; northstar-41 is superseded.

Reply with a short acknowledgement of the three accepted decisions.`
}

func sourceFollowUpPrompt() string {
	return `Confirm the recorded decisions once more, including both rejected alternatives and their failure reasons. State clearly that southstar-82 supersedes northstar-41. Do not use tools.`
}

func run(ctx context.Context, cfg config) error {
	if err := os.MkdirAll(cfg.outputDir, 0o700); err != nil {
		return fmt.Errorf("memory-e2e: create output: %w", err)
	}
	bin, err := prepareAgentsView(ctx, cfg)
	if err != nil {
		return err
	}
	clients, _ := selectedClients(cfg.clients)
	cases, err := selectCases(cfg.caseName)
	if err != nil {
		return err
	}
	report := verificationReport{
		SchemaVersion:     1,
		StartedAt:         time.Now().UTC(),
		Revision:          repositoryRevision(ctx, cfg.repository),
		AgentsViewVersion: commandOutput(ctx, cfg.repository, bin, "--version"),
		Passed:            true,
	}
	for _, client := range clients {
		result, runErr := runClient(ctx, cfg, bin, client, cases)
		if runErr != nil {
			return runErr
		}
		report.Clients = append(report.Clients, result)
		for _, item := range result.Runs {
			report.Passed = report.Passed && item.Passed
		}
	}
	report.FinishedAt = time.Now().UTC()
	reportPath := filepath.Join(cfg.outputDir, "aggregate-result.json")
	if err := writeJSON(reportPath, report); err != nil {
		return err
	}
	fmt.Printf("memory-e2e report: %s\n", reportPath)
	if !report.Passed {
		return errors.New("memory-e2e: one or more behavioral runs failed")
	}
	return nil
}

func selectCases(name string) ([]behaviorCase, error) {
	cases := behaviorCases()
	if name == "" {
		return cases, nil
	}
	for _, testCase := range cases {
		if testCase.Name == name {
			return []behaviorCase{testCase}, nil
		}
	}
	return nil, fmt.Errorf("memory-e2e: unknown --case %q", name)
}

func prepareAgentsView(ctx context.Context, cfg config) (string, error) {
	if cfg.agentsview != "" {
		path, err := filepath.Abs(cfg.agentsview)
		if err != nil {
			return "", err
		}
		return path, nil
	}
	bin := filepath.Join(cfg.outputDir, "bin", "agentsview")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return "", err
	}
	ldflags := fmt.Sprintf("-X main.version=memory-e2e -X main.commit=%s -X main.buildDate=%s",
		repositoryRevision(ctx, cfg.repository), time.Now().UTC().Format(time.RFC3339))
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "fts5",
		"-ldflags", ldflags, "-trimpath", "-o", bin, "./cmd/agentsview")
	cmd.Dir = cfg.repository
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("memory-e2e: build agentsview: %w", err)
	}
	return bin, nil
}

func runClient(
	ctx context.Context, cfg config, bin string, client clientName,
	cases []behaviorCase,
) (clientResult, error) {
	clientDir := filepath.Join(cfg.outputDir, string(client))
	projectDir := filepath.Join(clientDir, "memory-e2e-project")
	dataDir := filepath.Join(clientDir, "agentsview-data")
	for _, dir := range []string{projectDir, dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return clientResult{}, err
		}
	}
	if err := initFixtureProject(ctx, projectDir); err != nil {
		return clientResult{}, err
	}
	version := commandOutput(ctx, projectDir, string(client), "--version")
	model := cfg.claudeModel
	if client == clientCodex {
		model = cfg.codexModel
	}
	sourceTrace, err := runSourceSession(ctx, cfg, client, projectDir, model)
	if err != nil {
		return clientResult{}, err
	}
	sourceTracePath := filepath.Join(clientDir, "source-session.jsonl")
	if err := os.WriteFile(sourceTracePath, sourceTrace, 0o600); err != nil {
		return clientResult{}, err
	}
	parsedSource, err := parseAgentTrace(client, sourceTrace)
	if err != nil {
		return clientResult{}, err
	}
	if parsedSource.SessionID == "" {
		return clientResult{}, fmt.Errorf("memory-e2e: %s source trace has no session id", client)
	}
	sourceFile, err := findClientSessionFile(client, parsedSource.SessionID)
	if err != nil {
		return clientResult{}, err
	}
	canonicalID, err := syncSourceSession(ctx, bin, dataDir, sourceFile)
	if err != nil {
		return clientResult{}, err
	}
	if client == clientCodex {
		if err := installCodexFixtureSkill(cfg.repository, projectDir); err != nil {
			return clientResult{}, err
		}
	}
	server, err := startFixtureServer(ctx, bin, dataDir, clientDir)
	if err != nil {
		return clientResult{}, err
	}
	defer server.stop()

	result := clientResult{
		Client: client, ClientVersion: version,
		ConfiguredModel: model, ReasoningEffort: cfg.effort,
		SourceSessionID: canonicalID,
	}
	for _, testCase := range cases {
		for attempt := 1; attempt <= cfg.runs; attempt++ {
			runDir := filepath.Join(clientDir, testCase.Name, fmt.Sprintf("run-%d", attempt))
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				return clientResult{}, err
			}
			trace, stderr, elapsed, runErr := runBehaviorSession(
				ctx, cfg, bin, client, projectDir, model, server.url, testCase.Prompt,
			)
			tracePath := filepath.Join(runDir, "trace.jsonl")
			errorPath := filepath.Join(runDir, "stderr.txt")
			if err := os.WriteFile(tracePath, trace, 0o600); err != nil {
				return clientResult{}, err
			}
			if err := os.WriteFile(errorPath, stderr, 0o600); err != nil {
				return clientResult{}, err
			}
			parsed, parseErr := parseAgentTrace(client, trace)
			parsed.DurationMS = elapsed.Milliseconds()
			failures := gradeBehaviorRun(testCase, parsed, canonicalID)
			if runErr != nil {
				failures = append(failures, "agent command failed: "+runErr.Error())
			}
			if parseErr != nil {
				failures = append(failures, "trace parse failed: "+parseErr.Error())
			}
			item := runResult{
				Case: testCase.Name, Attempt: attempt, Passed: len(failures) == 0,
				Failures:  failures,
				TraceFile: relativeArtifact(cfg.outputDir, tracePath),
				ErrorFile: relativeArtifact(cfg.outputDir, errorPath), Parsed: parsed,
			}
			result.Runs = append(result.Runs, item)
			fmt.Printf("%s %-20s run %d: %v\n", client, testCase.Name, attempt, item.Passed)
		}
	}
	return result, nil
}

func initFixtureProject(ctx context.Context, dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Synthetic memory verification project\n"), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("memory-e2e: git init: %w: %s", err, out)
	}
	return nil
}

func runSourceSession(
	ctx context.Context, cfg config, client clientName, projectDir, model string,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	var args []string
	if client == clientClaude {
		args = []string{
			"-p", "--safe-mode", "--model", model,
			"--effort", cfg.effort, "--output-format", "stream-json", "--verbose",
			"--session-id", newUUID(), sourcePrompt(),
		}
	} else {
		args = []string{
			"exec", "--json", "--ignore-user-config", "--model", model,
			"--enable", "skip_host_skill_discovery", "--disable", "plugins",
			"--disable", "recommended_plugins",
			"-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.effort), "--sandbox", "read-only",
			"--skip-git-repo-check", sourcePrompt(),
		}
	}
	stdout, stderr, err := executeAgent(ctx, projectDir, nil, string(client), args...)
	if err != nil {
		return stdout, fmt.Errorf("memory-e2e: %s source session: %w: %s", client, err, stderr)
	}
	parsed, err := parseAgentTrace(client, stdout)
	if err != nil {
		return stdout, fmt.Errorf("memory-e2e: parse %s source session id: %w", client, err)
	}
	if parsed.SessionID == "" {
		return stdout, fmt.Errorf("memory-e2e: %s source session has no id", client)
	}
	if client == clientClaude {
		args = []string{
			"-p", "--safe-mode", "--model", model,
			"--effort", cfg.effort, "--output-format", "stream-json", "--verbose",
			"--resume", parsed.SessionID, sourceFollowUpPrompt(),
		}
	} else {
		args = []string{
			"exec", "resume", "--json", "--ignore-user-config",
			"--enable", "skip_host_skill_discovery", "--disable", "plugins",
			"--disable", "recommended_plugins",
			"--model", model, "-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.effort),
			"-c", `sandbox_mode="read-only"`, "--skip-git-repo-check",
			parsed.SessionID, sourceFollowUpPrompt(),
		}
	}
	followUp, followUpStderr, err := executeAgent(
		ctx, projectDir, nil, string(client), args...,
	)
	if err != nil {
		return stdout, fmt.Errorf("memory-e2e: %s source follow-up: %w: %s",
			client, err, followUpStderr)
	}
	stdout = append(stdout, '\n')
	stdout = append(stdout, followUp...)
	return stdout, nil
}

func syncSourceSession(
	ctx context.Context, bin, dataDir, sourceFile string,
) (string, error) {
	env := []string{
		"AGENTSVIEW_DATA_DIR=" + dataDir,
		"AGENTSVIEW_NO_DAEMON=1",
		"DO_NOT_TRACK=1",
	}
	stdout, stderr, err := executeAgent(ctx, "", env, bin,
		"session", "sync", sourceFile, "--format", "json")
	if err != nil {
		return "", fmt.Errorf("memory-e2e: sync source transcript: %w: %s", err, stderr)
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout, &response); err != nil {
		return "", fmt.Errorf("memory-e2e: decode sync response: %w", err)
	}
	if response.ID == "" {
		return "", errors.New("memory-e2e: sync response has no canonical session id")
	}
	return response.ID, nil
}

func findClientSessionFile(client clientName, sessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var root string
	if client == clientCodex {
		root = strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		root = filepath.Join(root, "sessions")
	} else {
		root = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		root = filepath.Join(root, "projects")
	}
	var found string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") ||
			!strings.Contains(entry.Name(), sessionID) {
			return nil
		}
		found = path
		return filepath.SkipAll
	})
	if err != nil {
		return "", fmt.Errorf("memory-e2e: locate %s source transcript: %w", client, err)
	}
	if found == "" {
		return "", fmt.Errorf("memory-e2e: %s source transcript %s was not found", client, sessionID)
	}
	return found, nil
}

func installCodexFixtureSkill(repo, project string) error {
	source := filepath.Join(repo, "plugins", "agentsview-memory", "skills",
		"agentsview-finding-history", "SKILL.md")
	target := filepath.Join(project, ".agents", "skills",
		"agentsview-finding-history", "SKILL.md")
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, body, 0o600)
}

type fixtureServer struct {
	cmd *exec.Cmd
	url string
}

func startFixtureServer(
	ctx context.Context, bin, dataDir, clientDir string,
) (*fixtureServer, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	logFile, err := os.OpenFile(filepath.Join(clientDir, "server.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "serve", "--host", "127.0.0.1",
		"--port", strconv.Itoa(port), "--no-browser", "--no-sync", "--no-update-check")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_DATA_DIR="+dataDir, "DO_NOT_TRACK=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	server := &fixtureServer{cmd: cmd, url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	deadline := time.Now().Add(20 * time.Second)
	client, err := apiclient.NewHTTPClient(server.url, "", &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		server.stop()
		_ = logFile.Close()
		return nil, err
	}
	for time.Now().Before(deadline) {
		response, requestErr := client.GetAPIV1VersionWithResponse(ctx)
		if requestErr == nil {
			if response.StatusCode == http.StatusOK {
				_ = logFile.Close()
				return server, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	server.stop()
	_ = logFile.Close()
	return nil, errors.New("memory-e2e: fixture server did not become ready")
}

func (s *fixtureServer) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

func runBehaviorSession(
	ctx context.Context, cfg config, agentsviewBin string, client clientName,
	projectDir, model, serverURL, prompt string,
) ([]byte, []byte, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	path := filepath.Dir(agentsviewBin)
	env := []string{
		"PATH=" + path + string(os.PathListSeparator) + os.Getenv("PATH"),
		"AGENTSVIEW_MEMORY_MODE=hosted-reader",
		"AGENTSVIEW_MEMORY_SERVER=" + serverURL,
		"DO_NOT_TRACK=1",
	}
	var args []string
	if client == clientClaude {
		pluginRoot := filepath.Join(cfg.repository, "plugins", "agentsview-memory")
		mcpConfig, err := json.Marshal(map[string]any{
			"mcpServers": map[string]any{
				"agentsview": map[string]any{
					"command": agentsviewBin,
					"args": []string{
						"mcp", "--profile", "memory", "--server", serverURL,
					},
				},
			},
		})
		if err != nil {
			return nil, nil, 0, err
		}
		args = []string{
			"-p", "--model", model, "--effort", cfg.effort,
			"--output-format", "stream-json", "--verbose", "--forward-subagent-text",
			"--no-session-persistence", "--setting-sources", "project",
			"--strict-mcp-config", "--mcp-config", string(mcpConfig),
			"--permission-mode", "dontAsk", "--allowedTools",
			"Agent,mcp__agentsview__search_content,mcp__agentsview__get_messages",
			"--plugin-dir", pluginRoot, prompt,
		}
	} else {
		args = []string{
			"exec", "--json", "--ephemeral", "--ignore-user-config",
			"--enable", "skip_host_skill_discovery", "--disable", "plugins",
			"--disable", "recommended_plugins",
			"--model", model, "-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.effort),
			"-c", fmt.Sprintf(`mcp_servers.agentsview.command=%q`, agentsviewBin),
			"-c", fmt.Sprintf(
				`mcp_servers.agentsview.args=["mcp","--profile","memory","--server",%q]`,
				serverURL,
			),
			"--sandbox", "danger-full-access", "--skip-git-repo-check", prompt,
		}
	}
	started := time.Now()
	stdout, stderr, err := executeAgent(ctx, projectDir, env, string(client), args...)
	return stdout, stderr, time.Since(started), err
}

func executeAgent(
	ctx context.Context, dir string, extraEnv []string, name string, args ...string,
) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func parseAgentTrace(client clientName, trace []byte) (parsedTrace, error) {
	var out parsedTrace
	seenTools := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(trace))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return parsedTrace{}, fmt.Errorf("decode JSONL event: %w", err)
		}
		if client == clientClaude {
			parseClaudeEvent(event, &out, seenTools)
		} else {
			parseCodexEvent(event, &out, seenTools)
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedTrace{}, err
	}
	return out, nil
}

func parseClaudeEvent(event map[string]any, out *parsedTrace, seen map[string]bool) {
	typeName, _ := event["type"].(string)
	if typeName == "system" {
		if subtype, _ := event["subtype"].(string); subtype == "init" {
			out.Model, _ = event["model"].(string)
			out.SessionID, _ = event["session_id"].(string)
		}
	}
	if typeName == "assistant" {
		message, _ := event["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for _, raw := range content {
			item, _ := raw.(map[string]any)
			if itemType, _ := item["type"].(string); itemType == "tool_use" {
				name, _ := item["name"].(string)
				appendMemoryTool(out, seen, normalizeMemoryTool(name, ""))
			}
		}
	}
	if typeName != "result" {
		return
	}
	out.FinalAnswer, _ = event["result"].(string)
	out.DurationMS = numberAsInt64(event["duration_ms"])
	out.CostUSD = numberAsFloat64(event["total_cost_usd"])
	if usage, ok := event["usage"].(map[string]any); ok {
		out.InputTokens = numberAsInt64(usage["input_tokens"])
		out.OutputTokens = numberAsInt64(usage["output_tokens"])
	}
	if modelUsage, ok := event["modelUsage"].(map[string]any); ok {
		var input, cacheRead, cacheWrite, output int64
		for _, raw := range modelUsage {
			usage, _ := raw.(map[string]any)
			input += numberAsInt64(usage["inputTokens"])
			cacheRead += numberAsInt64(usage["cacheReadInputTokens"])
			cacheWrite += numberAsInt64(usage["cacheCreationInputTokens"])
			output += numberAsInt64(usage["outputTokens"])
		}
		out.InputTokens = input
		out.CacheReadTokens = cacheRead
		out.CacheWriteTokens = cacheWrite
		out.OutputTokens = output
	}
	if id, _ := event["session_id"].(string); id != "" {
		out.SessionID = id
	}
}

func parseCodexEvent(event map[string]any, out *parsedTrace, seen map[string]bool) {
	typeName, _ := event["type"].(string)
	if typeName == "thread.started" {
		out.SessionID, _ = event["thread_id"].(string)
	}
	if typeName == "item.completed" {
		item, _ := event["item"].(map[string]any)
		itemType, _ := item["type"].(string)
		switch itemType {
		case "mcp_tool_call":
			server, _ := item["server"].(string)
			tool, _ := item["tool"].(string)
			if tool == "" {
				tool, _ = item["name"].(string)
			}
			appendMemoryTool(out, seen, normalizeMemoryTool(tool, server))
		case "agent_message":
			out.FinalAnswer, _ = item["text"].(string)
		}
	}
	if typeName == "turn.completed" {
		if usage, ok := event["usage"].(map[string]any); ok {
			out.InputTokens = numberAsInt64(usage["input_tokens"])
			out.CacheReadTokens = numberAsInt64(usage["cached_input_tokens"])
			out.CacheWriteTokens = numberAsInt64(usage["cache_write_input_tokens"])
			out.OutputTokens = numberAsInt64(usage["output_tokens"])
		}
	}
}

func normalizeMemoryTool(name, server string) string {
	if server != "" && server != "agentsview" {
		return ""
	}
	for _, leaf := range []string{"search_content", "get_messages", "get_memory_status"} {
		if name == leaf && server == "agentsview" {
			return leaf
		}
		if strings.HasSuffix(name, "__"+leaf) && strings.Contains(name, "agentsview") {
			return leaf
		}
	}
	return ""
}

func appendMemoryTool(out *parsedTrace, seen map[string]bool, tool string) {
	if tool == "" || seen[tool] {
		return
	}
	seen[tool] = true
	out.Tools = append(out.Tools, tool)
}

func gradeBehaviorRun(
	testCase behaviorCase, parsed parsedTrace, sourceSessionID string,
) []string {
	var failures []string
	answer := strings.ToLower(parsed.FinalAnswer)
	normalizedAnswer := normalizePhrase(parsed.FinalAnswer)
	if strings.TrimSpace(answer) == "" {
		failures = append(failures, "missing final answer")
	}
	for _, phrase := range testCase.ExpectedPhrases {
		if !strings.Contains(normalizedAnswer, normalizePhrase(phrase)) {
			failures = append(failures, "missing expected phrase: "+phrase)
		}
	}
	searched := slices.Contains(parsed.Tools, "search_content")
	read := slices.Contains(parsed.Tools, "get_messages")
	if testCase.RequiresHistory {
		if !searched {
			failures = append(failures, "did not search conversation history")
		}
		if !read {
			failures = append(failures, "did not read source messages")
		}
	}
	if testCase.ForbidsHistory && (searched || read) {
		failures = append(failures, "searched history for a current-context answer")
	}
	if testCase.RequiresCitation {
		canonicalCitation := strings.ToLower(sourceSessionID)
		browserCitation := strings.ReplaceAll(canonicalCitation, ":", "/")
		if !strings.Contains(answer, canonicalCitation) &&
			!strings.Contains(answer, browserCitation) {
			failures = append(failures, "missing source session citation")
		}
		plainAnswer := strings.NewReplacer("*", "", "_", "", "`", "", "~", "").
			Replace(parsed.FinalAnswer)
		ranges := sourceRangePattern.FindAllStringSubmatch(plainAnswer, -1)
		if len(ranges) == 0 {
			failures = append(failures, "missing ordinal range citation")
		} else if !containsSourceRange(ranges) {
			failures = append(failures, "citation range does not cover source evidence")
		}
	}
	return failures
}

func containsSourceRange(ranges [][]string) bool {
	for _, match := range ranges {
		if len(match) != 3 {
			continue
		}
		first, firstErr := strconv.Atoi(match[1])
		last, lastErr := strconv.Atoi(match[2])
		if firstErr == nil && lastErr == nil &&
			first == sourceFirstOrdinal && last == sourceLastOrdinal {
			return true
		}
	}
	return false
}

func normalizePhrase(value string) string {
	return strings.Join(strings.Fields(
		phraseSeparatorPattern.ReplaceAllString(strings.ToLower(value), " "),
	), " ")
}

func numberAsInt64(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func numberAsFloat64(value any) float64 {
	if value, ok := value.(float64); ok {
		return value
	}
	return 0
}

func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[:8], hexID[8:12],
		hexID[12:16], hexID[16:20], hexID[20:])
}

func commandOutput(ctx context.Context, dir, name string, args ...string) string {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func repositoryRevision(ctx context.Context, repository string) string {
	head := commandOutput(ctx, repository, "git", "rev-parse", "HEAD")
	if head == "unknown" {
		return head
	}
	status := commandOutput(ctx, repository, "git", "status", "--porcelain")
	if status != "" && status != "unknown" {
		return head + "-dirty"
	}
	return head
}

func relativeArtifact(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

func writeJSON(path string, value any) error {
	body, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("memory-e2e: write report: %w", err)
	}
	return nil
}
