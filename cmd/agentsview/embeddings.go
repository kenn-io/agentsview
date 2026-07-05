// ABOUTME: `embeddings` command group — build, list, activate, and retire
// ABOUTME: semantic-search embedding generations, via the daemon or directly.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

// embeddingsPollInterval bounds how often the daemon build path polls
// /api/v1/embeddings/status. A package var so tests can shrink it.
var embeddingsPollInterval = 2 * time.Second

// directBuildProgressInterval bounds how often the direct build path polls
// the in-process Manager's Status for a progress line. A package var so
// tests can shrink it.
var directBuildProgressInterval = 2 * time.Second

// fingerprintDisplayLen is how many leading characters of a generation
// fingerprint `embeddings list` prints.
const fingerprintDisplayLen = 12

// embeddingsDaemonHTTPClient bounds each individual request the embeddings
// daemon client makes (build/status/list/activate/retire) so a wedged
// daemon cannot hang the CLI forever, matching the timeout other daemon
// HTTP clients in this codebase use (internal/service/http.go's
// httpBackend.client). This is a per-request timeout, not a deadline on the
// overall command: buildViaDaemon's poll loop issues one status call every
// embeddingsPollInterval, so a build that legitimately runs for longer than
// this timeout keeps polling fine — only an individual unresponsive call is
// cut off.
var embeddingsDaemonHTTPClient = &http.Client{Timeout: 30 * time.Second}

func newEmbeddingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "embeddings",
		Short:        "Manage the semantic search embedding index",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newEmbeddingsBuildCommand())
	cmd.AddCommand(newEmbeddingsListCommand())
	cmd.AddCommand(newEmbeddingsActivateCommand())
	cmd.AddCommand(newEmbeddingsRetireCommand())
	return cmd
}

// EmbeddingsBuildOptions holds the parsed `embeddings build` flags.
type EmbeddingsBuildOptions struct {
	FullRebuild bool
	Backstop    bool
	Yes         bool
	// IncludeAutomated is the --include-automated flag's parsed value; only
	// meaningful when IncludeAutomatedSet is true (see that field).
	IncludeAutomated bool
	// IncludeAutomatedSet reports whether --include-automated was
	// explicitly passed (cmd.Flags().Changed), overriding
	// [vector].include_automated to IncludeAutomated's parsed value (true or
	// false) for this one build.
	IncludeAutomatedSet bool
}

func newEmbeddingsBuildCommand() *cobra.Command {
	var opts EmbeddingsBuildOptions
	cmd := &cobra.Command{
		Use:          "build",
		Short:        "Build or refresh the embedding index",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.IncludeAutomatedSet = cmd.Flags().Changed("include-automated")
			return runEmbeddingsBuild(
				cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), opts,
			)
		},
	}
	cmd.Flags().BoolVar(&opts.FullRebuild, "full-rebuild", false,
		"Re-embed every document, even ones already embedded under the active generation")
	cmd.Flags().BoolVar(&opts.Backstop, "backstop", false,
		"Force a full mirror reconciliation scan without forcing a re-embed")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false,
		"Skip the full-rebuild confirmation prompt")
	cmd.Flags().BoolVar(&opts.IncludeAutomated, "include-automated", false,
		"Override [vector].include_automated for this build only: bare "+
			"--include-automated embeds automated (non-interactive) sessions "+
			"too, and --include-automated=false force-excludes them even if "+
			"the config default is true. Prefer setting the config key for "+
			"scheduled builds: mixing this flag with a different config "+
			"default flips the index's scope on every other build, forcing "+
			"a full mirror reconciliation each time.")
	return cmd
}

func newEmbeddingsListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List embedding generations",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEmbeddingsList(
				cmd.Context(), cmd.OutOrStdout(), outputFormat(cmd) == "json",
			)
		},
	}
	registerFormatFlags(cmd.Flags())
	return cmd
}

func newEmbeddingsActivateCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:          "activate <id>",
		Short:        "Activate an embedding generation",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseGenerationID(args[0])
			if err != nil {
				return err
			}
			return runEmbeddingsGenerationAction(
				cmd.Context(), cmd.OutOrStdout(), id, force, false,
			)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"Activate even if the generation has incomplete coverage")
	return cmd
}

func newEmbeddingsRetireCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:          "retire <id>",
		Short:        "Retire an embedding generation",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseGenerationID(args[0])
			if err != nil {
				return err
			}
			return runEmbeddingsGenerationAction(
				cmd.Context(), cmd.OutOrStdout(), id, force, true,
			)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"Retire even if the generation is currently active")
	return cmd
}

func parseGenerationID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid generation id %q: %w", raw, err)
	}
	return id, nil
}

// requireVectorEnabled rejects every embeddings subcommand up front when
// [vector] is not configured, with the exact message the brief specifies.
func requireVectorEnabled(cfg config.Config) error {
	if !cfg.Vector.Enabled {
		return errors.New(
			"vector search is not enabled: set [vector] enabled = true in config.toml",
		)
	}
	return nil
}

// vectorGeneration maps the configured embeddings model to the kit
// Generation identity Build/Fill fingerprint against. Defined once here so
// the daemon serve wiring (Task 16) can reuse the exact same mapping.
func vectorGeneration(c config.VectorEmbeddingsConfig) kitvec.Generation {
	return kitvec.Generation{
		Model:      c.Model,
		Dimensions: c.Dimension,
		Params:     map[string]string{"max_input_chars": strconv.Itoa(c.MaxInputChars)},
	}
}

// newVectorEncoder builds the OpenAI-compatible embeddings encoder from
// config, parsing the configured timeout duration.
func newVectorEncoder(c config.VectorEmbeddingsConfig) (kitvec.EncodeFunc, error) {
	timeout, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("parsing [vector.embeddings] timeout %q: %w", c.Timeout, err)
	}
	return vector.NewEncoder(vector.EncoderConfig{
		Endpoint:   c.Endpoint,
		APIKey:     c.APIKey(),
		Model:      c.Model,
		Dimension:  c.Dimension,
		Timeout:    timeout,
		MaxRetries: c.MaxRetries,
	}), nil
}

// runEmbeddingsBuild loads config, gates on [vector] enabled, confirms a
// requested full rebuild (unless --yes), and then dispatches to the daemon
// or direct build path depending on whether a writable local daemon owns
// the archive.
func runEmbeddingsBuild(
	ctx context.Context, out io.Writer, in io.Reader, opts EmbeddingsBuildOptions,
) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := requireVectorEnabled(cfg); err != nil {
		return err
	}

	includeAutomated := cfg.Vector.IncludeAutomated
	if opts.IncludeAutomatedSet {
		includeAutomated = opts.IncludeAutomated
	}

	if opts.FullRebuild && !opts.Yes {
		proceed, err := confirmFullRebuild(ctx, in, out, cfg, includeAutomated)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	req := vector.BuildRequest{
		FullRebuild:      opts.FullRebuild,
		Backstop:         opts.Backstop,
		IncludeAutomated: includeAutomated,
	}
	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		return runEmbeddingsBuildDaemon(ctx, out, cfg, req)
	}
	return runEmbeddingsBuildDirect(ctx, out, cfg, req)
}

// confirmFullRebuild prints and reads the "This re-embeds all N messages."
// confirmation prompt, where N is the current count of embeddable messages
// in the archive under includeAutomated's scope.
func confirmFullRebuild(
	ctx context.Context, in io.Reader, out io.Writer, cfg config.Config, includeAutomated bool,
) (bool, error) {
	n, err := countEmbeddableMessages(ctx, cfg, includeAutomated)
	if err != nil {
		return false, fmt.Errorf("counting messages: %w", err)
	}
	msg := fmt.Sprintf("This re-embeds all %d messages. Continue?", n)
	return confirm(in, out, msg), nil
}

// countEmbeddableMessages opens the archive database read-only and counts
// every message eligible for embedding under includeAutomated's scope, for
// the full-rebuild confirmation prompt.
func countEmbeddableMessages(ctx context.Context, cfg config.Config, includeAutomated bool) (int, error) {
	archiveDB, err := openReadOnlyDB(cfg)
	if err != nil {
		return 0, err
	}
	defer archiveDB.Close()

	var n int
	if _, err := archiveDB.ScanEmbeddableMessages(
		ctx, "", includeAutomated, func(db.EmbeddableMessage) error {
			n++
			return nil
		},
	); err != nil {
		return 0, err
	}
	return n, nil
}

