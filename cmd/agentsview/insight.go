package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type insightListResponse struct {
	Insights []db.Insight `json:"insights"`
}

// insightHTTPError keeps the status available to commands while preserving
// the server's normal daemon error text.
type insightHTTPError struct {
	status int
	err    error
}

func (e *insightHTTPError) Error() string { return e.err.Error() }

func (e *insightHTTPError) Unwrap() error { return e.err }

func newInsightCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "insight",
		Short:        "Generate and inspect stored Activity Insights",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	registerFormatFlags(cmd.PersistentFlags())
	cmd.PersistentFlags().String(
		"server", "", "Remote daemon URL for insight API requests",
	)
	cmd.PersistentFlags().String(
		"server-token-file", "",
		"File containing bearer token for explicit --server requests",
	)

	cmd.AddCommand(newInsightListCommand())
	cmd.AddCommand(newInsightGetCommand())
	cmd.AddCommand(newInsightGenerateCommand())
	return cmd
}

func newInsightListCommand() *cobra.Command {
	var insightType string
	var project string
	var dateFrom string
	var dateTo string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List stored insights",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tr, token, err := resolveInsightTransport(cmd)
			if err != nil {
				return err
			}
			query := url.Values{}
			if insightType != "" {
				query.Set("type", insightType)
			}
			if project != "" {
				query.Set("project", project)
			}
			if dateFrom != "" {
				query.Set("date_from", dateFrom)
			}
			if dateTo != "" {
				query.Set("date_to", dateTo)
			}
			resp, err := doInsightRequest(
				cmd.Context(), tr, token, http.MethodGet,
				"/api/v1/insights", query, nil,
			)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			var result insightListResponse
			if err := json.UnmarshalRead(resp.Body, &result); err != nil {
				return fmt.Errorf("decoding insight list: %w", err)
			}
			if result.Insights == nil {
				result.Insights = []db.Insight{}
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(
					jsontext.NewEncoder(cmd.OutOrStdout()), result,
				)
			}
			return printInsightList(cmd.OutOrStdout(), result.Insights)
		},
	}
	cmd.Flags().StringVar(&insightType, "type", "", "Filter by insight type")
	cmd.Flags().StringVar(&project, "project", "", "Filter by project")
	cmd.Flags().StringVar(&dateFrom, "date-from", "", "Filter from date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&dateTo, "date-to", "", "Filter through date (YYYY-MM-DD)")
	return cmd
}

func newInsightGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "get <id>",
		Short:        "Get one stored insight",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid insight ID %q: %w", args[0], err)
			}
			tr, token, err := resolveInsightTransport(cmd)
			if err != nil {
				return err
			}
			resp, err := doInsightRequest(
				cmd.Context(), tr, token, http.MethodGet,
				"/api/v1/insights/"+strconv.FormatInt(id, 10), nil, nil,
			)
			if err != nil {
				httpErr, hasHttpErr := errors.AsType[*insightHTTPError](err)
				if hasHttpErr &&
					httpErr.status == http.StatusNotFound {
					return fmt.Errorf("insight %d not found", id)
				}
				return err
			}
			defer resp.Body.Close()

			var result db.Insight
			if err := json.UnmarshalRead(resp.Body, &result); err != nil {
				return fmt.Errorf("decoding insight: %w", err)
			}
			return printInsight(cmd, &result)
		},
	}
}

type insightGenerateRequest struct {
	Type           string `json:"type"`
	DateFrom       string `json:"date_from"`
	DateTo         string `json:"date_to"`
	Project        string `json:"project,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Agent          string `json:"agent,omitempty"`
	AutomatedScope string `json:"automated_scope,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
}

