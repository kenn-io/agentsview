// ABOUTME: recall extract CLI (run/status/activate/retire/doctor/preview)
// ABOUTME: and the config-to-manager wiring shared with the daemon.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
)

// extractDistillation is the resolved, ready-to-run extraction setup derived
// from [recall.extract]: the configured client, prompts, and scheduling
// durations, plus the resolved server and profile names for display.
type extractDistillation struct {
	Client    *extract.Client
	Prompts   map[extract.PromptRole]string
	Segmenter extract.TurnsV1
	Identity  extract.ModelIdentity
	Quiet     time.Duration
	Backoff   time.Duration
	Backstop  time.Duration
	Server    string
	Profile   string
}

// resolveExtractDistillation validates cfg and resolves it into a runnable
// distillation setup.
func resolveExtractDistillation(
	cfg config.RecallExtractConfig,
) (extractDistillation, error) {
	var dist extractDistillation
	if !cfg.Enabled {
		return dist, fmt.Errorf(
			"recall extraction is not enabled; set enabled = true under " +
				"[recall.extract] and configure a model and server")
	}
	if err := cfg.Validate(); err != nil {
		return dist, err
	}
	serverName, server, err := cfg.ResolvedServer()
	if err != nil {
		return dist, err
	}
	timeout, err := time.ParseDuration(server.Timeout)
	if err != nil {
		return dist, fmt.Errorf(
			"parsing [recall.extract.servers.%s] timeout: %w", serverName, err)
	}
	profile, err := extract.ResolveProfile(cfg.Prompts.Profile, cfg.Model)
	if err != nil {
		return dist, err
	}
	var overrides map[extract.PromptRole]string
	if cfg.Prompts.Dir != "" {
		overrides, err = extract.LoadPromptOverrides(cfg.Prompts.Dir)
		if err != nil {
			return dist, err
		}
	}
	request := profile.Request
	if cfg.Request.Temperature != nil {
		request.Temperature = *cfg.Request.Temperature
	}
	if cfg.MaxTokens > 0 {
		request.MaxTokens = cfg.MaxTokens
	}
	if len(cfg.Request.ExtraBody) > 0 {
		request.ExtraBody = cfg.Request.ExtraBody
	}
	quiet, err := time.ParseDuration(cfg.QuietPeriod)
	if err != nil {
		return dist, fmt.Errorf("parsing quiet_period: %w", err)
	}
	backoff, err := time.ParseDuration(cfg.FailureBackoff)
	if err != nil {
		return dist, fmt.Errorf("parsing failure_backoff: %w", err)
	}
	backstop, err := time.ParseDuration(cfg.BackstopInterval)
	if err != nil {
		return dist, fmt.Errorf("parsing backstop_interval: %w", err)
	}
	httpClient := &http.Client{
		Timeout:       timeout,
		CheckRedirect: extract.RefuseRedirects,
	}
	return extractDistillation{
		Client: &extract.Client{
			BaseURL:    server.Endpoint,
			Model:      cfg.Model,
			HTTPClient: httpClient,
			Request:    request,
		},
		Prompts:   extract.PromptsFor(profile, overrides),
		Segmenter: extract.TurnsV1{MaxWindowChars: cfg.MaxWindowChars},
		Identity: extract.ModelIdentity{
			Model:      cfg.Model,
			Deployment: cfg.Deployment,
		},
		Quiet:    quiet,
		Backoff:  backoff,
		Backstop: backstop,
		Server:   serverName,
		Profile:  profile.Name,
	}, nil
}

// buildExtractManager resolves cfg into an extraction Manager over database.
func buildExtractManager(
	cfg config.RecallExtractConfig, database *db.DB,
) (*extract.Manager, error) {
	dist, err := resolveExtractDistillation(cfg)
	if err != nil {
		return nil, err
	}
	return extract.NewManager(extract.ManagerConfig{
		DB:             database,
		Client:         dist.Client,
		Segmenter:      dist.Segmenter,
		Prompts:        dist.Prompts,
		Identity:       dist.Identity,
		QuietPeriod:    dist.Quiet,
		FailureBackoff: dist.Backoff,
	})
}