// runEmbeddingsBuildDirect acquires the vectors write lock, opens the
// archive read-only (so it never competes with a daemon for the SQLite
// write lock) and vectors.db read-write, and runs one build synchronously.
func runEmbeddingsBuildDirect(
	ctx context.Context, out io.Writer, cfg config.Config, req vector.BuildRequest,
) error {
	lock, err := tryAcquireNamedLock(cfg.DataDir, vectorsWriteLockFile)
	if err != nil {
		return err
	}
	defer lock.Close()

	archiveDB, err := openReadOnlyDB(cfg)
	if err != nil {
		return fmt.Errorf("opening archive database: %w", err)
	}
	defer archiveDB.Close()

	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false, cfg.Vector.Embeddings.MaxInputChars,
	)
	if err != nil {
		return fmt.Errorf("opening vectors.db: %w", err)
	}
	defer ix.Close()

	enc, err := newVectorEncoder(cfg.Vector.Embeddings)
	if err != nil {
		return err
	}

	m := vector.NewManager(
		ix, archiveDB, enc, vectorGeneration(cfg.Vector.Embeddings), cfg.Vector.Embeddings.BatchSize,
	)
	return runDirectBuild(ctx, out, m, req)
}

// runDirectBuild runs m.TryBuild synchronously on a background goroutine
// while the caller's goroutine polls m.Status at directBuildProgressInterval
// to print progress lines, then prints the final summary once the build
// completes.
func runDirectBuild(
	ctx context.Context, out io.Writer, m *vector.Manager, req vector.BuildRequest,
) error {
	type outcome struct {
		started bool
		err     error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		started, err := m.TryBuild(ctx, req)
		resultCh <- outcome{started: started, err: err}
	}()

	ticker := time.NewTicker(directBuildProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case res := <-resultCh:
			if !res.started {
				return errors.New("a build is already running")
			}
			if res.err != nil {
				return res.err
			}
			if status := m.Status(); status.LastResult != nil {
				printBuildSummary(out, *status.LastResult)
			}
			return nil
		case <-ticker.C:
			if status := m.Status(); status.Running {
				printBuildProgress(out, status.Done, status.Total)
			}
		}
	}
}

// runEmbeddingsBuildDaemon resolves the local daemon and runs the build
// through its HTTP API.
func runEmbeddingsBuildDaemon(
	ctx context.Context, out io.Writer, cfg config.Config, req vector.BuildRequest,
) error {
	client, err := resolveEmbeddingsDaemonClient(cfg)
	if err != nil {
		return err
	}
	return buildViaDaemon(ctx, out, client, req)
}

// buildViaDaemon starts a build via POST /build, printing a status line
// instead of failing when one is already running (409), then polls
// /status until the build stops running.
func buildViaDaemon(
	ctx context.Context, out io.Writer, client embeddingsDaemonClient, req vector.BuildRequest,
) error {
	if err := client.startBuild(ctx, req); err != nil {
		var apiErr *daemonAPIError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusConflict {
			fmt.Fprintln(out, "a build is already running (daemon)")
		} else {
			return err
		}
	}
	return pollDaemonBuildStatus(ctx, out, client)
}

// pollDaemonBuildStatus polls the daemon's build status at
// embeddingsPollInterval, printing a progress line on every poll while the
// build is running, until it reports Running == false.
func pollDaemonBuildStatus(
	ctx context.Context, out io.Writer, client embeddingsDaemonClient,
) error {
	for {
		status, err := client.status(ctx)
		if err != nil {
			return err
		}
		if !status.Running {
			return finalizeBuildStatus(out, status)
		}
		printBuildProgress(out, status.Done, status.Total)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(embeddingsPollInterval):
		}
	}
}

// finalizeBuildStatus reports a stopped build's outcome: a non-empty
// LastError becomes the returned (non-zero-exit) error, otherwise the
// LastResult prints the same final summary the direct path prints.
func finalizeBuildStatus(out io.Writer, status vector.BuildStatus) error {
	if status.LastError != "" {
		return errors.New(status.LastError)
	}
	if status.LastResult != nil {
		printBuildSummary(out, *status.LastResult)
	}
	return nil
}

