package codexcloud

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// usageReplyLimit bounds one plan usage reply. A full request of 100 rows
// with a few groups each is far smaller.
const usageReplyLimit = 16 << 20

// UsageGroup is one usage root and every known descendant, sent as one
// group so sub-agent usage counts toward the root.
type UsageGroup struct {
	ThreadID    string
	CreatedAt   time.Time
	Descendants []string
}

// UsageResponse is a plan usage reply. Threads holds a row per root the
// report included; it may leave roots out.
type UsageResponse struct {
	DataAsOf time.Time
	Threads  []mirror.UsageRow
}

type usageRequestGroup struct {
	ThreadID    string   `json:"thread_id"`
	CreatedAt   string   `json:"created_at"`
	Descendants []string `json:"descendant_thread_ids"`
}

type usageReply struct {
	DataAsOf string            `json:"data_as_of"`
	Threads  []mirror.UsageRow `json:"threads"`
}

// QueryUsage asks OpenAI's plan usage report for groups. A refused request
// is an *HTTPError; a reply this version cannot read is ErrProtocolChange.
// No error carries the token.
func QueryUsage(
	ctx context.Context, hc *http.Client, endpoint *url.URL, login Login, version string, groups []UsageGroup,
) (UsageResponse, error) {
	if endpoint == nil {
		return UsageResponse{}, errors.New("codex-cloud: no plan usage endpoint")
	}
	if login.Token.Empty() {
		return UsageResponse{}, errors.New("codex-cloud: no ChatGPT access token")
	}
	body := struct {
		Threads []usageRequestGroup `json:"threads"`
	}{Threads: make([]usageRequestGroup, 0, len(groups))}
	for _, g := range groups {
		// encoding/json/v2 sends a nil slice as [], which the report expects.
		body.Threads = append(body.Threads, usageRequestGroup{
			ThreadID: g.ThreadID, CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339), Descendants: g.Descendants,
		})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return UsageResponse{}, fmt.Errorf("codex-cloud: encode plan usage request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return UsageResponse{}, fmt.Errorf("codex-cloud: plan usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+login.Token.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentsview/"+version)
	req.Header.Set("originator", "agentsview")
	if login.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", login.AccountID)
	}
	client := http.Client{}
	if hc != nil {
		client = *hc
	}
	// A redirect would carry the bearer header to a URL the endpoint check
	// never saw.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return UsageResponse{}, Redact(fmt.Errorf("codex-cloud: plan usage request to %s: %w", endpoint.Host, err), login.Token)
	}
	defer func() { _ = resp.Body.Close() }() // the reply is read in full below
	if resp.StatusCode != http.StatusOK {
		return UsageResponse{}, newHTTPError(resp, time.Now())
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, usageReplyLimit+1))
	if err != nil {
		return UsageResponse{}, Redact(fmt.Errorf("codex-cloud: read plan usage reply: %w", err), login.Token)
	}
	if len(raw) > usageReplyLimit {
		return UsageResponse{}, fmt.Errorf("%w: plan usage reply", ErrReplyTooLarge)
	}
	return decodeUsageReply(raw)
}

// decimalCredits matches a plain decimal number with an optional
// exponent. strconv.ParseFloat alone also accepts NaN, infinities, and
// hexadecimal floats, which would be stored and shown as credits.
var decimalCredits = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

func decodeUsageReply(raw []byte) (UsageResponse, error) {
	var reply usageReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return UsageResponse{}, fmt.Errorf("%w: unreadable plan usage reply: %w", ErrProtocolChange, err)
	}
	asOf, err := time.Parse(time.RFC3339, reply.DataAsOf)
	if err != nil {
		return UsageResponse{}, fmt.Errorf("%w: plan usage data_as_of %q: %w", ErrProtocolChange, reply.DataAsOf, err)
	}
	for _, row := range reply.Threads {
		if row.ThreadID == "" {
			return UsageResponse{}, fmt.Errorf("%w: plan usage row without thread_id", ErrProtocolChange)
		}
		credits := []*string{row.Credits}
		for _, g := range row.Groups {
			credits = append(credits, g.Credits)
		}
		for _, c := range credits {
			if c == nil {
				continue
			}
			// Credits stay strings; this only checks they are decimal
			// numbers, which may use exponent form such as 0E-10.
			if !decimalCredits.MatchString(*c) {
				return UsageResponse{}, fmt.Errorf("%w: plan usage credits %q are not a number", ErrProtocolChange, *c)
			}
		}
	}
	return UsageResponse{DataAsOf: asOf.UTC(), Threads: reply.Threads}, nil
}
