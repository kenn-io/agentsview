package friction

import (
	"net/url"
	"strconv"
	"strings"
)

// SessionURL links a session message in the browser. base is the AgentsView
// public URL including any base path; "" means no public URL, so no link.
// It mirrors the browser router: the agent prefix and opaque id are separate
// path segments.
func SessionURL(base, sessionID string, ordinal *int) string {
	if base == "" || sessionID == "" {
		return ""
	}
	prefix, rest, found := strings.Cut(sessionID, ":")
	path := url.PathEscape(prefix)
	if found {
		path += "/" + url.PathEscape(rest)
	}
	u := base + "/sessions/" + path
	if ordinal != nil {
		u += "?msg=" + strconv.Itoa(*ordinal)
	}
	return u
}

// DigestURL links the Friction Log page for date under base, or "" without a
// public URL.
func DigestURL(base, date string) string {
	if base == "" {
		return ""
	}
	return base + "/friction?date=" + url.QueryEscape(date)
}
