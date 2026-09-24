package server

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledgerstatus"
	"go.kenn.io/agentsview/internal/serdejson"
)

func (s *Server) registerLedgerRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Ledger")

	s.get(group, "/ledger/events", "Query ledger events", s.humaQueryLedgerEvents)
	s.get(group, "/ledger/status", "Ledger status", s.humaLedgerStatus)
	s.post(group, "/ledger/events", "Append ledger events", s.humaAppendLedgerEvents)
	s.post(group, "/ledger/verify", "Verify ledger checksums", s.humaVerifyLedger)
}

type ledgerQueryInput struct {
	Since     string   `query:"since" default:"7d" doc:"Nh, Nd, Nw, YYYY-MM-DD (UTC midnight) or an RFC 3339 time"`
	Subsystem []string `query:"subsystem" doc:"Subsystem glob; a trailing * is a prefix match; repeatable, OR-ed"`
	Class     string   `query:"class" doc:"Event class, e.g. state-change"`
	Zone      string   `query:"zone" doc:"Only this zone"`
	Limit     int      `query:"limit" default:"100" minimum:"1" maximum:"1000" doc:"Events per zone"`
}

// ledgerEventView is one event in API responses. Serde is the event's
// exact compact serialization (the bytes its segment checksum covers);
// clients that need exact payload numbers parse it instead of Payload.
type ledgerEventView struct {
	EventID       string         `json:"event_id"`
	Zone          string         `json:"zone"`
	Source        string         `json:"source"`
	SourceSeq     uint64         `json:"source_seq"`
	Timestamp     string         `json:"timestamp"`
	CorrelationID *string        `json:"correlation_id"`
	CausationID   *string        `json:"causation_id"`
	ActorRef      *string        `json:"actor_ref"`
	ObjectRef     *string        `json:"object_ref"`
	EventClass    string         `json:"event_class"`
	PayloadTier   string         `json:"payload_tier"`
	Payload       jsontext.Value `json:"payload"`
	Subsystem     string         `json:"subsystem"`
	Summary       string         `json:"summary"`
	Serde         string         `json:"serde"`
}

type ledgerZoneEventsView struct {
	Zone   string            `json:"zone"`
	Events []ledgerEventView `json:"events"`
}

type ledgerQueryResponse struct {
	Since   string                 `json:"since"`
	Results []ledgerZoneEventsView `json:"results"`
}

func ledgerEventToView(e ledger.Event) (ledgerEventView, error) {
	serde, err := e.MarshalSerde()
	if err != nil {
		return ledgerEventView{}, err
	}
	payload, err := serdejson.Compact(e.Payload)
	if err != nil {
		return ledgerEventView{}, err
	}
	v := ledgerEventView{
		EventID: e.EventID.String(), Zone: e.Zone, Source: e.Source,
		SourceSeq: e.SourceSeq, Timestamp: serdejson.FormatTimestamp(e.Timestamp),
		ActorRef: e.ActorRef, ObjectRef: e.ObjectRef,
		EventClass: string(e.EventClass), PayloadTier: string(e.PayloadTier),
		Payload: jsontext.Value(payload), Subsystem: ledger.Subsystem(e),
		Summary: ledger.Summary(e), Serde: string(serde),
	}
	if e.CorrelationID != nil {
		v.CorrelationID = new(e.CorrelationID.String())
	}
	if e.CausationID != nil {
		v.CausationID = new(e.CausationID.String())
	}
	return v, nil
}

// parseLedgerSince accepts jilog's --since forms and, for API clients
// that hold an absolute time, RFC 3339.
func parseLedgerSince(since string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, since); err == nil {
		return t, nil
	}
	return ledger.ParseSince(since, now)
}

func (s *Server) ledgerConfig() config.LedgerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Ledger
}

