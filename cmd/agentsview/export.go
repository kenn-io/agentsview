package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const sessionExportCursorResetExitCode = 4

type exportSessionsFormat string

func (f *exportSessionsFormat) String() string { return string(*f) }

func (f *exportSessionsFormat) Set(v string) error {
	switch v {
	case "json", "ndjson":
		*f = exportSessionsFormat(v)
		return nil
	default:
		return errors.New("must be json or ndjson")
	}
}

func (*exportSessionsFormat) Type() string { return "json|ndjson" }

type exportSessionsConfig struct {
	Project            string
	ExcludeProject     string
	Machine            string
	GitBranch          string
	Agent              string
	Date               string
	DateFrom           string
	DateTo             string
	ActiveSince        string
	MinMessages        int
	MaxMessages        int
	MinUserMessages    int
	IncludeOneShot     bool
	IncludeAutomated   bool
	IncludeChildren    bool
	Outcome            string
	HealthGrade        string
	MinToolFailures    int
	HasSecret          bool
	Cursor             string
	Limit              int
	Format             exportSessionsFormat
	JSON               bool
	All                bool
	MinToolFailuresSet bool
}

type exportSessionsOutput struct {
	SchemaVersion int                               `json:"schema_version"`
	DatabaseID    string                            `json:"database_id"`
	Cursor        exportSessionsOutputCursor        `json:"cursor"`
	Pricing       any                               `json:"pricing"`
	Projects      map[string]export.ProjectMapEntry `json:"projects"`
	Sessions      []db.SessionSummaryRow            `json:"sessions"`
}

type exportSessionsMetaOutput struct {
	Type          string                            `json:"type"`
	SchemaVersion int                               `json:"schema_version"`
	DatabaseID    string                            `json:"database_id"`
	Cursor        exportSessionsOutputCursor        `json:"cursor"`
	Pricing       any                               `json:"pricing"`
	Projects      map[string]export.ProjectMapEntry `json:"projects"`
}

type exportSessionsOutputCursor struct {
	Next string `json:"next"`
}

type exportSessionsCursorResetError struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	DatabaseID string `json:"database_id"`
}

func newExportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "export",
		Short:        "Export local archive data",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newExportSessionsCommand())
	cmd.AddCommand(newExportStatusCommand())
	return cmd
}

func newExportStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show export evidence backfill status",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appCfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			database, err := openExportReadOnlyDB(appCfg)
			if err != nil {
				return err
			}
			defer database.Close()
			status, err := database.ProjectIdentityBackfillStatus(cmd.Context())
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(),
				"project identity evidence: %s (%d/%d)\n",
				status.State, status.CompletedItems, status.TotalItems); err != nil {
				return err
			}
			if status.State == "failed" && status.LastError != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(),
					"last error: %s\n", status.LastError)
			}
			return err
		},
	}
}

func openExportReadOnlyDB(appCfg config.Config) (*db.DB, error) {
	database, err := openReadOnlyDB(appCfg)
	if err == nil {
		return database, nil
	}
	if !db.IsSchemaUpgradeRequired(err) {
		return nil, fmt.Errorf("open local archive: %w", err)
	}
	if upgradeErr := db.UpgradeExportSchemaInPlace(
		appCfg.DBPath, err,
	); upgradeErr != nil {
		return nil, fmt.Errorf(
			"upgrade local archive schema for export: %w", upgradeErr)
	}
	database, err = openReadOnlyDB(appCfg)
	if err != nil {
		return nil, fmt.Errorf("reopen upgraded local archive: %w", err)
	}
	return database, nil
}

