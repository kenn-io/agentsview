package server

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const telemetryEventMaxBodyBytes = 4 << 10

// WithTelemetryCapture mounts the UI telemetry route; a nil handler leaves it unregistered.
func WithTelemetryCapture(h http.Handler) Option {
	return func(s *Server) { s.telemetryCapture = h }
}

func (s *Server) registerTelemetryCaptureRoute() {
	if s.telemetryCapture == nil {
		return
	}
	s.handleHTTP(&huma.Operation{
		Method:  http.MethodPost,
		Path:    "/api/v1/telemetry/events",
		Summary: "Report a UI telemetry event",
		Hidden:  true,
	}, func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, telemetryEventMaxBodyBytes)
		s.telemetryCapture.ServeHTTP(w, r)
	})
}