func (s *Server) humaQueryLedgerEvents(
	ctx context.Context, in *ledgerQueryInput,
) (*jsonOutput[ledgerQueryResponse], error) {
	since, err := parseLedgerSince(in.Since, time.Now())
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	q := ledger.Query{Since: since, Subsystems: in.Subsystem, Zone: in.Zone, Limit: in.Limit}
	if in.Class != "" {
		c, err := ledger.ParseClass(in.Class)
		if err != nil {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		q.Class = &c
	}
	if in.Zone != "" && !ledger.ValidSourceName(in.Zone) {
		return nil, apiError(http.StatusBadRequest, fmt.Sprintf("invalid zone %q", in.Zone))
	}
	results, err := s.db.QueryLedger(ctx, q)
	if err != nil {
		return nil, internalError("query ledger", err)
	}
	resp := ledgerQueryResponse{Since: in.Since, Results: []ledgerZoneEventsView{}}
	for _, z := range ledger.SortZoneEvents(results, s.ledgerConfig().ZoneIDs()) {
		view := ledgerZoneEventsView{Zone: z.Zone, Events: make([]ledgerEventView, 0, len(z.Events))}
		for _, e := range z.Events {
			v, err := ledgerEventToView(e)
			if err != nil {
				return nil, internalError("query ledger", err)
			}
			view.Events = append(view.Events, v)
		}
		resp.Results = append(resp.Results, view)
	}
	return &jsonOutput[ledgerQueryResponse]{Body: resp}, nil
}

type ledgerStatusInput struct {
	Zone string `query:"zone" doc:"Only this zone"`
}

type ledgerStatusResponse struct {
	Enabled bool                      `json:"enabled"`
	Zones   []ledgerstatus.ZoneReport `json:"zones"`
}

// ledgerZones is the configured zones (default first) followed by any
// other zone that holds segments, or just zone when it is set.
func (s *Server) ledgerZones(ctx context.Context, zone string) ([]string, error) {
	if zone != "" {
		if !ledger.ValidSourceName(zone) {
			return nil, apiError(http.StatusBadRequest, fmt.Sprintf("invalid zone %q", zone))
		}
		return []string{zone}, nil
	}
	stored, err := s.db.LedgerZones(ctx)
	if err != nil {
		return nil, internalError("list ledger zones", err)
	}
	return ledger.SortZones(stored, s.ledgerConfig().ZoneIDs()), nil
}

func (s *Server) humaLedgerStatus(
	ctx context.Context, in *ledgerStatusInput,
) (*jsonOutput[ledgerStatusResponse], error) {
	zones, err := s.ledgerZones(ctx, in.Zone)
	if err != nil {
		return nil, err
	}
	cfg := s.ledgerConfig()
	reports, err := ledgerstatus.Collect(ctx, cfg, s.db, zones)
	if err != nil {
		return nil, internalError("ledger status", err)
	}
	return &jsonOutput[ledgerStatusResponse]{Body: ledgerStatusResponse{Enabled: cfg.Enabled, Zones: reports}}, nil
}

// requireLedgerWrite applies the mutation rule for ledger routes: the
// ledger must be on, and with auth off only localhost may write.
func (s *Server) requireLedgerWrite(ctx context.Context) error {
	s.mu.RLock()
	enabled := s.cfg.Ledger.Enabled
	authRequired := s.cfg.RequireAuth
	s.mu.RUnlock()
	if !authRequired && !isLocalhostContext(ctx) {
		return apiError(http.StatusForbidden,
			"ledger writes are only permitted from localhost unless require_auth is on")
	}
	if !enabled {
		return apiErrorWithCode(http.StatusConflict, "ledger_disabled",
			"the event ledger is off; set enabled = true under [ledger] in config.toml")
	}
	return nil
}

type ledgerAppendEvent struct {
	EventClass    string         `json:"event_class" doc:"Event class, e.g. state-change"`
	PayloadTier   string         `json:"payload_tier,omitempty" doc:"metadata-only, structured or confidential; default structured with a payload, else metadata-only"`
	Timestamp     *time.Time     `json:"timestamp,omitempty" doc:"Defaults to now"`
	CorrelationID *string        `json:"correlation_id,omitempty"`
	CausationID   *string        `json:"causation_id,omitempty"`
	ActorRef      *string        `json:"actor_ref,omitempty"`
	ObjectRef     *string        `json:"object_ref,omitempty"`
	Payload       jsontext.Value `json:"payload,omitempty"`
}

type ledgerAppendRequest struct {
	Zone   string              `json:"zone,omitempty" doc:"Default: [ledger] default_zone"`
	Events []ledgerAppendEvent `json:"events" minItems:"1" maxItems:"1000"`
}

type ledgerAppendInput struct {
	Body ledgerAppendRequest
}

type ledgerAppendResponse struct {
	Zone      string   `json:"zone"`
	Source    string   `json:"source"`
	SourceSeq uint64   `json:"source_seq"`
	Checksum  uint32   `json:"checksum"`
	EventIDs  []string `json:"event_ids"`
}

func parseOptionalUUID(field string, s *string) (*uuid.UUID, error) {
	if s == nil {
		return nil, nil
	}
	u, err := uuid.Parse(*s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return &u, nil
}

func ledgerEventFromAppend(in ledgerAppendEvent) (ledger.Event, error) {
	class, err := ledger.ParseClass(in.EventClass)
	if err != nil {
		return ledger.Event{}, err
	}
	var payload any
	if len(in.Payload) > 0 && string(in.Payload) != "null" {
		if payload, err = serdejson.Decode(in.Payload); err != nil {
			return ledger.Event{}, fmt.Errorf("payload: %w", err)
		}
	}
	tier := ledger.TierMetadataOnly
	if payload != nil {
		tier = ledger.TierStructured
	}
	if in.PayloadTier != "" {
		if tier, err = ledger.ParseTier(in.PayloadTier); err != nil {
			return ledger.Event{}, err
		}
	}
	e := ledger.Event{
		EventClass: class, PayloadTier: tier, Payload: payload,
		ActorRef: in.ActorRef, ObjectRef: in.ObjectRef,
	}
	if in.Timestamp != nil {
		e.Timestamp = *in.Timestamp
	}
	if e.CorrelationID, err = parseOptionalUUID("correlation_id", in.CorrelationID); err != nil {
		return ledger.Event{}, err
	}
	if e.CausationID, err = parseOptionalUUID("causation_id", in.CausationID); err != nil {
		return ledger.Event{}, err
	}
	return e, nil
}

// humaAppendLedgerEvents writes one segment as this server's own source;
// the source is never taken from the request (spec §21).
func (s *Server) humaAppendLedgerEvents(
	ctx context.Context, in *ledgerAppendInput,
) (*createdOutput[ledgerAppendResponse], error) {
	if err := s.requireLedgerWrite(ctx); err != nil {
		return nil, err
	}
	events := make([]ledger.Event, 0, len(in.Body.Events))
	for i, raw := range in.Body.Events {
		e, err := ledgerEventFromAppend(raw)
		if err != nil {
			return nil, apiError(http.StatusBadRequest, fmt.Sprintf("events[%d]: %v", i, err))
		}
		events = append(events, e)
	}
	s.mu.RLock()
	cfg := s.cfg.Ledger
	source := cfg.EffectiveSource(s.cfg.InstallationID)
	s.mu.RUnlock()
	var exclusive func(func() error) error
	if s.engine != nil {
		exclusive = s.engine.RunExclusive
	}
	zone := in.Body.Zone
	if zone == "" {
		zone = cfg.EffectiveDefaultZone()
	}
	if _, ok := cfg.Zone(zone); !ok {
		return nil, apiError(http.StatusBadRequest, fmt.Sprintf("zone %q is not configured", zone))
	}
	seg, err := ledger.NewZoneWriters(s.db, source, cfg.ZoneIDs(), exclusive).AppendSegment(ctx, zone, events)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, ledger.ErrNoEvents) || errors.Is(err, ledger.ErrIntegrity) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, internalError("append ledger events", err)
	}
	ids := make([]string, 0, len(seg.Events))
	for _, e := range seg.Events {
		ids = append(ids, e.EventID.String())
	}
	return &createdOutput[ledgerAppendResponse]{
		Status: http.StatusCreated,
		Body: ledgerAppendResponse{
			Zone: zone, Source: seg.Source, SourceSeq: seg.SourceSeq,
			Checksum: seg.Checksum, EventIDs: ids,
		},
	}, nil
}

