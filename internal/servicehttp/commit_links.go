package servicehttp

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/requestsign"
	"go.kenn.io/agentsview/internal/service"
)

func (b *httpBackend) CommitSessions(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
	if err := db.ValidateCommitSHAs(shas); err != nil {
		return nil, err
	}
	if len(shas) == 0 {
		return map[string][]service.CommitSessionLink{}, nil
	}
	body, err := json.Marshal(struct {
		SHAs []string `json:"shas"`
	}{shas})
	if err != nil {
		return nil, err
	}
	var out service.CommitSessionsResponse
	if err := b.commitJSON(ctx, http.MethodPost, "/api/v1/commits/sessions", body, &out); err != nil {
		return nil, err
	}
	if out.Commits == nil {
		return nil, errors.New("invalid commit lookup response")
	}
	total := 0
	for _, sha := range shas {
		rows, ok := out.Commits[sha]
		if !ok || rows == nil {
			return nil, errors.New("commit lookup response omitted a requested SHA")
		}
	}
	for key, rows := range out.Commits {
		total += len(rows)
		if total > db.MaxCommitLinks {
			return nil, service.ErrCommitLookupTooLarge
		}
		for i := range rows {
			b.commitWebURL(&rows[i])
		}
		out.Commits[key] = rows
	}
	return out.Commits, nil
}

func (b *httpBackend) SessionCommits(ctx context.Context, id string) ([]service.CommitSessionLink, error) {
	if strings.TrimSpace(id) == "" || len(id) > 1024 {
		return nil, service.ErrInvalidCommitQuery
	}
	var out service.SessionCommitsResponse
	if err := b.commitJSON(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(id)+"/commits", nil, &out); err != nil {
		return nil, err
	}
	if out.Commits == nil {
		return nil, errors.New("invalid session commits response")
	}
	if len(out.Commits) > db.MaxCommitLinks {
		return nil, service.ErrCommitLookupTooLarge
	}
	for i := range out.Commits {
		b.commitWebURL(&out.Commits[i])
	}
	return out.Commits, nil
}

func (b *httpBackend) commitWebURL(link *service.CommitSessionLink) {
	if link.Session == nil || link.Gone {
		link.Session = nil
		link.Gone = true
		link.Agent = ""
		link.Project = ""
		link.SessionStartedAt = ""
		link.WebURL = ""
		link.Turn = nil
		return
	}
	link.WebURL = b.sessionWebURL(link.SessionID)
}

// commitJSON bounds bytes and collection expansion before JSON decoding. It
// preserves the generated client's signing, authentication and freshness seam.
func (b *httpBackend) commitJSON(ctx context.Context, method, path string, body []byte, out any) error {
	base, err := url.Parse(b.baseURL)
	if err != nil {
		return err
	}
	relative, err := url.Parse(path)
	if err != nil {
		return err
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/") + relative.Path
	base.RawPath = prefix + relative.EscapedPath()
	base.RawQuery = ""
	base.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, method, base.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if method != http.MethodGet {
		origin := *base
		origin.Path = ""
		origin.RawPath = ""
		req.Header.Set("Origin", origin.String())
	}
	client, err := requestsign.ClientFromEnvironment(b.baseURL, b.client)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	apiclient.ObserveArchiveFreshness(ctx, response)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, db.MaxCommitResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > db.MaxCommitResponseBytes {
		return service.ErrCommitLookupTooLarge
	}
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return service.ErrCommitIndexUnavailable
	case http.StatusUnprocessableEntity:
		return service.ErrCommitLookupTooLarge
	case http.StatusBadRequest:
		return service.ErrInvalidCommitQuery
	case http.StatusOK:
		if err := checkCommitJSONExpansion(data); err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	default:
		return &httpStatusError{method: method, path: path, statusCode: response.StatusCode, body: data}
	}
}

// A tiny JSON body can contain millions of empty array entries. Counting tokens
// before unmarshalling prevents that body from expanding into millions of cards.
func checkCommitJSONExpansion(data []byte) error {
	type frame struct {
		kind     jsontext.Kind
		names    int
		wantName bool
	}
	stack := []frame{}
	elements, objects := 0, 0
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		kind := token.Kind()
		if kind == '}' || kind == ']' {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.kind == '[' {
				elements++
				if elements > db.MaxCommitLinks {
					return service.ErrCommitLookupTooLarge
				}
			}
			if top.kind == '{' {
				if top.wantName {
					top.names++
					if top.names > db.MaxCommitSHAs {
						return service.ErrCommitLookupTooLarge
					}
					top.wantName = false
					continue
				}
				top.wantName = true
			}
		}
		if kind == '{' || kind == '[' {
			if len(stack) >= 64 {
				return service.ErrCommitLookupTooLarge
			}
			if kind == '{' {
				objects++
				if objects > db.MaxCommitLinks*3+db.MaxCommitSHAs+2 {
					return service.ErrCommitLookupTooLarge
				}
			}
			stack = append(stack, frame{kind: kind, wantName: kind == '{'})
		}
	}
}
