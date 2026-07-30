package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

const maxReportingDigestDays = 31

type exportReportingDeps struct {
	now          func() time.Time
	openDatabase func(*cobra.Command) (*db.DB, func(), error)
}

func defaultExportReportingDeps() exportReportingDeps {
	return exportReportingDeps{
		now:          time.Now,
		openDatabase: openReportingExportDB,
	}
}

func newExportHourCommand(deps exportReportingDeps) *cobra.Command {
	var schemaVersion *int
	command := &cobra.Command{
		Use:          "hour YYYY-MM-DD-HH",
		Short:        "Export one closed UTC reporting hour",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateReportingSchemaVersion(*schemaVersion); err != nil {
				return err
			}
			now := deps.now()
			hourStart, err := export.ParseReportingHourKey(args[0], now)
			if err != nil {
				return err
			}
			database, cleanup, err := deps.openDatabase(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			day, err := database.ExportReportingDay(
				cmd.Context(),
				db.ReportingExportOptions{
					Date: hourStart.Truncate(24 * time.Hour),
					Now:  now,
				},
			)
			if err != nil {
				return err
			}
			index := hourStart.Hour()
			if index >= len(day.Hours) ||
				day.Hours[index].Period != args[0] {
				return fmt.Errorf("reporting hour %q is unavailable", args[0])
			}
			return writeCanonicalReportingDocument(
				cmd, day.Hours[index],
			)
		},
	}
	schemaVersion = bindReportingSchemaVersion(command)
	return command
}

func newExportDayCommand(deps exportReportingDeps) *cobra.Command {
	var schemaVersion *int
	command := &cobra.Command{
		Use:          "day YYYY-MM-DD",
		Short:        "Export all closed UTC reporting hours for a date",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateReportingSchemaVersion(*schemaVersion); err != nil {
				return err
			}
			date, err := export.ParseReportingDate(args[0])
			if err != nil {
				return err
			}
			database, cleanup, err := deps.openDatabase(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			day, err := database.ExportReportingDay(
				cmd.Context(),
				db.ReportingExportOptions{Date: date, Now: deps.now()},
			)
			if err != nil {
				return err
			}
			return writeCanonicalReportingDocument(cmd, day)
		},
	}
	schemaVersion = bindReportingSchemaVersion(command)
	return command
}

func newExportDigestCommand(deps exportReportingDeps) *cobra.Command {
	var fromValue string
	var toValue string
	var schemaVersion *int
	command := &cobra.Command{
		Use:          "digest --from YYYY-MM-DD --to YYYY-MM-DD",
		Short:        "Export reporting digests for a UTC date range",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateReportingSchemaVersion(*schemaVersion); err != nil {
				return err
			}
			if fromValue == "" || toValue == "" {
				return fmt.Errorf("--from and --to are required")
			}
			from, err := export.ParseReportingDate(fromValue)
			if err != nil {
				return fmt.Errorf("invalid --from: %w", err)
			}
			to, err := export.ParseReportingDate(toValue)
			if err != nil {
				return fmt.Errorf("invalid --to: %w", err)
			}
			if from.After(to) {
				return fmt.Errorf("--from must not be after --to")
			}
			dayCount := int(to.Sub(from)/(24*time.Hour)) + 1
			if dayCount > maxReportingDigestDays {
				return fmt.Errorf(
					"digest range contains %d dates; maximum is %d",
					dayCount,
					maxReportingDigestDays,
				)
			}

			database, cleanup, err := deps.openDatabase(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			now := deps.now()
			days := make([]export.ReportingDigestDay, 0, dayCount)
			for date := from; !date.After(to); date = date.Add(24 * time.Hour) {
				day, err := database.ExportReportingDay(
					cmd.Context(),
					db.ReportingExportOptions{Date: date, Now: now},
				)
				if err != nil {
					return err
				}
				hourDigests := make([]string, len(day.Hours))
				for i := range day.Hours {
					hourDigests[i] = day.Hours[i].Digest
				}
				days = append(days, export.ReportingDigestDay{
					Date:        day.Date,
					Complete:    day.Complete,
					HasData:     day.HasData,
					DayDigest:   day.Digest,
					HourDigests: hourDigests,
				})
			}
			return writeCanonicalReportingDocument(cmd, export.ReportingDigest{
				SchemaVersion: export.ReportingSchemaVersion,
				From:          fromValue,
				To:            toValue,
				Days:          days,
			})
		},
	}
	schemaVersion = bindReportingSchemaVersion(command)
	command.Flags().StringVar(
		&fromValue, "from", "", "First UTC date (YYYY-MM-DD)",
	)
	command.Flags().StringVar(
		&toValue, "to", "", "Last UTC date (YYYY-MM-DD)",
	)
	return command
}

func bindReportingSchemaVersion(command *cobra.Command) *int {
	version := new(int)
	command.Flags().IntVar(
		version,
		"schema-version",
		export.ReportingSchemaVersion,
		"Reporting export schema version",
	)
	return version
}

func validateReportingSchemaVersion(version int) error {
	if version != export.ReportingSchemaVersion {
		return fmt.Errorf("unsupported reporting schema version %d", version)
	}
	return nil
}

func openReportingExportDB(
	cmd *cobra.Command,
) (*db.DB, func(), error) {
	appConfig, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, func() {}, fmt.Errorf("loading config: %w", err)
	}
	database, err := openExportReadOnlyDB(appConfig)
	if err != nil {
		return nil, func() {}, err
	}
	if err := ensureExportSessionsPricing(
		cmd.Context(), database, appConfig,
	); err != nil {
		_ = database.Close()
		return nil, func() {}, err
	}
	return database, func() {
		_ = database.Close()
	}, nil
}

func writeCanonicalReportingDocument(
	cmd *cobra.Command, document any,
) error {
	canonical, err := export.MarshalCanonical(document)
	if err != nil {
		return fmt.Errorf("marshal reporting export: %w", err)
	}
	canonical = append(canonical, '\n')
	if _, err := cmd.OutOrStdout().Write(canonical); err != nil {
		return fmt.Errorf("write reporting export: %w", err)
	}
	return nil
}
