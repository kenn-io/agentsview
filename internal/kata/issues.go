package kata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/oklog/ulid/v2"

	kg "go.kenn.io/kata/pkg/client/generated"
)

// Issue is the live Kata state agentsview reads. It is never persisted.
type Issue struct {
	UID, ShortID, QualifiedID, Title, Status, ClosedReason, WebURL string
	Labels                                                         []string
	Metadata                                                       map[string]any
	Priority                                                       *int
}

type CreateIssue struct {
	Title, Body string
	Priority    int
	Labels      []string
	Metadata    map[string]any
	ForceNew    bool
}

type CreateResult struct {
	Issue  Issue
	Reused bool
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intPtr(p *int64) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func (c *Client) routeRef(ref string) (string, error) {
	if ref == "" || strings.TrimSpace(ref) != ref {
		return "", errors.New("kata: invalid issue ref")
	}
	if shortID, ok := strings.CutPrefix(ref, c.project+"#"); ok && c.project != "" && validShortID(shortID) {
		// A qualified ref is already scoped to this project. Sending the short ID
		// keeps project names with URL delimiters out of the route.
		return shortID, nil
	}
	if strings.ContainsAny(ref, "/?#") {
		return "", errors.New("kata: invalid issue ref")
	}
	return ref, nil
}

// validShortID mirrors Kata's public short_id syntax: a 4..26 character,
// lowercase Crockford base32 suffix. It does not prove that the issue exists.
func validShortID(shortID string) bool {
	if len(shortID) < 4 || len(shortID) > 26 {
		return false
	}
	for i := range len(shortID) {
		if !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", rune(shortID[i])) {
			return false
		}
	}
	return true
}

func (c *Client) qualifiedIssueRef(shortID string) (string, error) {
	qualifiedID := c.project + "#" + shortID
	if c.project == "" || !validShortID(shortID) {
		return "", fmt.Errorf("%w: issue has invalid short_id", ErrInvalidResponse)
	}
	return qualifiedID, nil
}