type ledgerVerifyRequest struct {
	Zone string `json:"zone,omitempty" doc:"Only this zone"`
	Full bool   `json:"full,omitempty" doc:"Re-verify every segment and reset the checkpoints"`
}

type ledgerVerifyInput struct {
	Body ledgerVerifyRequest
}

type ledgerVerifyZone struct {
	Zone                string `json:"zone"`
	ledger.VerifyReport `json:",inline"`
}

type ledgerVerifyResponse struct {
	Zones []ledgerVerifyZone `json:"zones"`
}

func (s *Server) humaVerifyLedger(
	ctx context.Context, in *ledgerVerifyInput,
) (*jsonOutput[ledgerVerifyResponse], error) {
	s.mu.RLock()
	authRequired := s.cfg.RequireAuth
	s.mu.RUnlock()
	if !authRequired && !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden,
			"ledger writes are only permitted from localhost unless require_auth is on")
	}
	zones, err := s.ledgerZones(ctx, in.Body.Zone)
	if err != nil {
		return nil, err
	}
	resp := ledgerVerifyResponse{Zones: []ledgerVerifyZone{}}
	for _, z := range zones {
		rep, err := ledger.VerifyZone(ctx, s.db, z, in.Body.Full)
		if err != nil {
			if handled := handleHumaReadOnly(err); handled != nil {
				return nil, handled
			}
			return nil, internalError("verify ledger", err)
		}
		if rep.Failures == nil {
			rep.Failures = [][3]string{}
		}
		resp.Zones = append(resp.Zones, ledgerVerifyZone{Zone: z, VerifyReport: rep})
	}
	return &jsonOutput[ledgerVerifyResponse]{Body: resp}, nil
}
