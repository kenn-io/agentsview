package codexcloud

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
)

// Category says what a JSON-RPC error reply means for a pass or a join.
type Category int

const (
	// CatThreadError affects one thread: the fetcher retries it later.
	CatThreadError Category = iota
	// CatAccountUnavailable is an overloaded or shutting-down server: end
	// the pass with the 5xx wait and retry no thread.
	CatAccountUnavailable
	// CatProtocolChange is a reply this version cannot read.
	CatProtocolChange
	// CatAnchoredRead is a rejected anchored item read: re-read the turn in
	// full before giving up on the thread.
	CatAnchoredRead
	// CatAccountServiceError is any other error from initialize or
	// thread/list: end the pass with the 5xx wait.
	CatAccountServiceError
)

// Classify applies the spec's JSON-RPC error rules, in order. anchored
// reports whether a thread/items/list call used an item anchor.
func Classify(method string, anchored bool, e *RPCError) Category {
	var data struct {
		ErrorType string `json:"errorType"`
		Reason    string `json:"reason"`
	}
	if len(e.Data) > 0 {
		// Data shapes vary by error; an unreadable one just matches no rule.
		_ = json.Unmarshal(e.Data, &data)
	}
	switch {
	case e.Code == -32600 && data.ErrorType == "environment_terminated":
		return CatThreadError
	case e.Code == -32001, e.Code == -32600 && data.Reason == "serverShuttingDown":
		return CatAccountUnavailable
	case e.Code == -32601 && strings.HasPrefix(e.Message, "unsupported app-server method"),
		e.Code == -32600 && (strings.HasPrefix(e.Message, "Invalid request:") ||
			strings.Contains(e.Message, "experimentalApi")):
		return CatProtocolChange
	case e.Code == -32602 && method == "thread/items/list" && anchored:
		return CatAnchoredRead
	case e.Code == -32602:
		return CatProtocolChange
	}
	switch method {
	case "thread/turns/list", "thread/items/list", resumeMethod:
		return CatThreadError
	default:
		return CatAccountServiceError
	}
}

// Failure is a cause the status board and the waits know about.
type Failure int

const (
	FailNone Failure = iota
	FailSignedOut
	FailKeyring
	FailEphemeral
	FailRefreshRequested
	FailRefused
	FailRateLimited
	FailNetwork
	FailServer
	FailProtocol
	FailTooLarge
	FailThread
)

var failureNames = map[Failure]string{
	FailNone:             "none",
	FailSignedOut:        "signed-out",
	FailKeyring:          "keyring",
	FailEphemeral:        "ephemeral",
	FailRefreshRequested: "refresh-requested",
	FailRefused:          "refused",
	FailRateLimited:      "rate-limited",
	FailNetwork:          "network",
	FailServer:           "server",
	FailProtocol:         "protocol",
	FailTooLarge:         "too-large",
	FailThread:           "thread",
}

func (f Failure) String() string { return failureNames[f] }

// FailureFor names the cause of an error from Dial or a call. A JSON-RPC
// reply is classified as account-level; per-thread callers use Classify
// first. Callers check their own context before calling this.
func FailureFor(err error, login Login) Failure {
	switch {
	case err == nil:
		return FailNone
	case errors.Is(err, ErrRefreshRequested):
		return FailRefreshRequested
	case errors.Is(err, ErrReplyTooLarge):
		return FailTooLarge
	case errors.Is(err, ErrProtocolChange):
		return FailProtocol
	}
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		switch {
		case httpErr.Status == http.StatusUnauthorized && login.Store == "auto":
			// In auto mode Codex may have moved the live login to the
			// keyring, leaving a stale token in auth.json.
			return FailKeyring
		case httpErr.Status == http.StatusUnauthorized:
			return FailSignedOut
		case httpErr.Status == http.StatusForbidden:
			return FailRefused
		case httpErr.Status == http.StatusTooManyRequests:
			return FailRateLimited
		case httpErr.Status >= 500:
			return FailServer
		default:
			// A redirect or any other refusal: the handshake changed.
			return FailProtocol
		}
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); ok {
		switch Classify("", false, rpcErr) {
		case CatProtocolChange:
			return FailProtocol
		default:
			return FailServer
		}
	}
	return FailNetwork
}
