package servicehttp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/service"
)

// frictionAPIVersion is the first daemon API that serves /api/v1/friction.
const frictionAPIVersion = 11

func (b *httpBackend) SupportsFriction() bool { return b.friction }

func (b *httpBackend) browserPath(path string) string {
	base, err := url.Parse(b.browserURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return ""
	}
	base.User = nil
	base.RawQuery, base.ForceQuery, base.Fragment, base.RawFragment = "", false, "", ""
	return strings.TrimRight(base.String(), "/") + path
}

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
		WebURL: b.browserPath("/friction/" + url.PathEscape(body.Date)),
	}, nil
}

func (b *httpBackend) FrictionPatterns(
	ctx context.Context, f service.FrictionPatternFilter,
) ([]service.FrictionPatternView, error) {
	api, err := b.apiClient(b.client)
	if err != nil {
		return nil, err
	}
	q := &apiclient.GetAPIV1FrictionPatternsQuery{}
	if f.Kind != "" {
		q.Kind = new(f.Kind)
	}
	if f.Since != "" {
		q.Since = new(f.Since)
	}
	if f.Limit > 0 {
		q.Limit = new(int64(f.Limit))
	}
	if f.Linked != nil {
		state := "unlinked"
		if *f.Linked {
			state = "linked"
		}
		q.LinkState = new(state)
	}
	resp, err := api.GetAPIV1FrictionPatternsWithResponse(ctx, &apiclient.GetAPIV1FrictionPatternsRequestOptions{Query: q})
	if resp == nil {
		return nil, err
	}
	if err := b.frictionError("friction patterns", serviceResponseError(resp.HTTPResponse, resp.Body, err)); err != nil {
		return nil, err
	}
	var page struct {
		Patterns []db.FrictionPattern `json:"patterns"`
	}
	if err := json.Unmarshal(resp.Body, &page); err != nil {
		return nil, fmt.Errorf("decoding friction patterns: %w", err)
	}
	return service.FrictionPatternViews(page.Patterns, func(id string, ordinal *int) string {
		u := b.sessionWebURL(id)
		if u == "" || ordinal == nil {
			return u
		}
		return u + "?msg=" + strconv.Itoa(*ordinal)
	}), nil
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
