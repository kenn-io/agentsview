// Package requestsign implements AgentsView's strict RFC 9421 HMAC profile.
package requestsign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MaxBodyBytes   = 4 << 20
	MaxHeaderBytes = 16 << 10
	validity       = 30 * time.Second
	futureSkew     = 5 * time.Second
	components     = `("@method" "@target-uri" "content-digest" "content-type" "authorization" "x-agentsview-device-id" "upload-offset" "x-agentsview-search-intent")`
)

var (
	ErrInvalid     = errors.New("invalid request signature")
	ErrReplay      = errors.New("request signature replay or replay state unavailable")
	safeID         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	inputPattern   = regexp.MustCompile(`^sig1=` + regexp.QuoteMeta(components) + `;created=([0-9]{1,12});expires=([0-9]{1,12});nonce="([A-Za-z0-9_-]{32})";keyid="([A-Za-z0-9._-]{1,128})";alg="hmac-sha256"$`)
	coveredHeaders = []string{"Content-Digest", "Content-Type", "Authorization", "X-Agentsview-Device-Id", "Upload-Offset", "X-Agentsview-Search-Intent"}
)

type Key struct {
	ID       string
	Secret   []byte
	Grant    string
	DeviceID string
}

type signatureInput struct {
	created, expires     int64
	nonce, keyID, params string
}

func parseInput(value string) (signatureInput, error) {
	m := inputPattern.FindStringSubmatch(value)
	if m == nil {
		return signatureInput{}, ErrInvalid
	}
	created, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return signatureInput{}, ErrInvalid
	}
	expires, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || strconv.FormatInt(created, 10) != m[1] || strconv.FormatInt(expires, 10) != m[2] {
		return signatureInput{}, ErrInvalid
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(m[3])
	if err != nil || len(nonce) != 24 {
		return signatureInput{}, ErrInvalid
	}
	return signatureInput{created, expires, m[3], m[4], strings.TrimPrefix(value, "sig1=")}, nil
}

func fresh(created, expires int64, now time.Time) bool {
	n := now.Unix()
	return created >= 0 && expires > created && expires-created <= int64(validity/time.Second) && created <= n+int64(futureSkew/time.Second) && n < expires
}

func externalBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return nil, errors.New("signing requires an explicit HTTPS base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path != "" && (path.Clean(u.Path) != u.Path || strings.Contains(u.Path, "//")) {
		return nil, ErrInvalid
	}
	return u, nil
}

func validateTarget(u *url.URL) error {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.User != nil || u.Fragment != "" || u.Opaque != "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "%\\") || strings.Contains(p, "//") || (path.Clean(p) != strings.TrimSuffix(p, "/") && p != "/") {
		return ErrInvalid
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return ErrInvalid
	}
	if len(u.RequestURI()) > MaxHeaderBytes {
		return ErrInvalid
	}
	return nil
}

func withinBase(u, base *url.URL) bool {
	return u.Scheme == base.Scheme && u.Host == base.Host && (base.Path == "" || strings.HasPrefix(u.Path, base.Path+"/"))
}

func validateHeaders(r *http.Request, requirePresent bool) error {
	if len(r.Trailer) > 0 || len(r.Header.Values("Trailer")) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		return ErrInvalid
	}
	size := len(r.RequestURI) + len(r.Method) + len(r.Host)
	for name, values := range r.Header {
		size += len(name)
		for _, v := range values {
			size += len(v)
		}
	}
	if size > MaxHeaderBytes {
		return ErrInvalid
	}
	for _, name := range append(append([]string{}, coveredHeaders...), "Signature", "Signature-Input") {
		values := r.Header.Values(name)
		if len(values) > 1 {
			return ErrInvalid
		}
		for _, v := range values {
			if strings.ContainsAny(v, "\r\n") {
				return ErrInvalid
			}
		}
	}
	if requirePresent {
		for _, name := range coveredHeaders {
			if len(r.Header.Values(name)) != 1 {
				return ErrInvalid
			}
		}
		if r.Header.Get("Authorization") == "" || r.Header.Get("Content-Type") == "" {
			return ErrInvalid
		}
	}
	return nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

func signatureBase(r *http.Request, target, params string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\"@method\": %s\n\"@target-uri\": %s\n", r.Method, target)
	for _, h := range coveredHeaders {
		fmt.Fprintf(&b, "\"%s\": %s\n", strings.ToLower(h), strings.Trim(r.Header.Get(h), " \t"))
	}
	b.WriteString(`"@signature-params": `)
	b.WriteString(params)
	return b.String()
}

func macValue(key []byte, base string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(base))
	return m.Sum(nil)
}
