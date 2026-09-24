package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledgerstatus"
	"go.kenn.io/agentsview/internal/serdejson"
)

type ledgerQueryRequest struct {
	Since      string
	Subsystems []string
	Class      string
	Zone       string
	Limit      int
}

func newLedgerQueryCommand() *cobra.Command {
	var req ledgerQueryRequest
	var format string
	cmd := &cobra.Command{
		Use:          "query",
		Short:        "Show ledger events, newest first per zone",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				format = "json"
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if _, err := ledger.ParseSince(req.Since, time.Now()); err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}
			if req.Class != "" {
				if _, err := ledger.ParseClass(req.Class); err != nil {
					return fmt.Errorf("invalid --class value: %w", err)
				}
			}
			if req.Zone != "" {
				if _, err := ledgerZones(cfg, req.Zone); err != nil {
					return err
				}
			}
			results, err := runLedgerQuery(cmd.Context(), cfg, req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if format == "json" {
				b, err := ledger.FormatJSON(results)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(out, string(b))
				return err
			}
			_, err = io.WriteString(out, ledger.FormatText(results, req.Since, req.Subsystems))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Since, "since", "7d", `Time window: "24h", "7d", "4w" or "2026-04-01" (UTC midnight)`)
	f.StringArrayVar(&req.Subsystems, "subsystem", nil, "Subsystem glob (a trailing * matches a prefix); repeat to OR")
	f.StringVar(&req.Class, "class", "", "Event class (ingest, route, decision, state-change, claim, delivery, projection, health, approval, note-meta)")
	f.StringVar(&req.Zone, "zone", "", "Only this zone")
	f.IntVar(&req.Limit, "limit", 100, "Maximum events per zone")
	f.StringVar(&format, "format", "text", "Output format: text or json")
	f.Bool("json", false, "Emit JSON output (alias for --format json)")
	return cmd
}

// ledgerDaemon probes for a daemon without starting one; true means a
// writable daemon owns the archive and should take the request.
func ledgerDaemon(ctx context.Context, cfg config.Config) (transport, bool, error) {
	tr, err := detectTransportContext(ctx, cfg.DataDir, cfg.AuthToken, backgroundAutoStartReadyTimeout)
	if err != nil {
		return transport{}, false, fmt.Errorf("resolving archive transport: %w", err)
	}
	return tr, tr.Mode == transportHTTP && !tr.ReadOnly, nil
}

func runLedgerQuery(ctx context.Context, cfg config.Config, req ledgerQueryRequest) ([]ledger.ZoneEvents, error) {
	tr, daemon, err := ledgerDaemon(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if daemon {
		return requestLedgerQuery(ctx, tr, cfg.AuthToken, req)
	}
	database, err := openReadOnlyDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return queryLedgerLocal(ctx, cfg, database, req, time.Now())
}

type ledgerQueryStore interface {
	QueryLedger(ctx context.Context, q ledger.Query) ([]ledger.ZoneEvents, error)
}

func queryLedgerLocal(
	ctx context.Context, cfg config.Config, st ledgerQueryStore, req ledgerQueryRequest, now time.Time,
) ([]ledger.ZoneEvents, error) {
	since, err := ledger.ParseSince(req.Since, now)
	if err != nil {
		return nil, err
	}
	q := ledger.Query{Since: since, Subsystems: req.Subsystems, Zone: req.Zone, Limit: req.Limit}
	if req.Class != "" {
		c, err := ledger.ParseClass(req.Class)
		if err != nil {
			return nil, err
		}
		q.Class = &c
	}
	results, err := st.QueryLedger(ctx, q)
	if err != nil {
		return nil, err
	}
	return ledger.SortZoneEvents(results, cfg.Ledger.ZoneIDs()), nil
}

func ledgerAPI(tr transport, token string) (*apiclient.Client, error) {
	return apiclient.NewHTTPClient(tr.URL, token, &http.Client{Timeout: 0})
}

// ledgerAPIError turns a non-2xx response into the server's message.
func ledgerAPIError(action string, resp *http.Response, body []byte) error {
	if resp != nil && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	var apiErr struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &apiErr)
	if apiErr.Error == "" && resp != nil {
		apiErr.Error = resp.Status
	}
	return fmt.Errorf("%s: %s", action, apiErr.Error)
}

