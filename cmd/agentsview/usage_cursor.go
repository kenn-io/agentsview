package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
	"go.kenn.io/agentsview/internal/db"
)

var newCursorUsageClient = cursorusage.NewClient

type UsageCursorConfig struct {
	Since         string
	Until         string
	All           bool
	PageSize      int
	Email         string
	UserID        string
	EmailChanged  bool
	UserIDChanged bool
}

func newUsageCursorCommand() *cobra.Command {
	var cfg UsageCursorConfig
	cmd := &cobra.Command{
		Use:          "cursor",
		Short:        "Ingest Cursor admin usage events",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.EmailChanged = cmd.Flags().Changed("email")
			cfg.UserIDChanged = cmd.Flags().Changed("user-id")
			return runUsageCursor(cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.Since, "since", "", "Start date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&cfg.Until, "until", "", "End date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&cfg.All, "all", false, "Include all history")
	cmd.Flags().IntVar(&cfg.PageSize, "page-size", 100, "Events per request page")
	cmd.Flags().StringVar(&cfg.Email, "email", "", "Filter by user email")
	cmd.Flags().StringVar(&cfg.UserID, "user-id", "", "Filter by user ID")
	return cmd
}

func runUsageCursor(cfg UsageCursorConfig) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return err
	}
	database, writeLock, err := openWriteDB(
		context.Background(),
		appCfg,
	)
	if err != nil {
		return err
	}
	defer closeWriteDB(database, writeLock)

	apiKey := strings.TrimSpace(appCfg.CursorAdminAPIKey)
	if apiKey == "" {
		return fmt.Errorf("missing Cursor admin API key")
	}

	email := strings.TrimSpace(cfg.Email)
	userID := strings.TrimSpace(cfg.UserID)
	if !cfg.EmailChanged && !cfg.UserIDChanged {
		email = strings.TrimSpace(appCfg.CursorAdminEmail)
		userID = strings.TrimSpace(appCfg.CursorAdminUserID)
	} else {
		if !cfg.EmailChanged {
			email = ""
		}
		if !cfg.UserIDChanged {
			userID = ""
		}
	}

	loc, err := time.LoadLocation(localTimezone())
	if err != nil {
		loc = time.Local
	}

	start, end, err := resolveCursorUsageWindow(cfg, loc)
	if err != nil {
		return err
	}

	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}

	client := newCursorUsageClient(apiKey)
	events, err := client.FetchAllUsageEvents(context.Background(), cursorusage.Query{
		StartDate: start,
		EndDate:   end,
		PageSize:  pageSize,
		Email:     email,
		UserID:    userID,
	})
	if err != nil {
		return err
	}

	rows := make([]db.CursorUsageEvent, 0, len(events))
	for _, ev := range events {
		rows = append(rows, db.CursorUsageEvent{
			OccurredAt:       ev.Timestamp.UTC().Format(time.RFC3339Nano),
			Model:            ev.Model,
			Kind:             ev.Kind,
			InputTokens:      ev.TokenUsage.InputTokens,
			OutputTokens:     ev.TokenUsage.OutputTokens,
			CacheWriteTokens: ev.TokenUsage.CacheWriteTokens,
			CacheReadTokens:  ev.TokenUsage.CacheReadTokens,
			Charged:          ev.Charged,
			CursorTokenFee:   ev.CursorTokenFee,
			UserID:           ev.UserID,
			UserEmail:        ev.UserEmail,
			IsHeadless:       ev.IsHeadless,
		})
	}
	if err := database.InsertCursorUsageEvents(rows); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout,
		"Fetched %d Cursor usage events into the archive\n",
		len(rows),
	)
	return nil
}

func resolveCursorUsageWindow(
	cfg UsageCursorConfig, loc *time.Location,
) (time.Time, time.Time, error) {
	now := time.Now().In(loc)
	if cfg.All {
		return time.Unix(0, 0).UTC(), now.UTC(), nil
	}

	startDate := strings.TrimSpace(cfg.Since)
	endDate := strings.TrimSpace(cfg.Until)
	startDate, endDate = defaultUsageDateRange(startDate, endDate, now)

	var start time.Time
	var end time.Time
	var err error

	if startDate != "" {
		start, err = time.ParseInLocation("2006-01-02", startDate, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf(
				"invalid since date %q: %w", startDate, err,
			)
		}
	} else {
		start = time.Unix(0, 0).In(loc)
	}

	if endDate != "" {
		end, err = time.ParseInLocation("2006-01-02", endDate, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf(
				"invalid until date %q: %w", endDate, err,
			)
		}
		end = end.AddDate(0, 0, 1).Add(-time.Millisecond)
	} else {
		end = now
	}

	if start.After(end) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"since date %q is after until date %q", startDate, endDate,
		)
	}

	return start.UTC(), end.UTC(), nil
}