// printBuildProgress writes one progress line for either build path.
func printBuildProgress(w io.Writer, done, total int64) {
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	fmt.Fprintf(w, "progress: %d/%d chunks (%.1f%%)\n", done, total, pct)
}

// printBuildSummary writes the final build summary line (and, when the
// build auto-activated its generation, the activation line) for either
// build path.
func printBuildSummary(w io.Writer, result vector.BuildResult) {
	fmt.Fprintf(w, "Embedded %d documents (%d chunks), skipped %d, stale %d\n",
		result.Fill.Documents, result.Fill.Chunks, result.Fill.Skipped, result.Fill.Stale)
	if result.Activated {
		fmt.Fprintln(w, "Generation activated.")
	}
}

// runEmbeddingsList loads config, gates on [vector] enabled, lists every
// generation via the daemon or directly, and renders it as a table or JSON.
func runEmbeddingsList(ctx context.Context, out io.Writer, jsonOutput bool) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := requireVectorEnabled(cfg); err != nil {
		return err
	}

	var gens []vector.GenerationInfo
	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		client, err := resolveEmbeddingsDaemonClient(cfg)
		if err != nil {
			return err
		}
		gens, err = client.generations(ctx)
		if err != nil {
			return err
		}
	} else {
		gens, err = directListGenerations(ctx, cfg)
		if err != nil {
			return err
		}
	}

	if jsonOutput {
		if gens == nil {
			gens = []vector.GenerationInfo{}
		}
		return json.NewEncoder(out).Encode(struct {
			Generations []vector.GenerationInfo `json:"generations"`
		}{gens})
	}
	printGenerationsTable(out, gens)
	return nil
}

// directListGenerations opens vectors.db read-only and lists its
// generations, or returns an empty list without error when the index has
// never been built.
func directListGenerations(ctx context.Context, cfg config.Config) ([]vector.GenerationInfo, error) {
	path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ix, err := vector.Open(ctx, path, true, cfg.Vector.Embeddings.MaxInputChars)
	if err != nil {
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}
	defer ix.Close()
	return ix.Generations(ctx)
}

// printGenerationsTable renders gens as the `embeddings list` tabwriter
// table, truncating each fingerprint to fingerprintDisplayLen characters.
func printGenerationsTable(out io.Writer, gens []vector.GenerationInfo) {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tMODEL\tDIM\tEMBEDDED\tMISSING\tFINGERPRINT")
	for _, g := range gens {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%d\t%d\t%s\n",
			g.ID, g.State, g.Model, g.Dimension, g.Embedded, g.Missing,
			truncateFingerprint(g.Fingerprint))
	}
	_ = tw.Flush()
}

func truncateFingerprint(fp string) string {
	if len(fp) <= fingerprintDisplayLen {
		return fp
	}
	return fp[:fingerprintDisplayLen]
}

// runEmbeddingsGenerationAction implements both `activate <id>` and
// `retire <id>`: it loads config, gates on [vector] enabled, and dispatches
// the requested action to the daemon or directly. A 409/refusal error's
// message is returned verbatim (no extra wrapping) so it displays exactly
// as the manager phrased it.
func runEmbeddingsGenerationAction(
	ctx context.Context, out io.Writer, id int64, force, retire bool,
) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := requireVectorEnabled(cfg); err != nil {
		return err
	}

	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		client, err := resolveEmbeddingsDaemonClient(cfg)
		if err != nil {
			return err
		}
		if retire {
			err = client.retire(ctx, id, force)
		} else {
			err = client.activate(ctx, id, force)
		}
		if err != nil {
			return err
		}
	} else if err := directGenerationAction(ctx, cfg, id, force, retire); err != nil {
		return err
	}

	verb := "activated"
	if retire {
		verb = "retired"
	}
	fmt.Fprintf(out, "Generation %d %s.\n", id, verb)
	return nil
}