// isULID reports whether ref is a 26-character Crockford base32 UID.
func isULID(ref string) bool {
	if len(ref) != 26 {
		return false
	}
	for _, r := range ref {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}

func validateIssueUID(uid string) error {
	if _, err := ulid.ParseStrict(uid); err != nil {
		return fmt.Errorf("%w: issue has invalid uid", ErrInvalidResponse)
	}
	return nil
}

func (c *Client) issueFromOut(o kg.IssueOut) (Issue, error) {
	if o.UID == "" || o.ShortID == "" || o.Status == "" {
		return Issue{}, fmt.Errorf("%w: issue has no uid/short_id/status", ErrInvalidResponse)
	}
	if err := validateIssueUID(o.UID); err != nil {
		return Issue{}, err
	}
	if err := c.validateIssueProject(o.ProjectID, o.ProjectUID); err != nil {
		return Issue{}, err
	}
	qualifiedID, err := c.qualifiedIssueRef(o.ShortID)
	if err != nil {
		return Issue{}, err
	}
	if o.QualifiedID != qualifiedID {
		return Issue{}, fmt.Errorf("%w: issue qualified_id does not match its project and short_id", ErrInvalidResponse)
	}
	return Issue{
		UID: o.UID, ShortID: o.ShortID, QualifiedID: qualifiedID, Title: o.Title, Status: o.Status,
		ClosedReason: deref(o.ClosedReason), WebURL: deref(o.WebURL), Labels: o.Labels, Metadata: o.Metadata, Priority: intPtr(o.Priority),
	}, nil
}

func (c *Client) validateIssueProject(id int64, uid *string) error {
	c.mu.RLock()
	projectID, projectUID := c.projectID, c.projectUID
	c.mu.RUnlock()
	if projectID == 0 || id != projectID || (uid != nil && *uid != projectUID) {
		return fmt.Errorf("%w: issue belongs to a different project", ErrInvalidResponse)
	}
	return nil
}

func (c *Client) issueFromShow(s kg.ShowIssueResponseBody) (Issue, error) {
	o := s.Issue
	if o.UID == "" || o.ShortID == "" || o.Status == "" {
		return Issue{}, fmt.Errorf("%w: issue has no uid/short_id/status", ErrInvalidResponse)
	}
	if err := validateIssueUID(o.UID); err != nil {
		return Issue{}, err
	}
	if err := c.validateIssueProject(o.ProjectID, o.ProjectUID); err != nil {
		return Issue{}, err
	}
	qualifiedID, err := c.qualifiedIssueRef(o.ShortID)
	if err != nil {
		return Issue{}, err
	}
	labels := make([]string, 0, len(s.Labels))
	for _, l := range s.Labels {
		labels = append(labels, l.Label)
	}
	return Issue{
		UID: o.UID, ShortID: o.ShortID, QualifiedID: qualifiedID, Title: o.Title, Status: o.Status,
		ClosedReason: deref(o.ClosedReason), WebURL: deref(s.WebURL), Labels: labels, Metadata: o.Metadata, Priority: intPtr(o.Priority),
	}, nil
}

// listEnvelope keeps "issues" required: a payload without it is drift.
type listEnvelope struct {
	Issues *[]kg.IssueOut `json:"issues"`
}

// FindByMetadata lists issues in the project whose metadata key equals value,
// in every status. Drift is an error, never an empty list.
func (c *Client) FindByMetadata(ctx context.Context, key, value string) ([]Issue, error) {
	q := url.Values{}
	q.Set("meta", key+"="+value)
	var env listEnvelope
	err := c.withProject(ctx, func(pid int64) error {
		env = listEnvelope{}
		return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues", pid), q, nil, nil, &env)
	})
	if err != nil {
		return nil, err
	}
	if env.Issues == nil {
		return nil, fmt.Errorf("%w: issues collection missing", ErrInvalidResponse)
	}
	out := make([]Issue, 0, len(*env.Issues))
	for _, o := range *env.Issues {
		is, err := c.issueFromOut(o)
		if err != nil {
			return nil, err
		}
		matched, ok := is.Metadata[key].(string)
		if !ok || matched != value {
			return nil, fmt.Errorf("%w: issue metadata does not match requested filter", ErrInvalidResponse)
		}
		out = append(out, is)
	}
	return out, nil
}

// CreateIssue files one issue with an Idempotency-Key and re-reads it for
// web_url. A failed re-read falls back to the create response.
func (c *Client) CreateIssue(ctx context.Context, idempotencyKey string, req CreateIssue) (CreateResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return CreateResult{}, ErrIdempotencyKeyRequired
	}
	if strings.TrimSpace(req.Title) == "" {
		return CreateResult{}, errors.New("kata: issue title is required")
	}
	if req.Priority < 0 || req.Priority > 4 {
		return CreateResult{}, fmt.Errorf("kata: priority %d outside 0..4", req.Priority)
	}
	prio := int64(req.Priority)
	body := req.Body
	payload := kg.CreateIssueRequestBody{Actor: strPtr(c.actor), Title: req.Title, Body: &body, Priority: &prio, Labels: req.Labels, Metadata: req.Metadata}
	if req.ForceNew {
		t := true
		payload.ForceNew = &t
	}
	var resp kg.MutationResponseBody
	err := c.withProject(ctx, func(pid int64) error {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues", pid), nil,
			http.Header{"Idempotency-Key": {idempotencyKey}}, payload, &resp)
	})
	if err != nil {
		return CreateResult{}, err
	}
	if resp.Issue.UID == "" || resp.Issue.ShortID == "" {
		return CreateResult{}, fmt.Errorf("%w: create response has no issue uid/short_id", ErrInvalidResponse)
	}
	if err := validateIssueUID(resp.Issue.UID); err != nil {
		return CreateResult{}, err
	}
	if err := c.validateIssueProject(resp.Issue.ProjectID, resp.Issue.ProjectUID); err != nil {
		return CreateResult{}, err
	}
	reused := resp.Reused != nil && *resp.Reused
	full, err := c.GetIssue(ctx, resp.Issue.UID)
	qualifiedID, refErr := c.qualifiedIssueRef(resp.Issue.ShortID)
	if refErr != nil {
		return CreateResult{}, refErr
	}
	if err != nil {
		if resp.Issue.Status == "" || resp.Issue.Title == "" {
			return CreateResult{}, fmt.Errorf("%w: create response has no issue status/title", ErrInvalidResponse)
		}
		full = Issue{
			UID: resp.Issue.UID, ShortID: resp.Issue.ShortID, QualifiedID: qualifiedID,
			Title: resp.Issue.Title, Status: resp.Issue.Status, Metadata: resp.Issue.Metadata, Priority: intPtr(resp.Issue.Priority),
		}
	}
	return CreateResult{Issue: full, Reused: reused}, nil
}

