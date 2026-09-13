package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	pgMigrationParityPollInterval = 250 * time.Millisecond
	pgMigrationParitySQLTimeout   = 10 * time.Second
)

type pgMigrationParityOptions struct {
	RuntimeTarget     string
	BaselineTarget    string
	RunID             string
	Device            string
	Provider          parser.AgentType
	Root              string
	BatchSize         int
	Wait              time.Duration
	Status            bool
	RequestGeneration int64
}

type pgMigrationParityRunner func(context.Context, io.Writer, pgMigrationParityOptions, string) error

func newPGMigrationParityCommand() *cobra.Command {
	return newPGMigrationParityCommandWithRunner(runPGMigrationParity)
}

func newPGMigrationParityCommandWithRunner(run pgMigrationParityRunner) *cobra.Command {
	var options pgMigrationParityOptions
	var provider string
	cmd := &cobra.Command{
		Use:          "parity",
		Short:        "Request or read bounded hosted migration parity evidence",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.Provider = parser.AgentType(provider)
			if err := validatePGMigrationParityOptions(cmd, options); err != nil {
				return err
			}
			return run(cmd.Context(), cmd.OutOrStdout(), options, outputFormat(cmd))
		},
	}
	cmd.Flags().StringVar(&options.RuntimeTarget, "runtime-target", "", "Existing named PostgreSQL runtime target")
	cmd.Flags().StringVar(&options.BaselineTarget, "baseline-target", "", "Owner-provisioned baseline profile")
	cmd.Flags().StringVar(&options.RunID, "run-id", "", "Canonical UUID for this immutable parity run")
	cmd.Flags().StringVar(&options.Device, "device", "", "Authenticated device ID")
	cmd.Flags().StringVar(&provider, "provider", "", "Raw-sync provider")
	cmd.Flags().StringVar(&options.Root, "root", "", "Configured root ID")
	cmd.Flags().IntVar(&options.BatchSize, "batch-size", 32, "Maximum sources processed by this request (1..128)")
	cmd.Flags().DurationVar(&options.Wait, "wait", 0, "Wait up to 60s for this request")
	cmd.Flags().BoolVar(&options.Status, "status", false, "Read previously recorded evidence")
	cmd.Flags().Int64Var(&options.RequestGeneration, "request-generation", 0, "Consume one completed observation-bound request result")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func validatePGMigrationParityOptions(cmd *cobra.Command, options pgMigrationParityOptions) error {
	if options.RuntimeTarget == "" {
		return errors.New("--runtime-target is required")
	}
	if options.RuntimeTarget != strings.TrimSpace(options.RuntimeTarget) || strings.Contains(options.RuntimeTarget, ",") {
		return errors.New("--runtime-target must name one configured target")
	}
	if options.RunID == "" {
		return errors.New("--run-id is required")
	}
	runID, err := uuid.Parse(options.RunID)
	if err != nil || runID.String() != options.RunID {
		return errors.New("--run-id must be a canonical lowercase UUID")
	}
	if options.Status {
		for _, name := range []string{"baseline-target", "device", "provider", "root", "batch-size", "wait"} {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("--%s cannot be used with --status", name)
			}
		}
		if cmd.Flags().Changed("request-generation") && options.RequestGeneration <= 0 {
			return errors.New("--request-generation must be a positive integer")
		}
		return nil
	}
	if cmd.Flags().Changed("request-generation") {
		return errors.New("--request-generation requires --status")
	}
	if options.BaselineTarget == "" {
		return errors.New("--baseline-target is required")
	}
	if options.BaselineTarget != strings.TrimSpace(options.BaselineTarget) || strings.Contains(options.BaselineTarget, ",") {
		return errors.New("--baseline-target must name one configured profile")
	}
	if options.Device == "" {
		return errors.New("--device is required")
	}
	if _, err := rawsync.NewAuthIdentity("parity-validation", options.Device); err != nil {
		return errors.New("--device is not a safe opaque identifier")
	}
	if options.Provider == "" {
		return errors.New("--provider is required")
	}
	if _, err := rawsync.NewAuthIdentity("parity-validation", string(options.Provider)); err != nil {
		return errors.New("--provider must name a raw-sync provider")
	}
	provider, ok := parser.AgentByType(options.Provider)
	if !ok || provider.RemoteSyncExcluded {
		return errors.New("--provider must name a raw-sync provider")
	}
	if options.Root == "" {
		return errors.New("--root is required")
	}
	if _, err := rawsync.NewAuthIdentity("parity-validation", options.Root); err != nil {
		return errors.New("--root is not a safe opaque identifier")
	}
	if options.BatchSize < 1 || options.BatchSize > 128 {
		return errors.New("--batch-size must be between 1 and 128")
	}
	if options.Wait < 0 || options.Wait > 60*time.Second {
		return errors.New("--wait must be between 0s and 60s")
	}
	return nil
}

