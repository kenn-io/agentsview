package requestsign

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Signer struct {
	base *url.URL
	key  Key
}

func NewSigner(baseURL string, key Key) (*Signer, error) {
	base, err := externalBase(baseURL)
	if err != nil {
		return nil, err
	}
	if !safeID.MatchString(key.ID) || len(key.Secret) < 64 {
		return nil, errors.New("signing key requires a safe ID and at least 64 decoded bytes")
	}
	key.Secret = bytes.Clone(key.Secret)
	return &Signer{base: base, key: key}, nil
}

func (s *Signer) Sign(r *http.Request) error {
	if !withinBase(r.URL, s.base) || r.Host != "" && canonicalHost(s.base.Scheme, r.Host) != s.base.Host ||
		validateTarget(r.URL) != nil || validateHeaders(r, false) != nil {
		return ErrInvalid
	}
	if r.ContentLength > MaxBodyBytes {
		return errors.New("signed request exceeds body limit")
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		_ = r.Body.Close()
		if err != nil {
			return errors.New("reading signed request body failed")
		}
		if len(body) > MaxBodyBytes {
			return errors.New("signed request exceeds body limit")
		}
	}
	if err := r.Context().Err(); err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set("Content-Digest", digest(body))
	if r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	for _, name := range coveredHeaders {
		if len(r.Header.Values(name)) == 0 {
			r.Header.Set(name, "")
		}
	}
	if err := validateHeaders(r, true); err != nil {
		return err
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("generating request nonce failed")
	}
	created := time.Now().Unix()
	params := fmt.Sprintf(`%s;created=%d;expires=%d;nonce="%s";keyid="%s";alg="hmac-sha256"`, components, created, created+30, base64.RawURLEncoding.EncodeToString(nonce), s.key.ID)
	r.Header.Set("Signature-Input", "sig1="+params)
	r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(macValue(s.key.Secret, signatureBase(r, canonicalTarget(r.URL, s.base), params)))+":")
	return nil
}

type signedTransport struct {
	signer *Signer
	next   http.RoundTripper
}

func (t signedTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t signedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clonedRequest := r.Clone(r.Context())
	if err := t.signer.Sign(clonedRequest); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(clonedRequest)
}

// Client pins credentials and signatures to one HTTPS origin and base prefix.
// Opaque retries inside net/http may replay a signature and fail closed.
func (s *Signer) Client(client *http.Client) (*http.Client, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	c := *client
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if standard, ok := transport.(*http.Transport); ok {
		cfg := standard.TLSClientConfig
		if cfg != nil && (cfg.InsecureSkipVerify || cfg.MinVersion != 0 && cfg.MinVersion < tls.VersionTLS12 || cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS12) {
			return nil, errors.New("signing requires TLS certificate verification")
		}
	}
	c.Transport = signedTransport{s, transport}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c, nil
}

// ClientFromEnvironment signs requests to the origin configured by
// AGENTSVIEW_SIGNING_URL and leaves every other origin unchanged, so local
// daemons and other remote hosts keep their existing transport. Plain HTTP to
// the signing host and a different prefix on the signing origin fail closed.
func ClientFromEnvironment(baseURL string, client *http.Client) (*http.Client, error) {
	target, signed, err := environmentTarget(baseURL)
	if err != nil {
		return nil, err
	}
	if !signed {
		return client, nil
	}
	secret, err := ReadSecret(os.Getenv("AGENTSVIEW_SIGNING_KEY_FILE"))
	if err != nil {
		return nil, err
	}
	signer, err := NewSigner(target, Key{ID: os.Getenv("AGENTSVIEW_SIGNING_KEY_ID"), Secret: secret})
	if err != nil {
		return nil, err
	}
	return signer.Client(client)
}

// SignsFromEnvironment reports whether ClientFromEnvironment signs requests to
// baseURL. Signed origins are machine listeners that browsers cannot use.
func SignsFromEnvironment(baseURL string) bool {
	_, signed, err := environmentTarget(baseURL)
	return signed && err == nil
}

func environmentTarget(baseURL string) (string, bool, error) {
	target := os.Getenv("AGENTSVIEW_SIGNING_URL")
	id, file := os.Getenv("AGENTSVIEW_SIGNING_KEY_ID"), os.Getenv("AGENTSVIEW_SIGNING_KEY_FILE")
	if target == "" && id == "" && file == "" {
		return "", false, nil
	}
	if target == "" || id == "" || file == "" {
		return "", false, errors.New(
			"set AGENTSVIEW_SIGNING_URL, AGENTSVIEW_SIGNING_KEY_ID and AGENTSVIEW_SIGNING_KEY_FILE together")
	}
	base, err := externalBase(target)
	if err != nil {
		return "", false, err
	}
	requested, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", false, ErrInvalid
	}
	// Credentials for the signing host never travel over plain HTTP, on any port.
	if !strings.EqualFold(requested.Hostname(), base.Hostname()) {
		return "", false, nil
	}
	if requested.Scheme != base.Scheme {
		return "", false, errors.New("HTTP target cannot downgrade the HTTPS signing origin")
	}
	if canonicalHost(requested.Scheme, requested.Host) != base.Host {
		return "", false, nil
	}
	if requested.Path != base.Path {
		return "", false, errors.New("HTTP target differs from configured signing prefix")
	}
	return target, true, nil
}