func newInsightGenerateCommand() *cobra.Command {
	var req insightGenerateRequest
	cmd := &cobra.Command{
		Use:          "generate",
		Short:        "Generate and store a new insight via the daemon",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tr, token, err := resolveInsightTransport(cmd)
			if err != nil {
				return err
			}
			result, err := postInsightGenerate(
				cmd.Context(), tr, token, req, cmd.ErrOrStderr(),
			)
			if err != nil {
				return err
			}
			return printInsight(cmd, result)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(
		&req.Type, "type", "daily_activity",
		"Insight type: daily_activity or agent_analysis",
	)
	flags.StringVar(&req.DateFrom, "date-from", "", "Start date (YYYY-MM-DD)")
	flags.StringVar(&req.DateTo, "date-to", "", "End date (YYYY-MM-DD)")
	flags.StringVar(&req.Project, "project", "", "Project to include")
	flags.StringVar(&req.Prompt, "prompt", "", "Optional focus prompt")
	flags.StringVar(&req.SessionID, "session-id", "", "Session ID for agent analysis")
	flags.StringVar(&req.Agent, "agent", "", "Agent used by the server")
	flags.StringVar(
		&req.AutomatedScope, "automated-scope", "",
		"Session scope: human, all, or automated",
	)
	flags.StringVar(
		&req.Timezone, "timezone", "",
		"IANA timezone for the date range (empty means UTC)",
	)
	return cmd
}

func resolveInsightTransport(
	cmd *cobra.Command,
) (transport, string, error) {
	remote, _ := cmd.Flags().GetString("server")
	remote = strings.TrimSpace(remote)
	if remote != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return transport{}, "", err
		}
		return transport{
			Mode: transportHTTP,
			URL:  remote,
		}, token, nil
	}

	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return transport{}, "", fmt.Errorf("loading config: %w", err)
	}
	tr, err := ensureTransportContext(
		cmd.Context(), &cfg, transportIntentRead, 0,
	)
	if err != nil {
		return transport{}, "", err
	}
	if tr.Mode != transportHTTP || strings.TrimSpace(tr.URL) == "" {
		return transport{}, "", errors.New(
			"insight commands require a running daemon; start one or pass --server",
		)
	}
	return tr, cfg.AuthToken, nil
}

func doInsightRequest(
	ctx context.Context,
	tr transport,
	token, method, path string,
	query url.Values,
	body io.Reader,
) (*http.Response, error) {
	baseURL := strings.TrimSuffix(tr.URL, "/")
	if baseURL == "" {
		return nil, errors.New("insight server URL is empty")
	}
	baseURL = insightRequestBaseURL(baseURL)
	endpoint := baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if origin := daemonOriginURL(tr.URL); origin != "" {
			req.Header.Set("Origin", origin)
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &insightHTTPError{
			status: resp.StatusCode,
			err:    errors.New(daemonErrorMessage(resp.StatusCode, responseBody)),
		}
	}
	return resp, nil
}

func insightRequestBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimSuffix(raw, "/")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return strings.TrimSuffix(parsed.String(), "/")
}

func postInsightGenerate(
	ctx context.Context,
	tr transport,
	token string,
	req insightGenerateRequest,
	progress io.Writer,
) (*db.Insight, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding insight request: %w", err)
	}
	resp, err := doInsightRequest(
		ctx, tr, token, http.MethodPost,
		"/api/v1/insights/generate", nil, bytes.NewReader(data),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return consumeInsightGenerateSSE(resp.Body, progress)
}

