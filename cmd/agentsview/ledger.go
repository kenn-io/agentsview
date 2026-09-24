package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
	"go.kenn.io/agentsview/internal/ledgerstatus"
	"go.kenn.io/agentsview/internal/serdejson"
)

func newLedgerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "ledger",
		Short:        "Append to, inspect and maintain the event ledger",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newLedgerAppendCommand())
	cmd.AddCommand(newLedgerStatusCommand())
	cmd.AddCommand(newLedgerQueryCommand())
	cmd.AddCommand(newLedgerVerifyCommand())
	cmd.AddCommand(newLedgerImportCommand())
	cmd.AddCommand(newLedgerExportCommand())
	cmd.AddCommand(newLedgerRebuildIndexCommand())
	return cmd
}

var errLedgerDisabled = errors.New(
	"the event ledger is off; set enabled = true under [ledger] in config.toml")

// ledgerZones resolves --zone against the configured zones; "" means every
// zone, default first.
func ledgerZones(cfg config.Config, zone string) ([]string, error) {
	ids := cfg.Ledger.ZoneIDs()
	if zone == "" {
		return ids, nil
	}
	if _, ok := cfg.Ledger.Zone(zone); !ok {
		return nil, fmt.Errorf("zone %q is not configured (configured: %s)", zone, strings.Join(ids, ", "))
	}
	return []string{zone}, nil
}

func writeLedgerJSON(w io.Writer, v any) error {
	return json.MarshalEncode(jsontext.NewEncoder(w, jsontext.WithIndent("  ")), v)
}

func newLedgerAppendCommand() *cobra.Command {
	var class, tier, zone, subsystem, summary, payload, actor, object string
	cmd := &cobra.Command{
		Use:   "append",
		Short: "Append one event as the local source",
		Long: "Append one event to a zone as a new sealed segment written by this " +
			"host's local source ([ledger] source, default av-<installation id>).",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if !cfg.Ledger.Enabled {
				return errLedgerDisabled
			}
			zones, err := ledgerZones(cfg, zone)
			if err != nil {
				return err
			}
			event, err := buildLedgerAppendEvent(class, tier, subsystem, summary, payload, actor, object)
			if err != nil {
				return err
			}
			tr, daemon, err := ledgerDaemon(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if daemon {
				res, err := requestLedgerAppend(cmd.Context(), tr, cfg.AuthToken, zones[0], event)
				if err != nil {
					return err
				}
				return writeLedgerAppendResult(cmd.OutOrStdout(), res, outputFormat(cmd) == "json")
			}
			database, lock, err := openWriteDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			source := cfg.Ledger.EffectiveSource(cfg.InstallationID)
			zw := ledger.NewZoneWriters(database, source, cfg.Ledger.ZoneIDs(), nil)
			seg, err := zw.AppendSegment(cmd.Context(), zones[0], []ledger.Event{event})
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(seg.Events))
			for _, e := range seg.Events {
				ids = append(ids, e.EventID.String())
			}
			return writeLedgerAppendResult(cmd.OutOrStdout(), ledgerAppendResult{
				Zone: zones[0], Source: seg.Source, SourceSeq: seg.SourceSeq,
				Checksum: seg.Checksum, EventIDs: ids,
			}, outputFormat(cmd) == "json")
		},
	}
	f := cmd.Flags()
	f.StringVar(&class, "class", "", "Event class (ingest, route, decision, state-change, claim, delivery, projection, health, approval, note-meta)")
	f.StringVar(&tier, "tier", "", "Payload tier: metadata-only, structured or confidential (default: structured with a payload, else metadata-only)")
	f.StringVar(&zone, "zone", "", "Zone (default: [ledger] default_zone)")
	f.StringVar(&subsystem, "subsystem", "", "Sets payload.subsystem")
	f.StringVar(&summary, "summary", "", "Sets payload.summary")
	f.StringVar(&payload, "payload", "", "Payload as JSON")
	f.StringVar(&actor, "actor", "", "actor_ref, e.g. machine:<name>")
	f.StringVar(&object, "object", "", "object_ref, e.g. subsystem:<name>")
	registerFormatFlags(f)
	_ = cmd.MarkFlagRequired("class")
	return cmd
}

