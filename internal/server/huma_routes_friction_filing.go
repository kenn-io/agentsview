package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/timeutil"
)

// WithFrictionFiler attaches the hub's filer. Nil (every pusher) leaves the
// filing routes answering 503 kata_unavailable.
func WithFrictionFiler(f *filing.Filer) Option {
	return func(s *Server) { s.frictionFiler = f }
}

type FrictionFingerprintPath struct {
	Fingerprint string `path:"fingerprint" doc:"Friction pattern fingerprint (fl1:<sha256 hex>)"`
}

type frictionFileRequest struct {
	ForceNew bool   `json:"force_new,omitempty" doc:"Skip the metadata lookup and ask Kata for a new issue"`
	DryRun   bool   `json:"dry_run,omitempty" doc:"Return the request that would be sent; write nothing"`
	Date     string `json:"date,omitempty" doc:"Use the signal occurrence frozen in this digest date"`
}

type frictionFileInput struct {
	FrictionFingerprintPath
	Body frictionFileRequest
}

type FrictionFilePreview struct {
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Priority int               `json:"priority"`
	Labels   []string          `json:"labels"`
	Metadata map[string]string `json:"metadata"`
	ForceNew bool              `json:"force_new"`
}

type FrictionFileResponse struct {
	Link    *db.FrictionIssueLink `json:"link,omitempty"`
	Preview *FrictionFilePreview  `json:"preview,omitempty"`
}

type frictionLinkRequest struct {
	IssueRef string `json:"issue_ref" minLength:"1" doc:"Kata short id, qualified id (project#short) or UID"`
}

type frictionLinkInput struct {
	FrictionFingerprintPath
	Body frictionLinkRequest
}

func (s *Server) registerFrictionFilingRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Friction")
	s.post(group, "/friction/patterns/{fingerprint}/file", "File a friction pattern to Kata", s.humaFileFrictionPattern)
	s.put(group, "/friction/patterns/{fingerprint}/link", "Link a friction pattern to a Kata issue", s.humaLinkFrictionPattern)
	s.deleteRoute(group, "/friction/patterns/{fingerprint}/link", "Remove a friction pattern's local Kata link", s.humaUnlinkFrictionPattern)
}

// frictionMutationAllowed mirrors PR 6's run-route gate: auth, or localhost
// when auth is off.
func (s *Server) frictionMutationAllowed(ctx context.Context) error {
	if !s.frictionRequireAuth() && !isLocalhostContext(ctx) {
		return apiError(http.StatusForbidden, "friction filing is only permitted from localhost when auth is off")
	}
	return nil
}

func (s *Server) withFrictionExclusive(fn func() error) error {
	if s.frictionExclusive == nil {
		return fn()
	}
	return s.frictionExclusive(fn)
}

// kataUnavailable reports 503 with the Kata state (not_hub on pushers).
func (s *Server) kataUnavailable(ctx context.Context) error {
	return kataUnavailableError(s.kataConn().Status(ctx, false))
}

func (s *Server) frictionFilerUnavailable(ctx context.Context) error {
	st := s.kataConn().Status(ctx, false)
	if st.State == kata.StateReady {
		st.Message = "Friction filing is unavailable because no Friction review filer is configured on this archive hub."
	}
	return kataUnavailableError(st)
}

func kataUnavailableError(st kata.Status) error {
	msg := st.Message
	if msg == "" {
		msg = "Kata is " + string(st.State)
	}
	return &apiResponseError{Status: http.StatusServiceUnavailable, Code: "kata_unavailable", Message: msg, KataState: string(st.State)}
}