// setupRecallExtraction wires the daemon's extraction scheduler. When the
// section is disabled it returns a reconcile-only scheduler if a generation
// exists, and nil when none does.
func setupRecallExtraction(
	cfg config.Config, database *db.DB, idle *server.IdleTracker,
) (*extractScheduler, error) {
	if !cfg.Recall.Extract.Enabled {
		return setupExtractReconcileOnly(database, idle)
	}
	dist, err := resolveExtractDistillation(cfg.Recall.Extract)
	if err != nil {
		return nil, err
	}
	mgr, err := buildExtractManager(cfg.Recall.Extract, database)
	if err != nil {
		return nil, err
	}
	backstop := max(dist.Backstop, 0)
	// With the backstop disabled, catchup ticks (paced by the quiet period,
	// never faster than once a minute) keep scanning so a session whose
	// quiet period elapses after the last sync still gets extracted.
	catchup := max(dist.Quiet, time.Minute)
	return newExtractScheduler(
		mgr, extractDebounceInterval, backstop, catchup, idle,
	), nil
}

// setupExtractReconcileOnly wires a scheduler that only retracts the
// generated corpus of sessions that lost eligibility, for the case where
// extraction is disabled but a generation activated while it was enabled is
// still serving. Retraction must not depend on the model-backed loop being
// enabled: a session trashed, flagged automated, or found to carry secrets
// must stop serving its entries regardless. Nil when no generation exists —
// extraction was never run, so there is nothing to retract and a
// default-disabled daemon starts no scheduler.
func setupExtractReconcileOnly(
	database *db.DB, idle *server.IdleTracker,
) (*extractScheduler, error) {
	generations, err := database.ExtractGenerations(context.Background())
	if err != nil {
		return nil, fmt.Errorf("checking for extraction generations: %w", err)
	}
	if len(generations) == 0 {
		return nil, nil
	}
	// No backstop: there is no corpus to top up. Session-mutation
	// notifications drive prompt retraction; the catchup ticker is a cheap
	// periodic backstop for anything a missed notification left behind. A
	// short debounce — retraction is cheap and carries no model latency,
	// unlike extraction's sync-batching debounce — keeps the startup pass
	// and post-trash retraction prompt.
	return newExtractScheduler(
		extract.NewReconciler(database),
		extractReconcileDebounce, 0, time.Minute, idle,
	), nil
}

// extractReconcileDebounce is the reconcile-only scheduler's debounce: short,
// so entries of a just-trashed session stop serving promptly and the startup
// pass retracts soon after launch.
const extractReconcileDebounce = 2 * time.Second

// openWritableExtractDB opens the archive read-write for manual extraction
// commands, refusing while a daemon owns it: there is no extraction HTTP
// seam yet, and a daemon with [recall.extract] enabled runs passes itself.
// The returned lock is the offline writer lock; the caller holds it for the
// command's lifetime so a multi-step extraction pass cannot overlap another
// direct writer or a resync swapping the database underneath it.
func openWritableExtractDB(
	ctx context.Context, cfg config.Config,
) (*db.DB, *writeOwnerLock, error) {
	tr, err := detectTransport(cfg.DataDir, cfg.AuthToken, 0)
	if err != nil {
		return nil, nil, err
	}
	if tr.Mode == transportHTTP {
		return nil, nil, fmt.Errorf(
			"a local daemon is running and owns the archive; a daemon with " +
				"[recall.extract] enabled runs extraction passes itself — " +
				"stop it to run extraction manually")
	}
	if tr.Mode == transportDirect && tr.DirectReadOnly {
		reason := tr.DirectReason
		if reason == "" {
			reason = "the archive is not writable right now"
		}
		return nil, nil, fmt.Errorf("%s; refusing to run extraction", reason)
	}
	return openWriteDB(ctx, cfg)
}

