package telemetry

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (r *Reporter) screenViewHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			http.Error(w, "telemetry request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "invalid telemetry request", http.StatusBadRequest)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var event struct {
			Event      string         `json:"event"`
			Properties map[string]any `json:"properties"`
		}
		if err := json.UnmarshalDecode(jsontext.NewDecoder(bytes.NewReader(body)), &event); err != nil || !r.Enabled() || strings.TrimSpace(event.Event) != EventScreenViewed {
			next.ServeHTTP(w, req)
			return
		}
		properties, _ := r.SanitizeProperties(event.Event, event.Properties)
		screen, valid := properties["screen"].(string)
		claimed := false
		if valid {
			r.screenMu.Lock()
			today := time.Now().UTC().Format(time.DateOnly)
			if r.screenViews[screen] != today {
				var day string
				day, claimed, err = r.claimScreenView(screen, func() error {
					return r.client.Capture(EventScreenViewed, properties)
				})
				if claimed || err == nil {
					if r.screenViews == nil {
						r.screenViews = make(map[string]string)
					}
					r.screenViews[screen] = day
				}
			}
			r.screenMu.Unlock()
			if err != nil {
				if !claimed {
					http.Error(w, "recording screen view failed", http.StatusInternalServerError)
					return
				}
				slog.Warn("saving accepted screen view failed", "err", err)
			}
		}
		status := "dropped"
		if claimed {
			status = "queued"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.MarshalWrite(w, map[string]string{"status": status})
	})
}