func newExportSessionsCommand() *cobra.Command {
	cfg := exportSessionsConfig{
		Limit:  db.MaxSessionLimit,
		Format: exportSessionsFormat("json"),
	}
	cmd := &cobra.Command{
		Use:          "sessions",
		Short:        "Export session summaries from the local archive",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.MinToolFailuresSet = cmd.Flags().Changed("min-tool-failures")
			if cfg.JSON {
				if cmd.Flags().Changed("format") && cfg.Format != "json" {
					return fmt.Errorf("--json cannot be combined with --format %s", cfg.Format)
				}
				cfg.Format = exportSessionsFormat("json")
			}
			return runExportSessions(cmd, cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.Project, "project", "",
		"Filter by project name")
	flags.StringVar(&cfg.ExcludeProject, "exclude-project", "",
		"Exclude sessions from the given project")
	flags.StringVar(&cfg.Machine, "machine", "",
		"Filter by machine name")
	flags.StringVar(&cfg.GitBranch, "git-branch", "",
		"Filter by project/branch token")
	flags.StringVar(&cfg.Agent, "agent", "",
		"Filter by agent (claude, codex, cursor, ...)")
	flags.StringVar(&cfg.Date, "date", "",
		"Filter sessions active on YYYY-MM-DD")
	flags.StringVar(&cfg.DateFrom, "date-from", "",
		"Filter sessions active on or after YYYY-MM-DD")
	flags.StringVar(&cfg.DateTo, "date-to", "",
		"Filter sessions active on or before YYYY-MM-DD")
	flags.StringVar(&cfg.ActiveSince, "active-since", "",
		"Filter sessions active since RFC3339 timestamp")
	flags.IntVar(&cfg.MinMessages, "min-messages", 0,
		"Minimum total message count")
	flags.IntVar(&cfg.MaxMessages, "max-messages", 0,
		"Maximum total message count")
	flags.IntVar(&cfg.MinUserMessages, "min-user-messages", 0,
		"Minimum user message count")
	flags.BoolVar(&cfg.IncludeOneShot, "include-one-shot", false,
		"Include one-shot sessions (excluded by default)")
	flags.BoolVar(&cfg.IncludeAutomated, "include-automated", false,
		"Include automated sessions (excluded by default)")
	flags.BoolVar(&cfg.IncludeChildren, "include-children", false,
		"Include subagent/child sessions")
	flags.StringVar(&cfg.Outcome, "outcome", "",
		"Filter by outcome (comma-separated: success,failure,...)")
	flags.StringVar(&cfg.HealthGrade, "health-grade", "",
		"Filter by health grade (comma-separated: A,B,C,D,F)")
	flags.IntVar(&cfg.MinToolFailures, "min-tool-failures", 0,
		"Minimum tool-failure signal count (0 is a valid filter)")
	flags.BoolVar(&cfg.HasSecret, "has-secret", false,
		"Only sessions with detected secret leaks")
	flags.StringVar(&cfg.Cursor, "cursor", "",
		"Pagination cursor from a previous response")
	flags.IntVar(&cfg.Limit, "limit", db.MaxSessionLimit,
		fmt.Sprintf(
			"Maximum sessions to return (default %d, max %d)",
			db.MaxSessionLimit, db.MaxSessionLimit,
		))
	flags.Var(&cfg.Format, "format", "Output format: json or ndjson")
	flags.BoolVar(&cfg.JSON, "json", false,
		"Emit JSON output (alias for --format json)")
	flags.BoolVar(&cfg.All, "all", false,
		"Export every page as one output stream")
	return cmd
}

func runExportSessions(cmd *cobra.Command, cfg exportSessionsConfig) error {
	if cfg.Cursor != "" {
		if err := validateExportSessionsCursorFlags(cmd.Flags()); err != nil {
			return err
		}
	}
	if cfg.Limit <= 0 || cfg.Limit > db.MaxSessionLimit {
		return fmt.Errorf(
			"--limit must be between 1 and %d", db.MaxSessionLimit)
	}

	appCfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	database, err := openExportReadOnlyDB(appCfg)
	if err != nil {
		return err
	}
	defer database.Close()

	ctx := cmd.Context()
	backfill, err := database.ProjectIdentityBackfillStatus(ctx)
	if err != nil {
		return fmt.Errorf("checking project identity evidence: %w", err)
	}
	if backfill.State != "not_needed" && backfill.State != "completed" {
		return fmt.Errorf(
			"project identity evidence backfill is %s (%d/%d); start or restart the writable daemon, then check `agentsview export status`",
			backfill.State, backfill.CompletedItems, backfill.TotalItems,
		)
	}
	if err := ensureExportSessionsPricing(ctx, database, appCfg); err != nil {
		return err
	}
	databaseID, err := database.GetDatabaseID(ctx)
	if err != nil {
		if errors.Is(err, db.ErrDatabaseIDMissing) {
			return fmt.Errorf(
				"database id missing; restart agentsview serve to initialize export metadata: %w",
				err,
			)
		}
		return err
	}
	pages, err := collectExportSessionPages(ctx, database, cfg)
	if err != nil {
		if isExportSessionsCursorReset(err) {
			return writeExportSessionsCursorReset(cmd, databaseID)
		}
		return err
	}

	output := buildExportSessionsOutput(databaseID, pages)
	enc := json.NewEncoder(cmd.OutOrStdout())
	if cfg.Format == "ndjson" {
		if err := enc.Encode(exportSessionsMetaOutput{
			Type:          "meta",
			SchemaVersion: output.SchemaVersion,
			DatabaseID:    output.DatabaseID,
			Cursor:        output.Cursor,
			Pricing:       output.Pricing,
			Projects:      output.Projects,
		}); err != nil {
			return err
		}
		for _, row := range output.Sessions {
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}
	return enc.Encode(output)
}

// ensureExportSessionsPricing installs embedded fallback plus custom pricing
// for archives whose model_pricing table was never seeded (fresh sync-only
// archives, before serve or usage commands run). The read-only export cannot
// seed the table, and the overlay would override newer fetched rows, so it is
// gated on the table being empty.
func ensureExportSessionsPricing(
	ctx context.Context, database *db.DB, appCfg config.Config,
) error {
	seeded, err := database.HasModelPricingRows(ctx)
	if err != nil {
		return fmt.Errorf("checking export pricing: %w", err)
	}
	if seeded {
		return nil
	}
	applyFallbackPricing(database, appCfg.CustomModelPricing)
	return nil
}

func validateExportSessionsCursorFlags(flags *pflag.FlagSet) error {
	for _, name := range []string{
		"project",
		"exclude-project",
		"machine",
		"git-branch",
		"agent",
		"date",
		"date-from",
		"date-to",
		"active-since",
		"min-messages",
		"max-messages",
		"min-user-messages",
		"include-one-shot",
		"include-automated",
		"include-children",
		"outcome",
		"health-grade",
		"min-tool-failures",
		"has-secret",
		"all",
	} {
		if flags.Changed(name) {
			return fmt.Errorf(
				"--cursor cannot be combined with --%s", name)
		}
	}
	return nil
}

func collectExportSessionPages(
	ctx context.Context, database *db.DB, cfg exportSessionsConfig,
) ([]db.SessionExportResult, error) {
	opts := db.SessionExportOptions{
		Filter:          exportSessionsFilter(cfg),
		Cursor:          cfg.Cursor,
		UseCursorFilter: cfg.Cursor != "",
		Limit:           cfg.Limit,
		Format:          string(cfg.Format),
	}
	if cfg.All {
		return database.ExportAllSessionSummaries(ctx, opts)
	}
	result, err := database.ExportSessionSummaries(ctx, opts)
	if err != nil {
		return nil, err
	}
	pages := []db.SessionExportResult{result}
	return pages, nil
}

func exportSessionsFilter(cfg exportSessionsConfig) db.SessionFilter {
	filter := db.SessionFilter{
		Project:          cfg.Project,
		ExcludeProject:   cfg.ExcludeProject,
		Machine:          cfg.Machine,
		GitBranch:        cfg.GitBranch,
		Agent:            cfg.Agent,
		Date:             cfg.Date,
		DateFrom:         cfg.DateFrom,
		DateTo:           cfg.DateTo,
		ActiveSince:      cfg.ActiveSince,
		MinMessages:      cfg.MinMessages,
		MaxMessages:      cfg.MaxMessages,
		MinUserMessages:  cfg.MinUserMessages,
		ExcludeOneShot:   !cfg.IncludeOneShot,
		ExcludeAutomated: !cfg.IncludeAutomated,
		IncludeChildren:  cfg.IncludeChildren,
		Outcome:          splitExportSessionsCSV(cfg.Outcome),
		HealthGrade:      splitExportSessionsCSV(cfg.HealthGrade),
		HasSecret:        cfg.HasSecret,
	}
	if cfg.MinToolFailuresSet {
		filter.MinToolFailures = &cfg.MinToolFailures
	}
	return filter
}

func splitExportSessionsCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func buildExportSessionsOutput(
	databaseID string, pages []db.SessionExportResult,
) exportSessionsOutput {
	var pricing *export.PricingBlock
	output := exportSessionsOutput{
		SchemaVersion: export.SessionSummarySchemaVersion,
		DatabaseID:    databaseID,
		Cursor:        exportSessionsOutputCursor{},
		Pricing:       map[string]any{},
		Projects:      map[string]export.ProjectMapEntry{},
		Sessions:      []db.SessionSummaryRow{},
	}
	for _, page := range pages {
		if output.SchemaVersion == export.SessionSummarySchemaVersion &&
			page.SchemaVersion != 0 {
			output.SchemaVersion = page.SchemaVersion
		}
		output.Sessions = append(output.Sessions, page.Rows...)
		output.Cursor.Next = page.NextCursor
		if page.Pricing != nil {
			pricing = mergeExportSessionsPricing(pricing, page.Pricing)
		}
		if page.Projects != nil {
			for key, next := range page.Projects {
				if existing, ok := output.Projects[key]; ok {
					output.Projects[key] = db.MergeSessionProjectCatalogEntry(
						existing, next,
					)
					continue
				}
				output.Projects[key] = next
			}
		}
	}
	if pricing != nil {
		output.Pricing = pricing
	}
	return output
}

func mergeExportSessionsPricing(
	base, next *export.PricingBlock,
) *export.PricingBlock {
	if next == nil {
		return cloneExportSessionsPricing(base)
	}
	if base == nil {
		return cloneExportSessionsPricing(next)
	}

	merged := cloneExportSessionsPricing(base)
	if merged.Models == nil {
		merged.Models = map[string]export.EffectiveModelRate{}
	}
	for model, rate := range next.Models {
		if existing, ok := merged.Models[model]; ok {
			rate = mergeExportSessionsModelRate(existing, rate)
		} else {
			rate = cloneExportSessionsModelRate(rate)
		}
		merged.Models[model] = rate
	}
	merged.Fallback.Models = mergeExportSessionsStringSets(
		merged.Fallback.Models, next.Fallback.Models)
	merged.Fallback.Used = len(merged.Fallback.Models) > 0
	merged.CostSource = mergeExportSessionsMergedCostSource(
		exportSessionsPricingMergeCostSource(base),
		exportSessionsPricingMergeCostSource(next))
	return merged
}

func exportSessionsPricingMergeCostSource(
	block *export.PricingBlock,
) export.CostSource {
	if block == nil || len(block.Models) == 0 {
		return ""
	}
	return block.CostSource
}

func mergeExportSessionsMergedCostSource(
	a, b export.CostSource,
) export.CostSource {
	source := mergeExportSessionsCostSource(a, b)
	if source == "" {
		return export.CostSourceComputed
	}
	return source
}

func cloneExportSessionsPricing(
	block *export.PricingBlock,
) *export.PricingBlock {
	if block == nil {
		return nil
	}
	clone := *block
	clone.Fallback.Models = append([]string{}, block.Fallback.Models...)
	clone.Fallback.Used = len(clone.Fallback.Models) > 0
	clone.Models = make(map[string]export.EffectiveModelRate,
		len(block.Models))
	for model, rate := range block.Models {
		clone.Models[model] = cloneExportSessionsModelRate(rate)
	}
	return &clone
}

func cloneExportSessionsModelRate(
	rate export.EffectiveModelRate,
) export.EffectiveModelRate {
	if rate.MatchedPattern != nil {
		pattern := *rate.MatchedPattern
		rate.MatchedPattern = &pattern
	}
	return rate
}

func mergeExportSessionsModelRate(
	base, next export.EffectiveModelRate,
) export.EffectiveModelRate {
	merged := cloneExportSessionsModelRate(base)
	if merged.MatchedPattern == nil && next.MatchedPattern != nil {
		pattern := *next.MatchedPattern
		merged.MatchedPattern = &pattern
	}
	merged.CostSource = mergeExportSessionsCostSource(
		merged.CostSource, next.CostSource)
	return merged
}

func mergeExportSessionsCostSource(
	a, b export.CostSource,
) export.CostSource {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case a == b:
		return a
	default:
		return export.CostSourceMixed
	}
}

func mergeExportSessionsStringSets(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, value := range a {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	for _, value := range b {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return sortedStringSet(set)
}

func sortedStringSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func isExportSessionsCursorReset(err error) bool {
	return errors.Is(err, db.ErrSessionExportCursorReset) ||
		errors.Is(err, db.ErrInvalidCursor)
}

func writeExportSessionsCursorReset(
	cmd *cobra.Command, databaseID string,
) error {
	payload := exportSessionsCursorResetError{
		Error:      "cursor_reset",
		Message:    "session export cursor is no longer valid; restart the export",
		DatabaseID: databaseID,
	}
	if err := json.NewEncoder(cmd.ErrOrStderr()).Encode(payload); err != nil {
		return err
	}
	return withSilentExitCode(
		errors.New("session export cursor reset required"),
		sessionExportCursorResetExitCode,
	)
}
