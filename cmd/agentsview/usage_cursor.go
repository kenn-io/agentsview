package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/cursorusage"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/timeutil"
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
		Use:   "cursor",
		Short: "Ingest Cursor admin usage events",
		Long: "Fetch Cursor organization usage through the configured admin API and store it in the local archive. " +
			"It requires a Cursor admin API key. " +
			"Explicit --email or --user-id overrides the configured member selection.",
		Example:      "  agentsview source cursor sync-usage --since 2026-10-01 --until 2026-10-07",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.EmailChanged = cmd.Flags().Changed("email")
			cfg.UserIDChanged = cmd.Flags().Changed("user-id")
			return runUsageCursor(cmd.Context(), cfg)
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

func runUsageCursor(ctx context.Context, cfg UsageCursorConfig) error {
	appCfg, err := config.LoadMinimalFor(config.ScopeLocal)
	if err != nil {
		return err
	}
	database, writeLock, err := openWriteDB(
		ctx,
		appCfg,
	)
	if err != nil {
		return err
	}
	defer closeWriteDB(database, writeLock)

	apiKey, err := appCfg.ResolveCursorAdminAPIKey()
	if err != nil {
		return err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return errors.New("missing Cursor admin API key")
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

	loc := timeutil.LocalLocation()

	start, end, err := resolveCursorUsageWindow(cfg, loc)
	if err != nil {
		return err
	}

	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}

	client := newCursorUsageClient(apiKey)
	client.OnRetry = func(page int, err error, wait time.Duration) {
		fmt.Fprintf(os.Stderr, "Cursor usage page %d: %v; retrying in %s\n",
			page, err, wait.Round(time.Second))
	}
	closedThrough := time.Now().UTC().Truncate(time.Hour)
	events, err := client.FetchAllUsageEvents(ctx, cursorusage.Query{
		StartDate: start,
		EndDate:   end,
		PageSize:  pageSize,
		Email:     email,
		UserID:    userID,
	})
	if err != nil {
		return err
	}

	polledAt := time.Now().UTC()
	rows := make([]db.CursorUsageEvent, 0, len(events))
	fetched := make(map[string]bool, len(events))
	for _, ev := range events {
		row := cursorUsageRow(ev)
		rows = append(rows, row)
		fetched[db.CursorUsageEventDedupKey(row)] = true
	}
	if err := database.InsertCursorUsageEvents(ctx, rows); err != nil {
		return err
	}
	if email == "" && userID == "" {
		if err := database.RequireCursorAccountStamping(ctx); err != nil {
			if !errors.Is(err, db.ErrCursorAccountRebuildPending) {
				return err
			}
			fmt.Fprintf(os.Stdout, "Imported %d events; skipped account reporting until the archive rebuild finishes. Rerun this sync then.\n", len(rows))
			return nil
		}
		windowEnd := end.Add(time.Millisecond)
		if windowEnd.After(closedThrough) {
			windowEnd = closedThrough
		}
		if start.Before(windowEnd) {
			const rosterWindow = 30 * 24 * time.Hour
			windows := map[int64]bool{}
			priorHours, err := database.CursorAccountHours(ctx, start, windowEnd)
			if err != nil {
				return err
			}
			for _, hour := range priorHours {
				windows[int64(hour.Start.Sub(start)/rosterWindow)] = true
			}
			for _, ev := range events {
				hour := ev.Timestamp.UTC().Truncate(time.Hour)
				if !hour.Before(start) && !hour.Add(time.Hour).After(windowEnd) {
					windows[int64(ev.Timestamp.Sub(start)/rosterWindow)] = true
				}
			}
			var users []string
			seenUsers := map[string]bool{}
			membersByWindow := map[int64]map[string]bool{}
			for from, index := start, int64(0); from.Before(windowEnd); index++ {
				to := from.Add(rosterWindow)
				if to.After(windowEnd) {
					to = windowEnd
				}
				if windows[index] {
					members, err := client.ListDailyUsageUsers(ctx, from, to)
					if err != nil {
						return err
					}
					membersByWindow[index] = map[string]bool{}
					for _, id := range members {
						membersByWindow[index][id] = true
						if !seenUsers[id] {
							users = append(users, id)
							seenUsers[id] = true
						}
					}
				}
				from = to
			}
			owners := map[string]string{}
			keyHours := map[string]time.Time{}
			ambiguous := map[db.CursorAccountHour]bool{}
			for _, id := range users {
				memberEvents, err := client.FetchAllUsageEvents(ctx, cursorusage.Query{StartDate: start, EndDate: windowEnd.Add(-time.Millisecond), PageSize: 1000, UserID: id})
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return fmt.Errorf("fetching Cursor usage for a member: %w", err)
				}
				for _, ev := range memberEvents {
					row := cursorUsageRow(ev)
					hour := ev.Timestamp.UTC().Truncate(time.Hour)
					if hour.Before(start) || hour.Add(time.Hour).After(windowEnd) {
						continue
					}
					key := db.CursorUsageEventDedupKey(row)
					if !fetched[key] {
						continue
					}
					keyHours[key] = hour
					if previous, ok := owners[key]; ok && previous != id {
						if previous != "" {
							ambiguous[db.CursorAccountHour{UserID: previous, Start: hour}] = true
						}
						ambiguous[db.CursorAccountHour{UserID: id, Start: hour}] = true
						owners[key] = ""
					} else if !ok {
						owners[key] = id
					}
				}
			}
			byUser := map[string][]string{}
			coveredSet := map[db.CursorAccountHour]bool{}
			for key, match := range owners {
				hour := db.CursorAccountHour{UserID: match, Start: keyHours[key]}
				byUser[match] = append(byUser[match], key)
				if match != "" {
					coveredSet[hour] = true
				}
			}
			for hour := range ambiguous {
				coveredSet[hour] = true
			}
			for _, hour := range priorHours {
				index := int64(hour.Start.Sub(start) / rosterWindow)
				if membersByWindow[index][hour.UserID] {
					coveredSet[hour] = true
				}
			}
			var covered []db.CursorAccountHour
			for hour := range coveredSet {
				covered = append(covered, hour)
			}
			if err := database.RecordCursorAccountPoll(ctx, byUser, polledAt, covered); err != nil {
				return err
			}
		}
	}

	fmt.Fprintf(os.Stdout,
		"Fetched %d Cursor usage events into the archive\n",
		len(rows),
	)
	return nil
}

func cursorUsageRow(ev cursorusage.UsageEvent) db.CursorUsageEvent {
	return db.CursorUsageEvent{
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
	}
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