type pgMigrationParityStore interface {
	CreateOrResumeParity(context.Context, rawderive.ParityRequest, int) (rawderive.ParityReport, error)
	ReadParityReport(context.Context, string) (rawderive.ParityReport, error)
	ReadParityRequestReport(context.Context, string, int64) (rawderive.ParityReport, error)
}

type pgMigrationParityDeps struct {
	loadConfig   func() (config.Config, error)
	openRuntime  func(context.Context, config.PGConfig) (pgMigrationParityStore, string, func(), error)
	pollInterval time.Duration
}

func defaultPGMigrationParityDeps() pgMigrationParityDeps {
	return pgMigrationParityDeps{
		loadConfig:   config.LoadMinimal,
		openRuntime:  openPGMigrationParityRuntime,
		pollInterval: pgMigrationParityPollInterval,
	}
}

func runPGMigrationParity(ctx context.Context, out io.Writer, options pgMigrationParityOptions, format string) error {
	return runPGMigrationParityWithDeps(ctx, out, options, format, defaultPGMigrationParityDeps())
}

func runPGMigrationParityWithDeps(ctx context.Context, out io.Writer, options pgMigrationParityOptions, format string, deps pgMigrationParityDeps) error {
	app, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	runtime, err := resolvePGMigrationParityRuntime(&app, options.RuntimeTarget)
	if err != nil {
		return err
	}
	if err = runtime.ValidateParity(app.RequireAuth); err != nil {
		return err
	}
	if !options.Status {
		if _, _, err = app.ResolveParityBaseline(runtime, options.BaselineTarget); err != nil {
			return err
		}
	}

	openCtx, cancel := context.WithTimeout(ctx, pgMigrationParitySQLTimeout)
	store, runtimeID, closeRuntime, err := deps.openRuntime(openCtx, runtime)
	cancel()
	if err != nil {
		return pgMigrationParityBoundaryError("opening selected migration parity runtime target", err)
	}
	defer closeRuntime()

	if options.Status {
		return runPGMigrationParityStatus(ctx, out, store, options, format)
	}
	request := rawderive.ParityRequest{
		RunID:           options.RunID,
		RuntimeID:       runtimeID,
		BaselineProfile: options.BaselineTarget,
		Cohort: rawderive.ParityCohort{
			DeviceID: options.Device,
			Provider: options.Provider,
			RootID:   options.Root,
		},
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, pgMigrationParitySQLTimeout)
	report, err := store.CreateOrResumeParity(requestCtx, request, options.BatchSize)
	requestCancel()
	if err != nil {
		return pgMigrationParityBoundaryError("requesting migration parity evidence", err)
	}
	report.Passing = false
	if options.Wait == 0 {
		return formatParityReport(out, format, report)
	}
	return pollPGMigrationParity(ctx, out, store, report, options.Wait, format, deps.pollInterval)
}

func resolvePGMigrationParityRuntime(app *config.Config, name string) (config.PGConfig, error) {
	if len(app.PGTargets) == 0 {
		return config.PGConfig{}, errors.New("migration parity requires an existing named PostgreSQL runtime target")
	}
	if _, err := app.RawPGTarget(name); err != nil {
		return config.PGConfig{}, err
	}
	return app.ResolvePGTarget(name)
}