func requestLedgerQuery(ctx context.Context, tr transport, token string, req ledgerQueryRequest) ([]ledger.ZoneEvents, error) {
	api, err := ledgerAPI(tr, token)
	if err != nil {
		return nil, err
	}
	limit := int64(req.Limit)
	query := &apiclient.GetAPIV1LedgerEventsQuery{Since: &req.Since, Subsystem: req.Subsystems, Limit: &limit}
	if req.Class != "" {
		query.Class = &req.Class
	}
	if req.Zone != "" {
		query.Zone = &req.Zone
	}
	response, err := api.GetAPIV1LedgerEventsWithResponse(ctx, &apiclient.GetAPIV1LedgerEventsRequestOptions{Query: query})
	if response == nil {
		return nil, err
	}
	if apiErr := ledgerAPIError("ledger query", response.HTTPResponse, response.Body); apiErr != nil {
		return nil, apiErr
	}
	var body struct {
		Results []struct {
			Zone   string `json:"zone"`
			Events []struct {
				Serde string `json:"serde"`
			} `json:"events"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return nil, fmt.Errorf("decoding ledger query: %w", err)
	}
	out := make([]ledger.ZoneEvents, 0, len(body.Results))
	for _, z := range body.Results {
		ze := ledger.ZoneEvents{Zone: z.Zone}
		for _, e := range z.Events {
			ev, err := ledger.ParseEventJSON([]byte(e.Serde))
			if err != nil {
				return nil, err
			}
			ze.Events = append(ze.Events, ev)
		}
		out = append(out, ze)
	}
	return out, nil
}

// decodeLedgerPayload parses a --payload / API payload into the serdejson
// value model the ledger serializes.
func decodeLedgerPayload(raw string) (any, error) {
	return serdejson.Decode([]byte(raw))
}

type ledgerAppendResult struct {
	Zone      string   `json:"zone"`
	Source    string   `json:"source"`
	SourceSeq uint64   `json:"source_seq"`
	Checksum  uint32   `json:"checksum"`
	EventIDs  []string `json:"event_ids"`
}

func requestLedgerAppend(ctx context.Context, tr transport, token, zone string, e ledger.Event) (ledgerAppendResult, error) {
	api, err := ledgerAPI(tr, token)
	if err != nil {
		return ledgerAppendResult{}, err
	}
	ev := apiclient.LedgerAppendEvent{EventClass: string(e.EventClass), ActorRef: e.ActorRef, ObjectRef: e.ObjectRef}
	tier := string(e.PayloadTier)
	ev.PayloadTier = &tier
	if e.Payload != nil {
		b, err := serdejson.Compact(e.Payload)
		if err != nil {
			return ledgerAppendResult{}, err
		}
		value := jsontext.Value(b)
		ev.Payload = &value
	}
	body := apiclient.LedgerAppendRequest{Events: []apiclient.LedgerAppendEvent{ev}}
	if zone != "" {
		body.Zone = &zone
	}
	response, err := api.PostAPIV1LedgerEventsWithResponse(ctx, &apiclient.PostAPIV1LedgerEventsRequestOptions{Body: &body})
	if response == nil {
		return ledgerAppendResult{}, err
	}
	if apiErr := ledgerAPIError("ledger append", response.HTTPResponse, response.Body); apiErr != nil {
		return ledgerAppendResult{}, apiErr
	}
	var res ledgerAppendResult
	if err := json.Unmarshal(response.Body, &res); err != nil {
		return ledgerAppendResult{}, fmt.Errorf("decoding ledger append: %w", err)
	}
	return res, nil
}

func requestLedgerStatus(ctx context.Context, tr transport, token, zone string) ([]ledgerstatus.ZoneReport, bool, error) {
	api, err := ledgerAPI(tr, token)
	if err != nil {
		return nil, false, err
	}
	opts := &apiclient.GetAPIV1LedgerStatusRequestOptions{}
	if zone != "" {
		opts.Query = &apiclient.GetAPIV1LedgerStatusQuery{Zone: &zone}
	}
	response, err := api.GetAPIV1LedgerStatusWithResponse(ctx, opts)
	if response == nil {
		return nil, false, err
	}
	if apiErr := ledgerAPIError("ledger status", response.HTTPResponse, response.Body); apiErr != nil {
		return nil, false, apiErr
	}
	var body struct {
		Enabled bool                      `json:"enabled"`
		Zones   []ledgerstatus.ZoneReport `json:"zones"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return nil, false, fmt.Errorf("decoding ledger status: %w", err)
	}
	return body.Zones, body.Enabled, nil
}

type ledgerVerifyZone struct {
	Zone                string `json:"zone"`
	ledger.VerifyReport `json:",inline"`
}

func requestLedgerVerify(ctx context.Context, tr transport, token, zone string, full bool) ([]ledgerVerifyZone, error) {
	api, err := ledgerAPI(tr, token)
	if err != nil {
		return nil, err
	}
	body := apiclient.LedgerVerifyRequest{Full: &full}
	if zone != "" {
		body.Zone = &zone
	}
	response, err := api.PostAPIV1LedgerVerifyWithResponse(ctx, &apiclient.PostAPIV1LedgerVerifyRequestOptions{Body: &body})
	if response == nil {
		return nil, err
	}
	if apiErr := ledgerAPIError("ledger verify", response.HTTPResponse, response.Body); apiErr != nil {
		return nil, apiErr
	}
	var res struct {
		Zones []ledgerVerifyZone `json:"zones"`
	}
	if err := json.Unmarshal(response.Body, &res); err != nil {
		return nil, fmt.Errorf("decoding ledger verify: %w", err)
	}
	return res.Zones, nil
}
