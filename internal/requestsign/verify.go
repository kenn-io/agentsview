package requestsign

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VerifyConfig describes a listener behind a loopback-only TLS terminator that
// strips the configured external prefix. Forwarded headers never supply
// authority; the external URL is trusted operator configuration.
type VerifyConfig struct {
	ExternalURL string
	Replay      *Replay
	Keys        func() (map[string]Key, error)
}

const (
	maxConcurrentVerifications = 8
	maxConcurrentBodies        = 8
	maxConcurrentHandlers      = 32
	maxConcurrentStreams       = 8
)

type Verifier struct {
	base         *url.URL
	replay       *Replay
	keys         func() (map[string]Key, error)
	verifySlots  chan struct{}
	bodySlots    chan struct{}
	handlerSlots chan struct{}
	streamSlots  chan struct{}
	bodyTimeout  time.Duration
	// now records when request headers arrive; signatures must be fresh then.
	now func() time.Time
}

// verifiedSignature is a header signature that passed MAC verification.
type verifiedSignature struct {
	key      Key
	input    signatureInput
	received time.Time
}

func NewVerifier(cfg VerifyConfig) (*Verifier, error) {
	base, err := externalBase(cfg.ExternalURL)
	if err != nil {
		return nil, err
	}
	if cfg.Replay == nil || cfg.Keys == nil {
		return nil, errors.New("signature verification requires replay state and key policy")
	}
	return &Verifier{
		base:         base,
		replay:       cfg.Replay,
		keys:         cfg.Keys,
		verifySlots:  make(chan struct{}, maxConcurrentVerifications),
		bodySlots:    make(chan struct{}, maxConcurrentBodies),
		handlerSlots: make(chan struct{}, maxConcurrentHandlers),
		streamSlots:  make(chan struct{}, maxConcurrentStreams),
		bodyTimeout:  maxBodyWait,
		now:          time.Now,
	}, nil
}

func (v *Verifier) target(r *http.Request) (string, error) {
	if r.URL.IsAbs() || !strings.HasPrefix(r.RequestURI, "/") ||
		r.RequestURI != r.URL.RequestURI() || validateTarget(r.URL) != nil {
		return "", ErrInvalid
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return "", ErrInvalid
	}
	u := *r.URL
	u.Scheme, u.Host = v.base.Scheme, v.base.Host
	u.Path = v.base.Path + u.Path
	u.RawPath = ""
	if !withinBase(&u, v.base) {
		return "", ErrInvalid
	}
	return u.String(), nil
}

func (v *Verifier) key(id string) (Key, error) {
	keys, err := v.keys()
	if err != nil {
		return Key{}, ErrInvalid
	}
	key, ok := keys[id]
	if !ok || key.ID != id || len(key.Secret) < 64 {
		return Key{}, ErrInvalid
	}
	return key, nil
}

// Wrap checks headers and MAC before native authentication or body reads.
// authorize sees native identity and the signing key. Signing is never native
// authorization. Both authorization stages are rechecked after the body
// arrives and before durable admission.
func (v *Verifier) Wrap(
	next http.Handler,
	authenticate func(http.Handler) http.Handler,
	authorize func(Key, *http.Request) bool,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case v.handlerSlots <- struct{}{}:
			defer func() { <-v.handlerSlots }()
		default:
			busyError(w)
			return
		}
		if v.longLivedStream(r) {
			select {
			case v.streamSlots <- struct{}{}:
				defer func() { <-v.streamSlots }()
			default:
				busyError(w)
				return
			}
		}
		signed, status := v.verifyHeaders(r)
		if status != 0 {
			signatureError(w, status)
			return
		}
		bodyStage := v.bodyStage(next, authorize, signed, r)
		if authenticate != nil {
			bodyStage = authenticate(bodyStage)
		}
		bodyStage.ServeHTTP(w, r)
	})
}

