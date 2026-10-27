package servicehttp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/service"
)

func (b *httpBackend) SupportsFriction() bool { return b.friction }

func (b *httpBackend) FrictionDigest(
	ctx context.Context, date string,
) (*service.FrictionDigestView, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	if date == "" {
		list, err := api.GetAPIV1FrictionDigestsWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsRequestOptions{})
		if list == nil {
			return nil, err
		}
		if err := b.frictionError("friction digests", serviceResponseError(list.HTTPResponse, list.Body, err)); err != nil {
			return nil, err
		}
		var page struct {
			Digests []struct {
				Date string `json:"date"`
			} `json:"digests"`
		}
		if err := json.Unmarshal(list.Body, &page); err != nil {
			return nil, fmt.Errorf("decoding friction digests: %w", err)
		}
		if len(page.Digests) == 0 {
			return nil, service.ErrFrictionDigestNotFound
		}
		date = page.Digests[0].Date
	}
	detail, err := api.GetAPIV1FrictionDigestsDateWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsDateRequestOptions{
		PathParams: &apiclient.GetAPIV1FrictionDigestsDatePath{Date: url.PathEscape(date)},
	})
	if detail == nil {
		return nil, err
	}
	if err := b.frictionError("friction digest", serviceResponseError(detail.HTTPResponse, detail.Body, err)); err != nil {
		return nil, err
	}
	var body struct {
		Date     string         `json:"date"`
		Timezone string         `json:"timezone"`
		BuiltAt  time.Time      `json:"built_at"`
		Revision int            `json:"revision"`
		Summary  jsontext.Value `json:"summary"`
	}
	if err := json.Unmarshal(detail.Body, &body); err != nil {
		return nil, fmt.Errorf("decoding friction digest: %w", err)
	}
	summary, err := friction.CanonicalSummaryJSON(body.Summary)
	if err != nil {
		return nil, err
	}
	md, err := api.GetAPIV1FrictionDigestsDateMdWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsDateMdRequestOptions{
		PathParams: &apiclient.GetAPIV1FrictionDigestsDateMdPath{Date: url.PathEscape(date)},
	})
	if md == nil {
		return nil, err
	}
	if err := b.frictionError("friction digest markdown", serviceResponseError(md.HTTPResponse, md.Body, err)); err != nil {
		return nil, err
	}
	return &service.FrictionDigestView{
		Date: body.Date, Timezone: body.Timezone, BuiltAt: body.BuiltAt,
		Revision: body.Revision, Markdown: string(md.Body), Summary: summary,
	}, nil
}

func (b *httpBackend) FrictionDigests(
	ctx context.Context, from, to string,
) ([]service.FrictionDigestListItem, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	resp, err := api.GetAPIV1FrictionDigestsWithResponse(ctx, &apiclient.GetAPIV1FrictionDigestsRequestOptions{
		Query: &apiclient.GetAPIV1FrictionDigestsQuery{From: optionalString(from), To: optionalString(to)},
	})
	if resp == nil {
		return nil, err
	}
	if err := b.frictionError("friction digests", serviceResponseError(resp.HTTPResponse, resp.Body, err)); err != nil {
		return nil, err
	}
	var page struct {
		Digests []service.FrictionDigestListItem `json:"digests"`
	}
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return nil, fmt.Errorf("decoding friction digests: %w", err)
	}
	return page.Digests, nil
}

func (b *httpBackend) FrictionFindings(
	ctx context.Context, f service.FrictionFindingFilter,
) (*service.FrictionFindingPage, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1FrictionFindingsQuery{
		Date: optionalString(f.Date), Kind: optionalString(f.Kind),
		SessionID: optionalString(f.SessionID), Fingerprint: optionalString(f.Fingerprint),
		Cursor: optionalString(f.Cursor),
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	resp, err := api.GetAPIV1FrictionFindingsWithResponse(ctx, &apiclient.GetAPIV1FrictionFindingsRequestOptions{Query: q})
	if resp == nil {
		return nil, err
	}
	if err := b.frictionError("friction findings", serviceResponseError(resp.HTTPResponse, resp.Body, err)); err != nil {
		return nil, err
	}
	var page service.FrictionFindingPage
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return nil, fmt.Errorf("decoding friction findings: %w", err)
	}
	return &page, nil
}

func (b *httpBackend) FrictionPatterns(
	ctx context.Context, f service.FrictionPatternFilter,
) (*service.FrictionPatternPage, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1FrictionPatternsQuery{
		Kind: optionalString(f.Kind), Since: optionalString(f.Since), Cursor: optionalString(f.Cursor),
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	resp, err := api.GetAPIV1FrictionPatternsWithResponse(ctx, &apiclient.GetAPIV1FrictionPatternsRequestOptions{Query: q})
	if resp == nil {
		return nil, err
	}
	if err := b.frictionError("friction patterns", serviceResponseError(resp.HTTPResponse, resp.Body, err)); err != nil {
		return nil, err
	}
	var page service.FrictionPatternPage
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return nil, fmt.Errorf("decoding friction patterns: %w", err)
	}
	for i := range page.Patterns {
		p := &page.Patterns[i]
		p.LastSessionURL = b.sessionWebURL(p.LastSubjectID)
		if p.LastSessionURL != "" && p.LastOrdinal != nil {
			p.LastSessionURL += "?msg=" + strconv.Itoa(*p.LastOrdinal)
		}
	}
	return &page, nil
}

// optionalString maps an unset filter to an omitted query parameter.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (b *httpBackend) frictionError(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errHTTPNotFound):
		return service.ErrFrictionDigestNotFound
	case errors.Is(err, errHTTPNotImplemented):
		return fmt.Errorf("%s: daemon at %s: %w", op, b.baseURL, db.ErrReadOnly)
	default:
		return err
	}
}
