package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Mode selects the methods a connection may call.
type Mode int

const (
	// ModeFetch reads thread listings and history only.
	ModeFetch Mode = iota
	// ModeMonitor also joins running threads with thread/resume.
	ModeMonitor
)

const (
	// ClientSubprotocol names AgentsView to the thread host. Changing it
	// needs the user's consent; never fall back to another client's name.
	ClientSubprotocol = "codex-client.agentsview"
	// ServerSubprotocol is the only subprotocol the thread host may select.
	ServerSubprotocol = "codex-app-server"
	bearerSubprotocol = "openai-bearer."
	// DefaultReadLimit bounds one incoming message. The largest probed reply
	// was about 530 KB; a whole turn under itemsView "full" can be far
	// larger.
	DefaultReadLimit = 64 << 20
	// DefaultCallTimeout bounds the handshake and each call, so a server
	// that never answers ends one account's pass instead of every pass.
	DefaultCallTimeout = 2 * time.Minute

	refreshMethod = "account/chatgptAuthTokens/refresh"
	resumeMethod  = "thread/resume"
)

var (
	// ErrRefreshRequested means the thread host asked for a fresh login.
	// AgentsView never refreshes a token, so the connection is closed.
	ErrRefreshRequested = errors.New("codex-cloud: OpenAI asked for a token refresh")
	// ErrNotAllowed means a call was refused before it was sent.
	ErrNotAllowed = errors.New("codex-cloud: request not allowed")
	// ErrReplyTooLarge means a message exceeded the read limit, which closes
	// the connection.
	ErrReplyTooLarge = errors.New("codex-cloud: reply larger than the read limit")
	// ErrProtocolChange means the thread host answered in a way this
	// version cannot read.
	ErrProtocolChange = errors.New("codex-cloud: OpenAI changed its protocol")
	// ErrCallTimeout means a call got no reply within the call timeout.
	ErrCallTimeout = errors.New("codex-cloud: no reply from OpenAI")
	// ErrClosed means the connection was closed by this side.
	ErrClosed = errors.New("codex-cloud: connection closed")
)

// ResumeParams is the only accepted thread/resume request.
type ResumeParams struct {
	ThreadID     string `json:"threadId"`
	ExcludeTurns bool   `json:"excludeTurns"`
}

// Notification is a server message without an ID.
type Notification struct {
	Method string
	Params jsontext.Value
}

// RPCError is a JSON-RPC error reply.
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data,omitzero"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex-cloud: JSON-RPC error %d: %s", e.Code, e.Message)
}

// HTTPError is a refused websocket handshake.
type HTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("codex-cloud: handshake refused with HTTP %d", e.Status)
}

func newHTTPError(resp *http.Response, now time.Time) *HTTPError {
	err := &HTTPError{Status: resp.StatusCode}
	value := resp.Header.Get("Retry-After")
	if seconds, parseErr := strconv.Atoi(value); parseErr == nil && seconds > 0 {
		err.RetryAfter = time.Duration(seconds) * time.Second
	} else if when, parseErr := http.ParseTime(value); parseErr == nil && when.After(now) {
		err.RetryAfter = when.Sub(now)
	}
	return err
}

// allowed reports whether mode may send method with params. Dial sends
// initialize and initialized itself; nothing else reaches the wire.
func allowed(mode Mode, method string, params any) bool {
	switch method {
	case "thread/list", "thread/items/list", "thread/turns/list":
		return true
	case resumeMethod:
		resume, ok := params.(ResumeParams)
		return ok && mode == ModeMonitor && resume.ThreadID != "" && resume.ExcludeTurns
	default:
		return false
	}
}

// DecodeCount reads a token counter. The thread host sends counters as JSON
// numbers such as 4603049.0; a fraction, a negative value, or anything that
// is not a number is a protocol change.
func DecodeCount(v jsontext.Value) (int64, error) {
	if v.Kind() != '0' {
		return 0, fmt.Errorf("codex-cloud: token counter %s is not a number", v)
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return 0, fmt.Errorf("codex-cloud: token counter %s: %w", v, err)
	}
	if f < 0 || f != math.Trunc(f) || f >= math.MaxInt64 {
		return 0, fmt.Errorf("codex-cloud: token counter %s is not a whole count", v)
	}
	return int64(f), nil
}

// wireMessage is any JSON-RPC message in either direction.
type wireMessage struct {
	ID     jsontext.Value `json:"id,omitzero"`
	Method string         `json:"method,omitzero"`
	Params jsontext.Value `json:"params,omitzero"`
	Result jsontext.Value `json:"result,omitzero"`
	Error  *RPCError      `json:"error,omitzero"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitzero"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitzero"`
}