// GetIssue reads one issue. A ULID uses the cross-project route; a short or
// qualified id uses the configured project.
func (c *Client) GetIssue(ctx context.Context, ref string) (Issue, error) {
	routeRef, err := c.routeRef(ref)
	if err != nil {
		return Issue{}, err
	}
	var resp kg.ShowIssueResponseBody
	if isULID(ref) {
		err = c.do(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(routeRef), nil, nil, nil, &resp)
	} else {
		err = c.withProject(ctx, func(pid int64) error {
			return c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s", pid, url.PathEscape(routeRef)), nil, nil, nil, &resp)
		})
	}
	if err != nil {
		return Issue{}, err
	}
	issue, err := c.issueFromShow(resp)
	if err != nil {
		return Issue{}, err
	}
	if err := c.validateIssueRef(ref, issue.UID, issue.ShortID); err != nil {
		return Issue{}, err
	}
	return issue, nil
}

func (c *Client) issueAction(ctx context.Context, ref, suffix string, headers http.Header, body, out any) error {
	routeRef, err := c.routeRef(ref)
	if err != nil {
		return err
	}
	return c.withProject(ctx, func(pid int64) error {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/%s", pid, url.PathEscape(routeRef), suffix), nil, headers, body, out)
	})
}

func (c *Client) validateActionIssue(ref string, issue kg.Issue) error {
	if issue.UID == "" || issue.ShortID == "" || issue.Status == "" {
		return fmt.Errorf("%w: action response has no issue identity or status", ErrInvalidResponse)
	}
	if err := validateIssueUID(issue.UID); err != nil {
		return err
	}
	if err := c.validateIssueProject(issue.ProjectID, issue.ProjectUID); err != nil {
		return err
	}
	if _, err := c.qualifiedIssueRef(issue.ShortID); err != nil {
		return err
	}
	return c.validateIssueRef(ref, issue.UID, issue.ShortID)
}

func (c *Client) validateIssueRef(ref, uid, shortID string) error {
	if isULID(ref) {
		if uid != ref {
			return fmt.Errorf("%w: response refers to another issue", ErrInvalidResponse)
		}
	} else if ref != shortID && ref != c.project+"#"+shortID {
		return fmt.Errorf("%w: response refers to another issue", ErrInvalidResponse)
	}
	return nil
}

// Reopen reopens a closed issue. An open issue returns changed=false.
func (c *Client) Reopen(ctx context.Context, ref string) (bool, error) {
	var resp kg.MutationResponseBody
	if err := c.issueAction(ctx, ref, "actions/reopen", nil, kg.ActionRequestBody{Actor: strPtr(c.actor)}, &resp); err != nil {
		return false, err
	}
	if err := c.validateActionIssue(ref, resp.Issue); err != nil {
		return false, err
	}
	if resp.Issue.Status != "open" {
		return false, fmt.Errorf("%w: reopen response is not open", ErrInvalidResponse)
	}
	return resp.Changed, nil
}

// Comment adds a comment; a non-empty key makes retries idempotent.
func (c *Client) Comment(ctx context.Context, ref, idempotencyKey, body string) error {
	var headers http.Header
	if idempotencyKey != "" {
		headers = http.Header{"Idempotency-Key": {idempotencyKey}}
	}
	var resp kg.CommentResponseBody
	if err := c.issueAction(ctx, ref, "comments", headers, kg.CommentRequestBody{Actor: strPtr(c.actor), Body: body}, &resp); err != nil {
		return err
	}
	if err := c.validateActionIssue(ref, resp.Issue); err != nil {
		return err
	}
	if resp.Comment.UID == "" || resp.Comment.Body != body || resp.Issue.ID == 0 || resp.Comment.IssueID != resp.Issue.ID {
		return fmt.Errorf("%w: comment acknowledgement is missing or mismatched", ErrInvalidResponse)
	}
	return nil
}

// AddLabel adds a label; an existing label is a no-op in Kata.
func (c *Client) AddLabel(ctx context.Context, ref, label string) error {
	var resp kg.AddLabelResponseBody
	if err := c.issueAction(ctx, ref, "labels", nil, kg.AddLabelRequestBody{Actor: strPtr(c.actor), Label: label}, &resp); err != nil {
		return err
	}
	if err := c.validateActionIssue(ref, resp.Issue); err != nil {
		return err
	}
	if resp.Label.Label != label || resp.Issue.ID == 0 || resp.Label.IssueID != resp.Issue.ID {
		return fmt.Errorf("%w: label acknowledgement is missing or mismatched", ErrInvalidResponse)
	}
	return nil
}
