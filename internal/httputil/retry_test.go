package httputil

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {"120", 2 * time.Minute}, {"0", 0}, {"-3", 0}, {"", 0}, {"garbage", 0}, {"Mon, 02 Jan 2006 15:04:05 GMT", 0},
	} {
		assert.Equal(t, tt.want, ParseRetryAfter(tt.value), tt.value)
	}
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	assert.InDelta(t, 5*time.Second, ParseRetryAfter(future), float64(time.Second))
}