func buildLedgerAppendEvent(class, tier, subsystem, summary, payload, actor, object string) (ledger.Event, error) {
	c, err := ledger.ParseClass(class)
	if err != nil {
		return ledger.Event{}, err
	}
	var body any
	if payload != "" {
		if body, err = serdejson.Decode([]byte(payload)); err != nil {
			return ledger.Event{}, fmt.Errorf("--payload is not valid JSON: %w", err)
		}
	}
	if subsystem != "" || summary != "" {
		if body == nil {
			body = map[string]any{}
		}
		obj, ok := body.(map[string]any)
		if !ok {
			return ledger.Event{}, errors.New("--subsystem and --summary need --payload to be a JSON object")
		}
		if subsystem != "" {
			obj["subsystem"] = subsystem
		}
		if summary != "" {
			obj["summary"] = summary
		}
	}
	t := ledger.TierMetadataOnly
	if body != nil {
		t = ledger.TierStructured
	}
	if tier != "" {
		if t, err = ledger.ParseTier(tier); err != nil {
			return ledger.Event{}, err
		}
	}
	e := ledger.Event{EventClass: c, PayloadTier: t, Payload: body}
	if actor != "" {
		e.ActorRef = &actor
	}
	if object != "" {
		e.ObjectRef = &object
	}
	return e, nil
}

func writeLedgerAppendResult(w io.Writer, res ledgerAppendResult, jsonOut bool) error {
	if jsonOut {
		return writeLedgerJSON(w, res)
	}
	_, err := fmt.Fprintf(w, "appended %d event(s) to %s as %s-%06d.json\n",
		len(res.EventIDs), res.Zone, res.Source, res.SourceSeq)
	return err
}