// directGenerationAction acquires the vectors write lock and applies the
// activate/retire state change directly against vectors.db.
func directGenerationAction(
	ctx context.Context, cfg config.Config, id int64, force, retire bool,
) error {
	lock, err := tryAcquireNamedLock(cfg.DataDir, vectorsWriteLockFile)
	if err != nil {
		return err
	}
	defer lock.Close()

	ix, err := vector.Open(
		ctx, cfg.Vector.ResolvedDBPath(cfg.DataDir), false, cfg.Vector.Embeddings.MaxInputChars,
	)
	if err != nil {
		return fmt.Errorf("opening vectors.db: %w", err)
	}
	defer ix.Close()

	m := vector.NewManager(
		ix, nil, nil, vectorGeneration(cfg.Vector.Embeddings), cfg.Vector.Embeddings.BatchSize,
	)
	if retire {
		return m.Retire(ctx, id, force)
	}
	return m.Activate(ctx, id, force)
}

// resolveEmbeddingsDaemonClient finds the active local daemon and builds an
// HTTP client for its embeddings API. Callers only reach it after
// IsLocalDaemonActive reported true.
func resolveEmbeddingsDaemonClient(cfg config.Config) (embeddingsDaemonClient, error) {
	rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken)
	if rt == nil {
		return embeddingsDaemonClient{}, errors.New("no reachable local agentsview daemon found")
	}
	return embeddingsDaemonClient{baseURL: urlFromDaemonRuntime(rt), token: cfg.AuthToken}, nil
}

// embeddingsDaemonClient is a small HTTP client for the Task 14 embeddings
// build lifecycle endpoints.
type embeddingsDaemonClient struct {
	baseURL string
	token   string
}

// daemonAPIError carries an embeddings API error response's HTTP status
// alongside its message, so callers can distinguish 409 (conflict/refusal)
// from other failures.
type daemonAPIError struct {
	status  int
	message string
}

func (e *daemonAPIError) Error() string { return e.message }

func (c embeddingsDaemonClient) startBuild(ctx context.Context, req vector.BuildRequest) error {
	return c.do(ctx, http.MethodPost, "/api/v1/embeddings/build", req, nil)
}

func (c embeddingsDaemonClient) status(ctx context.Context) (vector.BuildStatus, error) {
	var st vector.BuildStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/embeddings/status", nil, &st)
	return st, err
}

func (c embeddingsDaemonClient) generations(ctx context.Context) ([]vector.GenerationInfo, error) {
	var body struct {
		Generations []vector.GenerationInfo `json:"generations"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/embeddings/generations", nil, &body)
	return body.Generations, err
}

func (c embeddingsDaemonClient) activate(ctx context.Context, id int64, force bool) error {
	path := fmt.Sprintf("/api/v1/embeddings/generations/%d/activate", id)
	return c.do(ctx, http.MethodPost, path, map[string]bool{"force": force}, nil)
}

func (c embeddingsDaemonClient) retire(ctx context.Context, id int64, force bool) error {
	path := fmt.Sprintf("/api/v1/embeddings/generations/%d/retire", id)
	return c.do(ctx, http.MethodPost, path, map[string]bool{"force": force}, nil)
}

// do performs one HTTP call against the daemon's embeddings API,
// marshaling reqBody (when non-nil) as the request body and decoding the
// response into out (when non-nil). A non-2xx response becomes a
// *daemonAPIError carrying the status and the server's "error" message.
func (c embeddingsDaemonClient) do(
	ctx context.Context, method, path string, reqBody, out any,
) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(
		ctx, method, strings.TrimSuffix(c.baseURL, "/")+path, bodyReader,
	)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The daemon's CSRF guard rejects mutating requests whose Origin is not
	// in the allowlist. Setting Origin to the daemon's own baseURL satisfies
	// that check for the CLI, which has no real browser origin.
	req.Header.Set("Origin", c.baseURL)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := embeddingsDaemonHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return &daemonAPIError{status: resp.StatusCode, message: daemonErrorMessage(resp.StatusCode, body)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// daemonErrorMessage extracts the {"error": "..."} message huma's error
// responses carry, falling back to a generic "HTTP <status>: <body>" when
// the body isn't in that shape.
func daemonErrorMessage(status int, body []byte) string {
	var apiErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return apiErr.Error
	}
	return fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(body)))
}