func consumeInsightGenerateSSE(
	r io.Reader, progress io.Writer,
) (*db.Insight, error) {
	reader := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data strings.Builder
	var result *db.Insight

	dispatch := func() error {
		if data.Len() == 0 {
			switch event {
			case "status", "log", "error", "done":
				return fmt.Errorf("insight %s event has no data", event)
			}
			return nil
		}
		raw := []byte(data.String())
		switch event {
		case "status":
			var status struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				return fmt.Errorf("decoding insight status event: %w", err)
			}
			if strings.TrimSpace(status.Phase) == "" {
				return errors.New("insight status event missing phase")
			}
			if progress != nil {
				_, _ = fmt.Fprintln(progress, sanitizeTerminal(status.Phase))
			}
		case "log":
			var logEvent struct {
				Stream string `json:"stream"`
				Line   string `json:"line"`
			}
			if err := json.Unmarshal(raw, &logEvent); err != nil {
				return fmt.Errorf("decoding insight log event: %w", err)
			}
			if progress != nil && logEvent.Line != "" {
				_, _ = fmt.Fprintln(progress, sanitizeTerminal(logEvent.Line))
			}
		case "error":
			var errorEvent struct {
				Message string `json:"message"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(raw, &errorEvent); err != nil {
				return fmt.Errorf("decoding insight error event: %w", err)
			}
			message := strings.TrimSpace(errorEvent.Message)
			if message == "" {
				message = strings.TrimSpace(errorEvent.Error)
			}
			if message == "" {
				message = strings.TrimSpace(data.String())
			}
			if message == "" {
				message = "insight generation failed"
			}
			return errors.New(message)
		case "done":
			var saved db.Insight
			if err := json.Unmarshal(raw, &saved); err != nil {
				return fmt.Errorf("decoding insight done event: %w", err)
			}
			if saved.ID <= 0 {
				return errors.New("insight done event missing saved insight")
			}
			result = &saved
		}
		return nil
	}

	resetFrame := func() {
		event = ""
		data.Reset()
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf(
				"reading insight generation stream: %w", readErr,
			)
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return nil, err
			}
			resetFrame()
		case strings.HasPrefix(line, ":"):
			// SSE comments carry no event data.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimPrefix(line, "event:")
			event = strings.TrimPrefix(event, " ")
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := dispatch(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("insight generation stream missing done event")
	}
	return result, nil
}

func printInsightList(w io.Writer, insights []db.Insight) error {
	if len(insights) == 0 {
		_, err := fmt.Fprintln(w, "(no insights)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTYPE\tRANGE\tPROJECT\tAGENT\tCREATED")
	for _, insight := range insights {
		project := "-"
		if insight.Project != nil && *insight.Project != "" {
			project = *insight.Project
		}
		rangeText := insight.DateFrom + ".." + insight.DateTo
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			insight.ID,
			sanitizeTerminal(insight.Type),
			sanitizeTerminal(rangeText),
			sanitizeTerminal(project),
			sanitizeTerminal(insight.Agent),
			sanitizeTerminal(insight.CreatedAt),
		)
	}
	return tw.Flush()
}

func printInsight(cmd *cobra.Command, insight *db.Insight) error {
	if outputFormat(cmd) == "json" {
		return json.MarshalEncode(
			jsontext.NewEncoder(cmd.OutOrStdout()), insight,
		)
	}
	return printInsightHuman(cmd.OutOrStdout(), insight)
}

func printInsightHuman(w io.Writer, insight *db.Insight) error {
	project := "-"
	if insight.Project != nil && *insight.Project != "" {
		project = *insight.Project
	}
	fmt.Fprintf(w, "ID:       %d\n", insight.ID)
	fmt.Fprintf(w, "Type:     %s\n", sanitizeTerminal(insight.Type))
	fmt.Fprintf(w, "Range:    %s..%s\n",
		sanitizeTerminal(insight.DateFrom), sanitizeTerminal(insight.DateTo))
	fmt.Fprintf(w, "Project:  %s\n", sanitizeTerminal(project))
	fmt.Fprintf(w, "Agent:    %s\n", sanitizeTerminal(insight.Agent))
	if insight.Model != nil && *insight.Model != "" {
		fmt.Fprintf(w, "Model:    %s\n", sanitizeTerminal(*insight.Model))
	}
	if insight.Prompt != nil && *insight.Prompt != "" {
		fmt.Fprintf(w, "Prompt:   %s\n", sanitizeTerminal(*insight.Prompt))
	}
	fmt.Fprintf(w, "Created:  %s\n", sanitizeTerminal(insight.CreatedAt))
	fmt.Fprintln(w)
	_, err := io.WriteString(w, sanitizeTerminal(insight.Content))
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}