func (s *Server) humaFileFrictionPattern(ctx context.Context, in *frictionFileInput) (*jsonOutput[FrictionFileResponse], error) {
	if err := s.frictionMutationAllowed(ctx); err != nil {
		return nil, err
	}
	f := s.frictionFiler
	if f == nil {
		return nil, s.frictionFilerUnavailable(ctx)
	}
	var sig friction.Signal
	var run filing.RunContext
	var err error
	if in.Body.Date != "" {
		if !timeutil.IsValidDate(in.Body.Date) {
			return nil, apiError(http.StatusBadRequest, "invalid date format: use YYYY-MM-DD")
		}
		sig, run, err = f.SignalForFingerprintOnDate(ctx, in.Fingerprint, in.Body.Date)
	} else {
		sig, run, err = f.SignalForFingerprint(ctx, in.Fingerprint)
	}
	if errors.Is(err, filing.ErrSignalNotFound) {
		return nil, apiErrorWithCode(http.StatusNotFound, "signal_not_found",
			"no stored finding or digest contains fingerprint "+in.Fingerprint)
	}
	if err != nil {
		return nil, serverError(err)
	}
	run.ForceNew = in.Body.ForceNew
	if in.Body.DryRun {
		p, err := f.PlanWithContext(ctx, sig, run)
		if errors.Is(err, filing.ErrSignalNotFound) {
			return nil, apiError(http.StatusNotFound, "the finding's session is missing or trashed")
		}
		if err != nil {
			return nil, serverError(err)
		}
		metadata := make(map[string]string, len(p.Metadata))
		for key, value := range p.Metadata {
			str, ok := value.(string)
			if !ok {
				return nil, serverError(fmt.Errorf("friction preview metadata %q is not a string", key))
			}
			metadata[key] = str
		}
		return &jsonOutput[FrictionFileResponse]{Body: FrictionFileResponse{Preview: &FrictionFilePreview{
			Title: p.Title, Body: p.Body, Priority: p.Priority, Labels: p.Labels, Metadata: metadata, ForceNew: p.ForceNew,
		}}}, nil
	}
	if !f.Ready(ctx) {
		return nil, s.kataUnavailable(ctx)
	}
	var link db.FrictionIssueLink
	err = s.withFrictionExclusive(func() error {
		var fileErr error
		link, fileErr = f.FileAndRerender(ctx, sig, run)
		return fileErr
	})
	if err != nil && !errors.Is(err, filing.ErrRecordedKataFailure) {
		if h := handleHumaReadOnly(err); h != nil {
			return nil, h
		}
		return nil, serverError(err)
	}
	return &jsonOutput[FrictionFileResponse]{Body: FrictionFileResponse{Link: &link}}, nil
}

func (s *Server) humaLinkFrictionPattern(ctx context.Context, in *frictionLinkInput) (*jsonOutput[db.FrictionIssueLink], error) {
	if err := s.frictionMutationAllowed(ctx); err != nil {
		return nil, err
	}
	f := s.frictionFiler
	if f == nil {
		return nil, s.frictionFilerUnavailable(ctx)
	}
	if !f.Ready(ctx) {
		return nil, s.kataUnavailable(ctx)
	}
	var link db.FrictionIssueLink
	err := s.withFrictionExclusive(func() error {
		var linkErr error
		link, linkErr = f.Link(ctx, in.Fingerprint, in.Body.IssueRef)
		return linkErr
	})
	if kata.IsCode(err, "issue_not_found") {
		return nil, apiError(http.StatusNotFound, "Kata issue "+in.Body.IssueRef+" not found")
	}
	if err != nil {
		if h := handleHumaReadOnly(err); h != nil {
			return nil, h
		}
		return nil, serverError(err)
	}
	return &jsonOutput[db.FrictionIssueLink]{Body: link}, nil
}

type unlinkResponse struct {
	Fingerprint string `json:"fingerprint"`
	Unlinked    bool   `json:"unlinked"`
}

func (s *Server) humaUnlinkFrictionPattern(ctx context.Context, in *FrictionFingerprintPath) (*jsonOutput[unlinkResponse], error) {
	if err := s.frictionMutationAllowed(ctx); err != nil {
		return nil, err
	}
	f := s.frictionFiler
	if f == nil {
		return nil, s.frictionFilerUnavailable(ctx)
	}
	if err := s.withFrictionExclusive(func() error { return f.Unlink(ctx, in.Fingerprint) }); err != nil {
		if h := handleHumaReadOnly(err); h != nil {
			return nil, h
		}
		return nil, serverError(err)
	}
	return &jsonOutput[unlinkResponse]{Body: unlinkResponse{Fingerprint: in.Fingerprint, Unlinked: true}}, nil
}