func newLedgerStatusCommand() *cobra.Command {
	var zone string
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show segments, sources, gaps and verify failures per zone",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			zones, err := ledgerZones(cfg, zone)
			if err != nil {
				return err
			}
			tr, daemon, err := ledgerDaemon(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if daemon {
				reports, enabled, err := requestLedgerStatus(cmd.Context(), tr, cfg.AuthToken, zone)
				if err != nil {
					return err
				}
				if outputFormat(cmd) == "json" {
					return writeLedgerJSON(cmd.OutOrStdout(), reports)
				}
				return ledgerstatus.WriteText(cmd.OutOrStdout(), enabled, reports)
			}
			database, err := openReadOnlyDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer database.Close()
			reports, err := ledgerstatus.Collect(cmd.Context(), cfg.Ledger, database, zones)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return writeLedgerJSON(cmd.OutOrStdout(), reports)
			}
			return ledgerstatus.WriteText(cmd.OutOrStdout(), cfg.Ledger.Enabled, reports)
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Only this zone")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func newLedgerVerifyCommand() *cobra.Command {
	var zone string
	var full bool
	cmd := &cobra.Command{
		Use:          "verify",
		Short:        "Re-check segment checksums (incremental unless --full)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			zones, err := ledgerZones(cfg, zone)
			if err != nil {
				return err
			}
			tr, daemon, err := ledgerDaemon(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if daemon {
				reports, err := requestLedgerVerify(cmd.Context(), tr, cfg.AuthToken, zone, full)
				if err != nil {
					return err
				}
				failures := 0
				for _, z := range reports {
					fmt.Fprintf(cmd.OutOrStdout(), "verify [%s]: %d newly verified, %d skipped, %d failure(s)\n",
						z.Zone, z.NewlyVerified, z.Skipped, len(z.Failures))
					for _, f := range z.Failures {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", ledgerstatus.FormatID(f[0], f[1]), f[2])
					}
					failures += len(z.Failures)
				}
				if failures > 0 {
					return fmt.Errorf("ledger verify found %d failure(s)", failures)
				}
				return nil
			}
			database, lock, err := openWriteDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			failures := 0
			for _, z := range zones {
				rep, err := ledger.VerifyZone(cmd.Context(), database, z, full)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "verify [%s]: %d newly verified, %d skipped, %d failure(s)\n",
					z, rep.NewlyVerified, rep.Skipped, len(rep.Failures))
				for _, f := range rep.Failures {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", ledgerstatus.FormatID(f[0], f[1]), f[2])
				}
				failures += len(rep.Failures)
			}
			if failures > 0 {
				return fmt.Errorf("ledger verify found %d failure(s)", failures)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Only this zone")
	cmd.Flags().BoolVar(&full, "full", false, "Re-verify every segment and reset the checkpoints")
	return cmd
}

func newLedgerImportCommand() *cobra.Command {
	var zone, dir string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a jilog-format segments directory into a zone",
		Long: "Import every new segment file from a jilog-format segments directory. " +
			"Each file is identity-checked against its name and CRC-verified; bad " +
			"files are reported and retried on the next import.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if !cfg.Ledger.Enabled {
				return errLedgerDisabled
			}
			if _, err := ledgerZones(cfg, zone); err != nil {
				return err
			}
			database, lock, err := openWriteDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			report, err := runLedgerImport(cmd.Context(), database, zone, dir)
			if err != nil {
				return err
			}
			if err := writeLedgerImportText(cmd.OutOrStdout(), zone, report); err != nil {
				return err
			}
			if len(report.Failed) > 0 || len(report.ListingErrors) > 0 {
				return errors.New("ledger import finished with failed segments (see above); they are retried on the next import")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Zone to import into")
	cmd.Flags().StringVar(&dir, "segments", "", "The segments directory (for a jilog ledger_path, <ledger_path>/segments)")
	_ = cmd.MarkFlagRequired("zone")
	_ = cmd.MarkFlagRequired("segments")
	return cmd
}

// runLedgerImport imports dir and records the report for `ledger status`.
func runLedgerImport(ctx context.Context, database *db.DB, zone, dir string) (segfile.ImportReport, error) {
	report, err := segfile.ImportDir(ctx, dir, zone, database)
	if err != nil {
		return report, err
	}
	b, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	return report, database.SaveLedgerImportState(ctx, zone, dir, string(b))
}

func writeLedgerImportText(w io.Writer, zone string, r segfile.ImportReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ledger import [%s]: %d segment(s) and %d event(s) indexed, %d skipped, %d failed\n",
		zone, r.SegmentsIndexed, r.EventsIndexed, r.Skipped, len(r.Failed))
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "  %s: %s\n", ledgerstatus.FormatID(f[0], f[1]), f[2])
	}
	for _, e := range r.ListingErrors {
		fmt.Fprintf(&b, "  %s\n", e)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func newLedgerExportCommand() *cobra.Command {
	var zone, dir, source string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write a zone's segments as jilog-format files",
		Long: "Write a zone's segments into a directory as jilog-format files. Existing " +
			"files are never replaced: identical ones are skipped and different ones " +
			"are reported.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if _, err := ledgerZones(cfg, zone); err != nil {
				return err
			}
			database, err := openReadOnlyDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer database.Close()
			report, err := segfile.ExportZone(cmd.Context(), database, zone, dir, source)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ledger export [%s]: %d written, %d already identical, %d failed\n",
				zone, report.Written, report.Identical, len(report.Failed))
			for _, f := range report.Failed {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", f[0], f[1])
			}
			if len(report.Failed) > 0 {
				return errors.New("ledger export left conflicting files untouched (see above)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Zone to export")
	cmd.Flags().StringVar(&dir, "dir", "", "Destination segments directory")
	cmd.Flags().StringVar(&source, "source", "", "Only this source")
	_ = cmd.MarkFlagRequired("zone")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

func newLedgerRebuildIndexCommand() *cobra.Command {
	var zone string
	cmd := &cobra.Command{
		Use:          "rebuild-index",
		Short:        "Rebuild a zone's query projection from its stored segments",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if !cfg.Ledger.Enabled {
				return errLedgerDisabled
			}
			if _, err := ledgerZones(cfg, zone); err != nil {
				return err
			}
			database, lock, err := openWriteDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			events, segments, err := database.RebuildLedgerIndex(cmd.Context(), zone)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ledger rebuild-index [%s]: %d event(s) projected from %d segment(s)\n",
				zone, events, segments)
			return nil
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Zone to rebuild")
	_ = cmd.MarkFlagRequired("zone")
	return cmd
}
