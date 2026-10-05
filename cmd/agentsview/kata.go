package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
)

var kataDaemonHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

const kataStatusRequestAllowance = 10 * time.Second // 5s discovery and 5s response overhead

// A fresh daemon status probe can discover Kata, then make three sequential
// requests. Leave room for the daemon's response after those request budgets.
func kataStatusRequestBudget(perRequest time.Duration) time.Duration {
	return kata.ProbeBudget(perRequest, kataStatusRequestAllowance)
}

func newKataCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "kata",
		Short:        "Inspect the optional Kata issue-tracker connection",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE:         func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.PersistentFlags().String("server", "", "Remote daemon URL for Kata status requests")
	cmd.PersistentFlags().String("server-token-file", "", "File containing bearer token for explicit --server requests")
	cmd.AddCommand(newKataStatusCommand())
	return cmd
}

func newKataStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show whether the configured Kata instance is reachable and ready",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := resolveKataStatus(cmd)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				if err := json.MarshalWrite(cmd.OutOrStdout(), st); err != nil {
					return err
				}
				_, err := io.WriteString(cmd.OutOrStdout(), "\n")
				return err
			}
			printKataStatus(cmd.OutOrStdout(), st)
			return nil
		},
	}
	registerFormatFlags(cmd.Flags())
	return cmd
}

// resolveKataStatus asks the daemon that owns this data directory (or an
// explicit --server) so the answer reflects that process's environment; with
// no daemon it probes locally from the loaded config.
func resolveKataStatus(cmd *cobra.Command) (kata.Status, error) {
	ctx := cmd.Context()
	remote, err := cmd.Flags().GetString("server")
	if err != nil {
		return kata.Status{}, err
	}
	if remote = strings.TrimSpace(remote); remote != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return kata.Status{}, err
		}
		return fetchKataStatus(ctx, remote, token, kataStatusRequestBudget(kata.DefaultTimeout))
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return kata.Status{}, fmt.Errorf("loading config: %w", err)
	}
	tr, err := detectTransportContext(ctx, cfg.DataDir, cfg.AuthToken, 0)
	if err != nil {
		return kata.Status{}, fmt.Errorf("kata status request failed: detecting daemon: %w", err)
	}
	if tr.Mode == transportHTTP {
		return fetchKataStatus(ctx, tr.URL, cfg.AuthToken, kataStatusRequestBudget(cfg.Kata.Timeout))
	}
	if tr.DirectReadOnly {
		if tr.DirectIncompatible {
			return kata.Status{}, fmt.Errorf("kata status request failed: %w", directIncompatibleDaemonError(tr))
		}
		return kata.Status{}, fmt.Errorf("kata status request failed: %w", errLocalDaemonUnreachable)
	}
	hub := filing.EligibleHost(false, cfg.HasPGPushTarget())
	st, _ := kata.Probe(ctx, kata.ConfigFrom(cfg.Kata, hub))
	return st, nil
}

func fetchKataStatus(ctx context.Context, baseURL, token string, budget time.Duration) (kata.Status, error) {
	requestCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client := *kataDaemonHTTPClient
	client.Timeout = budget
	api, err := apiclient.NewHTTPClient(baseURL, token, &client)
	if err != nil {
		return kata.Status{}, errors.New("kata status request failed: invalid server URL")
	}
	resp, err := api.GetAPIV1KataStatusWithResponse(requestCtx)
	if err != nil {
		if resp != nil && resp.StatusCode != 0 {
			if resp.StatusCode == http.StatusOK {
				return kata.Status{}, errors.New("kata status request failed: invalid HTTP 200 response")
			}
			return kata.Status{}, fmt.Errorf("kata status request failed: HTTP %d", resp.StatusCode)
		}
		switch {
		case errors.Is(err, context.Canceled):
			return kata.Status{}, fmt.Errorf("kata status request failed: %w", context.Canceled)
		case errors.Is(err, context.DeadlineExceeded):
			return kata.Status{}, fmt.Errorf("kata status request failed: %w", context.DeadlineExceeded)
		default:
			return kata.Status{}, errors.New("kata status request failed: connection error")
		}
	}
	if resp == nil {
		return kata.Status{}, errors.New("kata status request failed: empty response")
	}
	if resp.StatusCode != http.StatusOK {
		return kata.Status{}, fmt.Errorf("kata status request failed: HTTP %d", resp.StatusCode)
	}
	if !validKataStatus(resp.JSON200) {
		return kata.Status{}, errors.New("kata status request failed: invalid HTTP 200 response")
	}
	return *resp.JSON200, nil
}

func validKataStatus(st *kata.Status) bool {
	if st == nil || strings.TrimSpace(st.Project) == "" {
		return false
	}
	switch st.State {
	case kata.StateDisabled, kata.StateNotHub, kata.StateUnavailable,
		kata.StateIncompatible, kata.StateUnauthenticated,
		kata.StateWrongProject, kata.StateReady:
		return true
	default:
		return false
	}
}

func printKataStatus(w io.Writer, st kata.Status) {
	fmt.Fprintf(w, "State:    %s\n", sanitizeTerminal(string(st.State)))
	fmt.Fprintf(w, "Project:  %s\n", sanitizeTerminal(st.Project))
	if st.InstanceUID != "" {
		fmt.Fprintf(w, "Instance: %s\n", sanitizeTerminal(st.InstanceUID))
	}
	if st.APISchemaVersion != "" {
		fmt.Fprintf(w, "API:      %s\n", sanitizeTerminal(st.APISchemaVersion))
	}
	if st.Message != "" {
		fmt.Fprintf(w, "Message:  %s\n", sanitizeTerminal(st.Message))
	}
}
