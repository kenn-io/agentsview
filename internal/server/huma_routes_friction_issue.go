package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
)

// registerFrictionIssueRoutes serves a pattern's Kata issue. Each call talks
// to Kata and can take several request timeouts, so none uses the short
// default route timeout.
func (s *Server) registerFrictionIssueRoutes(group *huma.Group) {
	const path = "/friction/patterns/{fingerprint}/issue"
	registerRoute(group, http.MethodPost, path, "File a friction pattern to Kata", s.humaFileFrictionPattern)
	registerRoute(group, http.MethodPut, path, "Link a friction pattern to a Kata issue", s.humaLinkFrictionPattern)
	registerRoute(group, http.MethodDelete, path, "Remove a friction pattern's Kata issue link", s.humaUnlinkFrictionPattern)
}

type frictionIssueItem struct {
	QualifiedID string    `json:"qualified_id" doc:"Kata issue reference, such as agentsview#f001"`
	WebURL      string    `json:"web_url,omitempty" doc:"Kata issue page, when Kata reports one"`
	IssueUID    string    `json:"issue_uid"`
	Source      string    `json:"source" doc:"created, found (Kata already held it) or manual"`
	LinkedAt    time.Time `json:"linked_at"`
}

func frictionIssueItemFrom(l *db.FrictionIssueLink) *frictionIssueItem {
	if l == nil {
		return nil
	}
	return &frictionIssueItem{
		QualifiedID: l.QualifiedID, WebURL: l.WebURL, IssueUID: l.IssueUID,
		Source: l.Source, LinkedAt: l.LinkedAt.UTC(),
	}
}

// frictionFilingItem says why automatic filing has not linked a pattern.
type frictionFilingItem struct {
	State         string     `json:"state" enum:"retry,needs_person,paused" doc:"retry: waiting for the next attempt; needs_person: file or link it by hand; paused: a person removed its link"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

func frictionFilingItemFrom(st *db.FrictionFilingState) *frictionFilingItem {
	if st == nil {
		return nil
	}
	item := &frictionFilingItem{State: st.State, Attempts: st.Attempts, LastError: st.LastError}
	if st.NextAttemptAt != nil {
		next := st.NextAttemptAt.UTC()
		item.NextAttemptAt = &next
	}
	return item
}

type frictionPatternPathInput struct {
	Fingerprint string `path:"fingerprint" required:"true" doc:"Pattern fingerprint (fl1:<sha256>)"`
}

type frictionFileInput struct {
	Fingerprint string `path:"fingerprint" required:"true" doc:"Pattern fingerprint (fl1:<sha256>)"`
	Body        struct {
		DryRun bool `json:"dry_run,omitempty" doc:"Return the issue that would be filed without contacting Kata"`
	}
}

type frictionIssuePreview struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	Priority int            `json:"priority"`
	Labels   []string       `json:"labels"`
	Metadata map[string]any `json:"metadata"`
}

type frictionFileResponse struct {
	// Issue is the pattern's linked issue; absent for a dry run.
	Issue *frictionIssueItem `json:"issue,omitempty"`
	// Created is true when this call created the Kata issue.
	Created bool `json:"created"`
	// Preview is the issue a dry run would file.
	Preview *frictionIssuePreview `json:"preview,omitempty"`
}

type frictionLinkInput struct {
	Fingerprint string `path:"fingerprint" required:"true" doc:"Pattern fingerprint (fl1:<sha256>)"`
	Body        struct {
		Issue string `json:"issue" required:"true" minLength:"1" doc:"Kata issue: qualified ID (agentsview#f001), short ID (f001) or UID"`
	}
}

type frictionLinkResponse struct {
	Issue *frictionIssueItem `json:"issue"`
}

func (s *Server) humaFileFrictionPattern(
	ctx context.Context, in *frictionFileInput,
) (*jsonOutput[frictionFileResponse], error) {
	f, err := s.frictionFiler(ctx)
	if err != nil {
		return nil, err
	}
	if in.Body.DryRun {
		req, err := f.Preview(ctx, in.Fingerprint)
		if err != nil {
			return nil, frictionIssueError(err)
		}
		return &jsonOutput[frictionFileResponse]{Body: frictionFileResponse{Preview: &frictionIssuePreview{
			Title: req.Title, Body: req.Body, Priority: req.Priority, Labels: req.Labels, Metadata: req.Metadata,
		}}}, nil
	}
	res, err := f.File(ctx, in.Fingerprint)
	if err != nil {
		return nil, frictionIssueError(err)
	}
	return &jsonOutput[frictionFileResponse]{Body: frictionFileResponse{
		Issue: frictionIssueItemFrom(&res.Link), Created: res.Created,
	}}, nil
}

func (s *Server) humaLinkFrictionPattern(
	ctx context.Context, in *frictionLinkInput,
) (*jsonOutput[frictionLinkResponse], error) {
	f, err := s.frictionFiler(ctx)
	if err != nil {
		return nil, err
	}
	link, err := f.Link(ctx, in.Fingerprint, in.Body.Issue)
	if err != nil {
		return nil, frictionIssueError(err)
	}
	return &jsonOutput[frictionLinkResponse]{Body: frictionLinkResponse{Issue: frictionIssueItemFrom(&link)}}, nil
}

func (s *Server) humaUnlinkFrictionPattern(
	ctx context.Context, in *frictionPatternPathInput,
) (*noContentOutput, error) {
	f, err := s.frictionFiler(ctx)
	if err != nil {
		return nil, err
	}
	if err := f.Unlink(ctx, in.Fingerprint); err != nil {
		return nil, frictionIssueError(err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// frictionFiler checks that this caller may change Kata links here: the
// caller is allowed to start writes, this process is the filing hub, and the
// store keeps links.
func (s *Server) frictionFiler(ctx context.Context) (*filing.Filer, error) {
	if !s.frictionRunAllowed(ctx) {
		return nil, apiError(http.StatusForbidden, "friction filing requires auth or a localhost request")
	}
	if err := filing.CheckHub(s.kataConn().Config()); err != nil {
		return nil, apiError(http.StatusConflict, err.Error())
	}
	store, ok := s.db.(filing.Store)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "this backend does not store friction issue links")
	}
	return &filing.Filer{Kata: s.kataConn(), Store: store, PublicURL: s.frictionPublicURL()}, nil
}

func frictionIssueError(err error) error {
	if amb, ok := errors.AsType[*filing.AmbiguousError](err); ok {
		return apiError(http.StatusConflict, amb.Error())
	}
	switch {
	case errors.Is(err, filing.ErrPatternNotFound), errors.Is(err, filing.ErrNotLinked):
		return apiError(http.StatusNotFound, err.Error())
	case errors.Is(err, filing.ErrNotFileable), errors.Is(err, kata.ErrInvalidIssueRef),
		errors.Is(err, kata.ErrIssueInOtherProject):
		return apiError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, filing.ErrKataNotReady):
		return apiError(http.StatusServiceUnavailable, err.Error())
	}
	if status := kata.StatusOf(err); status == http.StatusNotFound {
		return apiError(http.StatusNotFound, "Kata has no such issue")
	}
	if kata.StatusOf(err) != 0 || errors.Is(err, kata.ErrUnavailable) ||
		errors.Is(err, kata.ErrInvalidResponse) || errors.Is(err, kata.ErrNotReady) {
		// Kata error text comes from the remote server; err.Error() keeps
		// only the status.
		return apiError(http.StatusBadGateway, "Kata request failed: "+err.Error())
	}
	return frictionStoreError("friction issue link", err)
}
