package server

import (
	"fmt"
	"net/http"
	"strconv"
)

// parseIntParam reads an integer query parameter from r.
// Returns (value, true) on success, or writes a 400 error and
// returns (0, false) if the parameter is present but not a valid
// integer.  When the parameter is absent, returns (0, true).
func parseIntParam(
	w http.ResponseWriter, r *http.Request, name string,
) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %s parameter", name))
		return 0, false
	}
	return v, true
}

// parseBoolParam reads a boolean query parameter from r. Returns
// (false, true) when the parameter is absent, and writes a 400 unless
// the parameter is exactly "true" or "false".
func parseBoolParam(
	w http.ResponseWriter, r *http.Request, name string,
) (bool, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, true
	}
	switch raw {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid %s parameter", name))
		return false, false
	}
}

// parseNonNegativeIntParam reads an integer query parameter that must
// be non-negative (e.g. cursor / OFFSET values). Returns (value, true)
// on success, writes a 400 and returns (0, false) on a non-integer or
// negative value. Negative values flow through to SQL OFFSET on
// PostgreSQL as an error (SQLite tolerates them); rejecting at the
// handler turns the silent 500 into a clean 400.
func parseNonNegativeIntParam(
	w http.ResponseWriter, r *http.Request, name string,
) (int, bool) {
	v, ok := parseIntParam(w, r, name)
	if !ok {
		return 0, false
	}
	if v < 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s must not be negative", name))
		return 0, false
	}
	return v, true
}

// validateDateFilters validates the shared date query params: date, date_from,
// and date_to must be YYYY-MM-DD; date_from must not be after date_to; and
// active_since must be an RFC3339 timestamp. On the first invalid value it
// writes a 400 and returns false, so callers must return when it returns false.
// Pass "" for any param an endpoint does not accept. Centralizing this keeps
// malformed dates out of the query layer, where they would otherwise reach the
// DB and surface as a 500 (e.g. PostgreSQL casting to date/timestamptz).
// clampLimit applies a default and upper bound to a limit value.
func clampLimit(limit, defaultLimit, maxLimit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