// verifyHeaders checks the signature over headers alone. It returns a non-zero
// HTTP status on rejection.
func (v *Verifier) verifyHeaders(r *http.Request) (verifiedSignature, int) {
	select {
	case v.verifySlots <- struct{}{}:
		defer func() { <-v.verifySlots }()
	case <-r.Context().Done():
		return verifiedSignature{}, http.StatusServiceUnavailable
	}
	received := v.now()
	if err := validateHeaders(r, true); err != nil {
		return verifiedSignature{}, http.StatusUnauthorized
	}
	target, err := v.target(r)
	if err != nil {
		return verifiedSignature{}, http.StatusUnauthorized
	}
	input, err := parseInput(r.Header.Get("Signature-Input"))
	if err != nil || !fresh(input.created, input.expires, received) {
		return verifiedSignature{}, http.StatusUnauthorized
	}
	key, err := v.key(input.keyID)
	encoded, ok := strings.CutPrefix(r.Header.Get("Signature"), "sig1=:")
	encoded, suffix := strings.CutSuffix(encoded, ":")
	signature, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || !ok || !suffix || decodeErr != nil || len(signature) != 32 ||
		!hmac.Equal(signature, macValue(key.Secret, signatureBase(r, target, input.params))) {
		return verifiedSignature{}, http.StatusUnauthorized
	}
	return verifiedSignature{key: key, input: input, received: received}, 0
}

func (v *Verifier) bodyStage(
	next http.Handler,
	authorize func(Key, *http.Request) bool,
	signed verifiedSignature,
	incoming *http.Request,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorize != nil && !authorize(signed.key, r) {
			signatureError(w, http.StatusForbidden)
			return
		}
		if r.ContentLength > MaxBodyBytes {
			signatureError(w, http.StatusRequestEntityTooLarge)
			return
		}
		body, status := v.readAndAdmit(w, r, incoming, signed, authorize)
		if status != 0 {
			signatureError(w, status)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}

// readAndAdmit buffers the signed body, checks its digest, rechecks the key and
// grant, and durably admits the nonce. It returns a non-zero HTTP status on
// rejection.
func (v *Verifier) readAndAdmit(
	w http.ResponseWriter,
	r, incoming *http.Request,
	signed verifiedSignature,
	authorize func(Key, *http.Request) bool,
) ([]byte, int) {
	if r.ContentLength != 0 {
		select {
		case v.bodySlots <- struct{}{}:
			defer func() { <-v.bodySlots }()
		case <-r.Context().Done():
			return nil, http.StatusServiceUnavailable
		}
	}
	body, status := v.readBody(w, r)
	if status != 0 {
		return nil, status
	}
	if len(incoming.Trailer) != 0 || len(r.Trailer) != 0 ||
		!hmac.Equal([]byte(r.Header.Get("Content-Digest")), []byte(digest(body))) ||
		r.Context().Err() != nil {
		return nil, http.StatusUnauthorized
	}
	// The key may have been rotated or revoked, or the native credential
	// withdrawn, while the body was in flight.
	current, err := v.key(signed.input.keyID)
	if err != nil || !hmac.Equal(current.Secret, signed.key.Secret) ||
		current.Grant != signed.key.Grant || current.DeviceID != signed.key.DeviceID ||
		authorize != nil && !authorize(current, r) {
		return nil, http.StatusUnauthorized
	}
	in := signed.input
	err = v.replay.Admit(r.Context(), in.keyID, in.nonce, in.created, in.expires, signed.received)
	if errors.Is(err, ErrReplay) {
		return nil, http.StatusConflict
	}
	if err != nil {
		return nil, http.StatusUnauthorized
	}
	return body, 0
}

func (v *Verifier) readBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(v.bodyTimeout)); err != nil {
		return nil, http.StatusServiceUnavailable
	}
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	if r.Body == nil {
		return nil, 0
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, http.StatusRequestTimeout
	}
	if len(body) > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge
	}
	return body, 0
}

func (v *Verifier) longLivedStream(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	return r.URL.Path == "/api/v1/events" || strings.HasSuffix(r.URL.Path, "/watch")
}

// Public errors carry no key IDs, content, credentials or signature material.
func signatureError(w http.ResponseWriter, status int) {
	writeSigningError(w, status, `{"error":"request signing rejected","code":"request_signing_rejected"}`)
}

func busyError(w http.ResponseWriter) {
	writeSigningError(w, http.StatusServiceUnavailable, `{"error":"request signing busy","code":"request_signing_busy"}`)
}

func writeSigningError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
