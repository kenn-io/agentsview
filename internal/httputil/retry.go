package httputil

import (
	"net/http"
	"strconv"
	"time"
)

// ParseRetryAfter reads a Retry-After header in either the delay-seconds or
// HTTP-date form, returning zero for absent or unparseable values.
func ParseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
