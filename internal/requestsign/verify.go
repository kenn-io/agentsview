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

type VerifyConfig struct {
	ExternalURL string
	// StripPrefix explicitly trusts a loopback-only TLS terminator that strips
	// the configured external prefix. Forwarded headers never supply authority.
	StripPrefix bool
	Replay      *Replay
	Keys        func() (map[string]Key, error)
}

const (
	maxConcurrentVerifications = 8
	maxConcurrentHandlers      = 32
	maxConcurrentStreams       = 8
)

type Verifier struct {
	base         *url.URL
	strip        bool
	replay       *Replay
	keys         func() (map[string]Key, error)
	verifySlots  chan struct{}
	handlerSlots chan struct{}
	streamSlots  chan struct{}
	bodyTimeout  time.Duration
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
		strip:        cfg.StripPrefix,
		replay:       cfg.Replay,
		keys:         cfg.Keys,
		verifySlots:  make(chan struct{}, maxConcurrentVerifications),
		handlerSlots: make(chan struct{}, maxConcurrentHandlers),
		streamSlots:  make(chan struct{}, maxConcurrentStreams),
		bodyTimeout:  30 * time.Second,
	}, nil
}

func (v *Verifier) target(r *http.Request) (string, error) {
	if r.URL.IsAbs() || !strings.HasPrefix(r.RequestURI, "/") || r.RequestURI != r.URL.RequestURI() || validateTarget(r.URL) != nil {
		return "", ErrInvalid
	}
	if v.strip {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return "", ErrInvalid
		}
	} else if r.TLS == nil {
		return "", ErrInvalid
	}
	u := *r.URL
	u.Scheme, u.Host = v.base.Scheme, v.base.Host
	if v.strip {
		u.Path = v.base.Path + u.Path
		u.RawPath = ""
	}
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
// authorization. Both authorization stages are rechecked before durable admission.
func (v *Verifier) Wrap(next http.Handler, authenticate func(http.Handler) http.Handler, authorize func(Key, *http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case v.handlerSlots <- struct{}{}:
			defer func() { <-v.handlerSlots }()
		default:
			http.Error(w, "signature verification busy", http.StatusServiceUnavailable)
			return
		}
		if v.longLivedStream(r) {
			select {
			case v.streamSlots <- struct{}{}:
				defer func() { <-v.streamSlots }()
			default:
				http.Error(w, "signature verification busy", http.StatusServiceUnavailable)
				return
			}
		}
		incoming := r
		released := false
		release := func() {
			if !released {
				<-v.verifySlots
				released = true
			}
		}
		select {
		case v.verifySlots <- struct{}{}:
			defer release()
		default:
			http.Error(w, "signature verification busy", http.StatusServiceUnavailable)
			return
		}
		if err := validateHeaders(r, true); err != nil {
			signatureError(w, 401)
			return
		}
		target, err := v.target(r)
		if err != nil {
			signatureError(w, 401)
			return
		}
		input, err := parseInput(r.Header.Get("Signature-Input"))
		if err != nil || !fresh(input.created, input.expires, time.Now()) {
			signatureError(w, 401)
			return
		}
		key, err := v.key(input.keyID)
		raw := r.Header.Get("Signature")
		encoded, ok := strings.CutPrefix(raw, "sig1=:")
		encoded, suffix := strings.CutSuffix(encoded, ":")
		signature, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || !ok || !suffix || decodeErr != nil || len(signature) != 32 || !hmac.Equal(signature, macValue(key.Secret, signatureBase(r, target, input.params))) {
			signatureError(w, 401)
			return
		}
		bodyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authorize != nil && !authorize(key, r) {
				signatureError(w, 403)
				return
			}
			if r.ContentLength > MaxBodyBytes {
				signatureError(w, 413)
				return
			}
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(time.Now().Add(v.bodyTimeout)); err != nil {
				signatureError(w, 503)
				return
			}
			defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
			var body []byte
			if r.Body != nil {
				body, err = io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
				_ = r.Body.Close()
				if err != nil {
					signatureError(w, 408)
					return
				}
			}
			if len(body) > MaxBodyBytes {
				signatureError(w, 413)
				return
			}
			if len(incoming.Trailer) != 0 || len(r.Trailer) != 0 {
				signatureError(w, 401)
				return
			}
			if !hmac.Equal([]byte(r.Header.Get("Content-Digest")), []byte(digest(body))) {
				signatureError(w, 401)
				return
			}
			if err = r.Context().Err(); err != nil {
				signatureError(w, 401)
				return
			}
			// Holding the replay write lock prevents lock wait from extending freshness
			// or admitting a key revoked while another request was committing.
			err = v.replay.admit(r.Context(), input.keyID, input.nonce, input.created, input.expires, func() error {
				current, err := v.key(input.keyID)
				if err != nil || !hmac.Equal(current.Secret, key.Secret) || current.Grant != key.Grant || current.DeviceID != key.DeviceID || authorize != nil && !authorize(current, r) {
					return ErrInvalid
				}
				return nil
			})
			if err != nil {
				if errors.Is(err, ErrReplay) {
					signatureError(w, 409)
				} else {
					signatureError(w, 401)
				}
				return
			}
			_ = controller.SetReadDeadline(time.Time{})
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			release()
			next.ServeHTTP(w, r)
		})
		var bodyStage http.Handler = bodyHandler
		if authenticate != nil {
			bodyStage = authenticate(bodyStage)
		}
		bodyStage.ServeHTTP(w, r)
	})
}

func (v *Verifier) longLivedStream(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.Path
	if !v.strip {
		path = strings.TrimPrefix(path, v.base.Path)
		if path == "" {
			path = "/"
		}
	}
	return path == "/api/v1/events" || strings.HasSuffix(path, "/watch")
}

func signatureError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// No key IDs, content, credentials or signature material in public errors.
	_, _ = io.WriteString(w, `{"error":"request signing rejected","code":"request_signing_rejected"}`+"\n")
}