func openPGMigrationParityRuntime(ctx context.Context, runtime config.PGConfig) (pgMigrationParityStore, string, func(), error) {
	database, err := postgres.OpenHostedContextWithInsecureWarning(ctx, runtime.URL, runtime.Schema, runtime.RawTenant, runtime.AllowInsecure, func() {
		log.Print("warning: migration parity runtime PostgreSQL connection permits plaintext")
	})
	if err != nil {
		return nil, "", nil, err
	}
	closeRuntime := func() { _ = database.Close() }
	var runtimeID string
	if err = database.QueryRowContext(ctx, `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&runtimeID); err != nil {
		closeRuntime()
		return nil, "", nil, err
	}
	id, idErr := uuid.Parse(runtimeID)
	if idErr != nil || id.String() != runtimeID {
		closeRuntime()
		return nil, "", nil, errors.New("invalid migration parity runtime identity")
	}
	store, err := postgres.NewMigrationParityStore(ctx, database, postgres.MigrationParityOptions{Schema: runtime.Schema, Tenant: runtime.RawTenant})
	if err != nil {
		closeRuntime()
		return nil, "", nil, err
	}
	return store, runtimeID, closeRuntime, nil
}

func runPGMigrationParityStatus(ctx context.Context, out io.Writer, store pgMigrationParityStore, options pgMigrationParityOptions, format string) error {
	readCtx, cancel := context.WithTimeout(ctx, pgMigrationParitySQLTimeout)
	defer cancel()
	var report rawderive.ParityReport
	var err error
	if options.RequestGeneration > 0 {
		report, err = store.ReadParityRequestReport(readCtx, options.RunID, options.RequestGeneration)
		if report.Code == "generation_not_available" || report.Freshness == "stale" {
			report.Passing = false
		} else {
			report.Passing = report.Passing && parityReportCanPass(report, options.RequestGeneration)
		}
	} else {
		report, err = store.ReadParityReport(readCtx, options.RunID)
		report.Passing = false
		if report.State == "complete" {
			report.Code = "historical_evidence"
		}
	}
	if err != nil {
		return pgMigrationParityBoundaryError("reading migration parity evidence", err)
	}
	return formatParityReport(out, format, report)
}

func pollPGMigrationParity(ctx context.Context, out io.Writer, store pgMigrationParityStore, initial rawderive.ParityReport, wait time.Duration, format string, interval time.Duration) error {
	if interval <= 0 {
		interval = pgMigrationParityPollInterval
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	report := initial
	generation := initial.RequestGeneration
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return pgMigrationParityBoundaryError("waiting for migration parity evidence", ctx.Err())
			}
			return formatParityReport(out, format, report)
		case <-timer.C:
			readCtx, readCancel := context.WithTimeout(waitCtx, pgMigrationParitySQLTimeout)
			observed, err := store.ReadParityRequestReport(readCtx, initial.RunID, generation)
			readCancel()
			if err != nil {
				if waitCtx.Err() != nil && ctx.Err() == nil {
					return formatParityReport(out, format, report)
				}
				return pgMigrationParityBoundaryError("reading migration parity evidence", err)
			}
			report = observed
			report.Passing = false
			if report.Code == "generation_not_available" {
				return formatParityReport(out, format, report)
			}
			if report.State == "complete" && report.CompletedGeneration == generation {
				if report.Freshness == "historical_checked" {
					report.Freshness = "checked"
				}
				report.Passing = parityReportCanPass(report, generation)
				return formatParityReport(out, format, report)
			}
			timer.Reset(interval)
		}
	}
}

func parityReportCanPass(report rawderive.ParityReport, generation int64) bool {
	memberTotal := report.Members.Matched + report.Members.Mismatched + report.Members.Ambiguous + report.Members.LegacyOnly + report.Members.Missing + report.Members.PartialUnsupported + report.Members.Stale
	badMembers := memberTotal - report.Members.Matched
	badSources := report.Sources.Mismatched + report.Sources.Ambiguous + report.Sources.LegacyOnly + report.Sources.Missing + report.Sources.PartialUnsupported + report.Sources.Stale
	return report.State == "complete" && report.CompletedGeneration == generation && report.BaselineSealed && report.Complete && report.PendingSources == 0 && memberTotal > 0 && badMembers == 0 && badSources == 0 && (report.Freshness == "checked" || report.Freshness == "historical_checked")
}

func pgMigrationParityBoundaryError(action string, err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("migration parity command canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out", action)
	}
	if strings.Contains(err.Error(), "binding_conflict") {
		return errors.New("binding_conflict")
	}
	return fmt.Errorf("%s failed", action)
}

type pgMigrationParityReport struct {
	RunID               string                  `json:"run_id"`
	State               string                  `json:"state"`
	Code                string                  `json:"code"`
	Members             pgMigrationParityCounts `json:"members"`
	Sources             pgMigrationParityCounts `json:"sources"`
	PendingSources      int64                   `json:"pending_sources"`
	BaselineSealed      bool                    `json:"baseline_sealed"`
	Complete            bool                    `json:"complete"`
	Freshness           string                  `json:"freshness"`
	Passing             bool                    `json:"passing"`
	RequestGeneration   int64                   `json:"request_generation"`
	CompletedGeneration int64                   `json:"completed_generation"`
	BaselineObservedAt  *time.Time              `json:"baseline_observed_at"`
	RuntimeObservedAt   *time.Time              `json:"runtime_observed_at"`
}

type pgMigrationParityCounts struct {
	Matched            int64 `json:"matched"`
	Mismatched         int64 `json:"mismatched"`
	Ambiguous          int64 `json:"ambiguous"`
	LegacyOnly         int64 `json:"legacy_only"`
	Missing            int64 `json:"missing"`
	PartialUnsupported int64 `json:"partial_unsupported"`
	Stale              int64 `json:"stale"`
}

func publicPGMigrationParityReport(report rawderive.ParityReport) pgMigrationParityReport {
	return pgMigrationParityReport{
		RunID: report.RunID, State: report.State, Code: report.Code,
		Members: publicPGMigrationParityCounts(report.Members), Sources: publicPGMigrationParityCounts(report.Sources), PendingSources: report.PendingSources,
		BaselineSealed: report.BaselineSealed, Complete: report.Complete, Freshness: report.Freshness, Passing: report.Passing,
		RequestGeneration: report.RequestGeneration, CompletedGeneration: report.CompletedGeneration,
		BaselineObservedAt: report.BaselineObservedAt, RuntimeObservedAt: report.RuntimeObservedAt,
	}
}

func publicPGMigrationParityCounts(counts rawderive.ParityCounts) pgMigrationParityCounts {
	return pgMigrationParityCounts{
		Matched: counts.Matched, Mismatched: counts.Mismatched, Ambiguous: counts.Ambiguous,
		LegacyOnly: counts.LegacyOnly, Missing: counts.Missing,
		PartialUnsupported: counts.PartialUnsupported, Stale: counts.Stale,
	}
}

func formatParityReport(w io.Writer, format string, report rawderive.ParityReport) error {
	public := publicPGMigrationParityReport(report)
	if format == "json" {
		return json.NewEncoder(w).Encode(public)
	}
	if format != "human" {
		return fmt.Errorf("unsupported migration parity output format %q", format)
	}
	_, err := fmt.Fprintf(w, "Run: %s\nState: %s\nCode: %s\n", public.RunID, public.State, public.Code)
	if err != nil {
		return err
	}
	if err = formatParityCounts(w, "Members", public.Members); err != nil {
		return err
	}
	if err = formatParityCounts(w, "Sources", public.Sources); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w,
		"Pending sources: %d\nBaseline sealed: %t\nComplete: %t\nFreshness: %s\nPassing: %t\nRequest generation: %d\nCompleted generation: %d\nBaseline observed at: %s\nRuntime observed at: %s\n",
		public.PendingSources, public.BaselineSealed, public.Complete, public.Freshness, public.Passing,
		public.RequestGeneration, public.CompletedGeneration, formatParityObservation(public.BaselineObservedAt), formatParityObservation(public.RuntimeObservedAt),
	)
	return err
}

func formatParityCounts(w io.Writer, label string, counts pgMigrationParityCounts) error {
	_, err := fmt.Fprintf(w, "%s: matched=%d mismatched=%d ambiguous=%d legacy_only=%d missing=%d partial_unsupported=%d stale=%d\n",
		label, counts.Matched, counts.Mismatched, counts.Ambiguous, counts.LegacyOnly, counts.Missing, counts.PartialUnsupported, counts.Stale)
	return err
}

func formatParityObservation(value *time.Time) string {
	if value == nil {
		return "unavailable"
	}
	return value.UTC().Format(time.RFC3339Nano)
}