func loadExtractConfig(cmd *cobra.Command) (config.Config, error) {
	// These commands operate on the local archive only; silently reading
	// local state while the user targets a remote daemon would be worse
	// than refusing.
	if remote, _ := cmd.Flags().GetString("server"); strings.TrimSpace(remote) != "" {
		return config.Config{}, fmt.Errorf(
			"recall extract %s does not support --server: extraction runs "+
				"against the local archive", cmd.Name())
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

func newRecallExtractCommand() *cobra.Command {
	var sessionID string
	var dryRun bool
	var chunkMaxChars int
	cmd := &cobra.Command{
		Use:          "extract",
		Short:        "Extract recall entries from session transcripts",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pre-subcommand invocations used `extract --session <id>
			// --dry-run`; keep them working as a silent fallback for the
			// preview subcommand.
			if dryRun || strings.TrimSpace(sessionID) != "" {
				return runRecallExtractPreview(cmd, sessionID, chunkMaxChars)
			}
			return cmd.Help()
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "",
		"Deprecated: use 'extract preview --session'")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Deprecated: use 'extract preview'")
	cmd.Flags().IntVar(&chunkMaxChars, "chunk-max-chars", 0,
		"Deprecated: use 'extract preview --chunk-max-chars'")
	cmd.AddCommand(
		newRecallExtractRunCommand(),
		newRecallExtractStatusCommand(),
		newRecallExtractActivateCommand(),
		newRecallExtractRetireCommand(),
		newRecallExtractDoctorCommand(),
		newRecallExtractPreviewCommand(),
	)
	return cmd
}

func newRecallExtractRunCommand() *cobra.Command {
	var sessionID string
	var full bool
	var limit int
	cmd := &cobra.Command{
		Use:          "run",
		Short:        "Run one extraction pass over eligible sessions",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				// A negative limit would reach the DB's "<= 0 means all"
				// rule and scan the entire eligible archive — a surprise
				// burst of model usage. Zero is the documented unlimited
				// value; negatives are a mistake.
				return fmt.Errorf(
					"--limit must not be negative (0 means all): got %d",
					limit)
			}
			cfg, err := loadExtractConfig(cmd)
			if err != nil {
				return err
			}
			database, lock, err := openWritableExtractDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			defer database.Close()
			mgr, err := buildExtractManager(cfg.Recall.Extract, database)
			if err != nil {
				return err
			}
			result, err := mgr.RunPass(cmd.Context(), extract.PassOptions{
				SessionID: strings.TrimSpace(sessionID),
				Full:      full,
				Limit:     limit,
			})
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(
					extractRunResult{
						Sessions:  result.Sessions,
						Failed:    result.Failed,
						Units:     result.Units,
						Entries:   result.Entries,
						Activated: result.Activated,
					})
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Sessions: %d done, %d failed\nUnits: %d\nEntries: %d new\n",
				result.Sessions, result.Failed, result.Units, result.Entries)
			if result.Activated {
				fmt.Fprintln(cmd.OutOrStdout(), "Generation activated")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "",
		"Extract a single session (bypasses the quiet period)")
	cmd.Flags().BoolVar(&full, "full", false,
		"Revisit completed sessions to top up grown transcripts")
	cmd.Flags().IntVar(&limit, "limit", 0,
		"Maximum sessions to process this pass (0 = all)")
	return cmd
}

type extractRunResult struct {
	Sessions  int  `json:"sessions"`
	Failed    int  `json:"failed"`
	Units     int  `json:"units"`
	Entries   int  `json:"entries"`
	Activated bool `json:"activated"`
}

func newRecallExtractStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show extraction coverage for the configured generation",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadExtractConfig(cmd)
			if err != nil {
				return err
			}
			applyClassifierConfig(cfg)
			database, err := db.OpenReadOnly(cfg.DBPath)
			if err != nil {
				return err
			}
			defer database.Close()
			mgr, err := buildExtractManager(cfg.Recall.Extract, database)
			if err != nil {
				return err
			}
			status, err := mgr.Status(cmd.Context())
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
			}
			printExtractStatusHuman(cmd.OutOrStdout(), status)
			return nil
		},
	}
	return cmd
}

func printExtractStatusHuman(w io.Writer, status extract.Status) {
	fmt.Fprintf(w, "Fingerprint: %s\n", status.Fingerprint)
	sessions := status.Stats.Pending + status.Stats.Partial +
		status.Stats.Done + status.Stats.Failed
	fmt.Fprintf(w, "Sessions: %d done, %d partial, %d pending, %d failed "+
		"(%d tracked, %d eligible not started)\n",
		status.Stats.Done, status.Stats.Partial, status.Stats.Pending,
		status.Stats.Failed, sessions, status.EligibleBacklog)
	coverage := 0.0
	if status.Stats.UnitsTotal > 0 {
		coverage = 100 * float64(status.Stats.UnitsDone) /
			float64(status.Stats.UnitsTotal)
	}
	fmt.Fprintf(w, "Units: %d/%d (%.1f%%)\n",
		status.Stats.UnitsDone, status.Stats.UnitsTotal, coverage)
	fmt.Fprintf(w, "Entries: %d\n", status.Stats.Entries)
	for _, gen := range status.Generations {
		marker := " "
		if gen.Fingerprint == status.Fingerprint {
			marker = "*"
		}
		fmt.Fprintf(w, "%s %s  %s  %s (%s)\n",
			marker, gen.Fingerprint, gen.State, gen.Model, gen.Segmenter)
	}
}

func newRecallExtractActivateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "activate",
		Short:        "Activate the configured generation for serving",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadExtractConfig(cmd)
			if err != nil {
				return err
			}
			database, lock, err := openWritableExtractDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			defer database.Close()
			mgr, err := buildExtractManager(cfg.Recall.Extract, database)
			if err != nil {
				return err
			}
			if err := mgr.Activate(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Activated generation %s\n", mgr.Fingerprint())
			return nil
		},
	}
	return cmd
}

func newRecallExtractRetireCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:          "retire <fingerprint>",
		Short:        "Retire an extraction generation",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadExtractConfig(cmd)
			if err != nil {
				return err
			}
			database, lock, err := openWritableExtractDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			defer database.Close()
			if err := database.RetireExtractGeneration(
				cmd.Context(), args[0], force,
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Retired generation %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"Retire even if the generation is currently active")
	return cmd
}

// extractDoctorProbeText is a tiny transcript-like input for the doctor's
// end-to-end probe: it exercises the full request shape (prompt, schema,
// extra body) without meaningful cost.
const extractDoctorProbeText = "USER MESSAGE (ordinal 0):\n" +
	"Use port 8080 for the local server from now on."

// extractProbeTimeout derives the doctor probe's context deadline from the
// configured per-request timeout, so slow-model configurations that work
// during normal extraction also pass diagnostics. The headroom keeps the
// request timeout as the bound that fires: its error names the setting the
// operator configured, where a context cancellation would not.
func extractProbeTimeout(requestTimeout time.Duration) time.Duration {
	return requestTimeout + 30*time.Second
}

func newRecallExtractDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Check extraction configuration with one probe call",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadExtractConfig(cmd)
			if err != nil {
				return err
			}
			dist, err := resolveExtractDistillation(cfg.Recall.Extract)
			if err != nil {
				return err
			}
			fingerprint, err := extract.Fingerprint(
				dist.Identity, dist.Segmenter, dist.Prompts,
				dist.Client.Request,
			)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Model: %s\n", dist.Identity.Model)
			if dist.Identity.Deployment != "" {
				fmt.Fprintf(out, "Deployment: %s\n", dist.Identity.Deployment)
			}
			fmt.Fprintf(out, "Server: %s (%s)\n",
				dist.Server, config.RedactedEndpoint(dist.Client.BaseURL))
			fmt.Fprintf(out, "Profile: %s\n", dist.Profile)
			fmt.Fprintf(out, "Segmenter: %s (max_window_chars=%d)\n",
				dist.Segmenter.Name(), dist.Segmenter.MaxWindowChars)
			fmt.Fprintf(out, "Fingerprint: %s\n", fingerprint)

			ctx, cancel := context.WithTimeout(cmd.Context(),
				extractProbeTimeout(dist.Client.HTTPClient.Timeout))
			defer cancel()
			entries, usage, err := dist.Client.DistillWithRecovery(
				ctx, dist.Prompts[extract.RoleIntent],
				extractDoctorProbeText, 1,
			)
			if err != nil {
				// The error can embed the endpoint's raw response body;
				// a malicious server must not reach the terminal with
				// escape sequences intact.
				return fmt.Errorf("probe: %s", sanitizeTerminal(err.Error()))
			}
			fmt.Fprintf(out,
				"probe: ok (%d entries, %d prompt + %d completion tokens)\n",
				len(entries), usage.PromptTokens, usage.CompletionTokens)
			return nil
		},
	}
	return cmd
}

func newRecallExtractPreviewCommand() *cobra.Command {
	var sessionID string
	var chunkMaxChars int
	cmd := &cobra.Command{
		Use:          "preview --session <id>",
		Short:        "Preview session chunks without calling the model",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecallExtractPreview(cmd, sessionID, chunkMaxChars)
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "Session id to analyze")
	cmd.Flags().IntVar(&chunkMaxChars, "chunk-max-chars", 0,
		"Maximum characters per analysis chunk")
	return cmd
}

func runRecallExtractPreview(
	cmd *cobra.Command, sessionID string, chunkMaxChars int,
) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("recall extract preview requires --session")
	}
	svc, cleanup, err := resolveRecallEntryService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	messages, err := loadRecallExtractionMessages(
		cmd.Context(), svc, sessionID,
	)
	if err != nil {
		return err
	}
	chunks := service.BuildRecallExtractionChunks(
		sessionID, messages,
		service.RecallExtractionChunkOptions{MaxChars: chunkMaxChars},
	)
	result := recallExtractDryRunResult{
		SessionID:     sessionID,
		DryRun:        true,
		MessageCount:  len(messages),
		ChunkCount:    len(chunks),
		ChunkMaxChars: chunkMaxChars,
		Chunks:        chunks,
	}
	if outputFormat(cmd) == "json" {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}
	return printRecallExtractDryRunHuman(cmd.OutOrStdout(), result)
}
