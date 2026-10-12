// Package codexcloudtest provides fakes of OpenAI's hosted Codex services
// for tests. Nothing here talks to a real service.
package codexcloudtest

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// Error is a JSON-RPC error a handler returns.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handler answers one request. A nil *Error sends result.
type Handler func(params jsontext.Value) (result any, rpcErr *Error)

// Request is one client message the host received.
type Request struct {
	ID     jsontext.Value
	Method string
	Params jsontext.Value
}

// Handshake is one websocket upgrade request the host received.
type Handshake struct {
	Header       http.Header
	Subprotocols []string
}

// Host is a fake durable thread host on a loopback websocket server.
type Host struct {
	server *httptest.Server

	mu         sync.Mutex
	handlers   map[string]Handler
	requests   []Request
	handshakes []Handshake
	conns      []*websocket.Conn
	selected   string
	refuse     *refusal
	bearers    map[string]int
	echo       bool
	serverIDs  int
}

type refusal struct {
	status int
	header http.Header
	body   string
}

// NewHost starts a host that selects ServerSubprotocol and answers
// initialize. It stops when the test ends.
func NewHost(t *testing.T) *Host {
	t.Helper()
	h := &Host{handlers: map[string]Handler{}, selected: "codex-app-server"}
	h.Handle("initialize", func(jsontext.Value) (any, *Error) {
		return map[string]string{"userAgent": "codexcloudtest"}, nil
	})
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.close)
	return h
}

// URL is the host's ws:// endpoint.
func (h *Host) URL() *url.URL {
	u, err := url.Parse("ws" + strings.TrimPrefix(h.server.URL, "http") + "/")
	if err != nil {
		panic(err)
	}
	return u
}

// Handle answers method with fn, replacing any earlier handler. Methods
// without a handler get "Method not found".
func (h *Host) Handle(method string, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method] = fn
}

// SelectSubprotocol makes later upgrades select p; "" selects none.
func (h *Host) SelectSubprotocol(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.selected = p
}

// RefuseHandshake makes later upgrades answer status with header and body.
func (h *Host) RefuseHandshake(status int, header http.Header, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuse = &refusal{status: status, header: header, body: body}
}

// AcceptHandshakes undoes RefuseHandshake.
func (h *Host) AcceptHandshakes() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuse = nil
}

// RefuseBearer makes later upgrades that carry token answer status, so
// tests can refuse one login while accepting another.
func (h *Host) RefuseBearer(token string, status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bearers == nil {
		h.bearers = map[string]int{}
	}
	h.bearers["openai-bearer."+token] = status
}

// Redirect makes later upgrades answer 302 to target.
func (h *Host) Redirect(target string) {
	h.RefuseHandshake(http.StatusFound, http.Header{"Location": {target}}, "")
}

// EchoSubprotocols makes later upgrades succeed while selecting the
// client's whole subprotocol header, as a broken proxy might. The client
// must refuse the connection without repeating that header.
func (h *Host) EchoSubprotocols() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.echo = true
}

// Requests returns every client message received so far, in order.
func (h *Host) Requests() []Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Request(nil), h.requests...)
}

// Methods returns the method of every client message, in order; a client
// reply to a server request appears as "".
func (h *Host) Methods() []string {
	requests := h.Requests()
	methods := make([]string, len(requests))
	for i, r := range requests {
		methods[i] = r.Method
	}
	return methods
}

// Handshakes returns every upgrade request received so far.
func (h *Host) Handshakes() []Handshake {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Handshake(nil), h.handshakes...)
}

// ServerRequest sends a server-initiated request to every open connection.
func (h *Host) ServerRequest(ctx context.Context, method string, params any) error {
	h.mu.Lock()
	h.serverIDs++
	id := "server-" + strconv.Itoa(h.serverIDs)
	h.mu.Unlock()
	return h.broadcast(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// Notify sends a notification to every open connection.
func (h *Host) Notify(ctx context.Context, method string, params any) error {
	return h.broadcast(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (h *Host) broadcast(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	conns := append([]*websocket.Conn(nil), h.conns...)
	h.mu.Unlock()
	for _, c := range conns {
		if err := c.Write(ctx, websocket.MessageText, data); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.handshakes = append(h.handshakes, Handshake{
		Header:       r.Header.Clone(),
		Subprotocols: websocketSubprotocols(r.Header),
	})
	refuse, selected, echo := h.refuse, h.selected, h.echo
	for _, protocol := range websocketSubprotocols(r.Header) {
		if status, ok := h.bearers[protocol]; ok {
			refuse = &refusal{status: status}
		}
	}
	h.mu.Unlock()
	if echo {
		echoUpgrade(w, r)
		return
	}
	if refuse != nil {
		maps.Copy(w.Header(), refuse.header)
		w.WriteHeader(refuse.status)
		_, _ = w.Write([]byte(refuse.body))
		return
	}
	var subprotocols []string
	if selected != "" {
		subprotocols = []string{selected}
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: subprotocols})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 30)
	h.mu.Lock()
	h.conns = append(h.conns, c)
	h.mu.Unlock()
	defer h.drop(c)
	for {
		_, data, err := c.Read(context.Background())
		if err != nil {
			return
		}
		var msg struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
			Params jsontext.Value `json:"params"`
		}
		if json.Unmarshal(data, &msg) != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, Request{ID: msg.ID, Method: msg.Method, Params: msg.Params})
		handler := h.handlers[msg.Method]
		h.mu.Unlock()
		if len(msg.ID) == 0 || msg.Method == "" {
			continue
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if handler == nil {
			reply["error"] = Error{Code: -32601, Message: "Method not found: " + msg.Method}
		} else if result, rpcErr := handler(msg.Params); rpcErr != nil {
			reply["error"] = rpcErr
		} else {
			reply["result"] = result
		}
		out, err := json.Marshal(reply)
		if err != nil {
			return
		}
		if c.Write(context.Background(), websocket.MessageText, out) != nil {
			return
		}
	}
}

func (h *Host) drop(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, open := range h.conns {
		if open == c {
			h.conns = append(h.conns[:i], h.conns[i+1:]...)
			break
		}
	}
}

func (h *Host) close() {
	h.mu.Lock()
	conns := append([]*websocket.Conn(nil), h.conns...)
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow() // the test is over; a close error changes nothing
	}
	h.server.Close()
}

// echoUpgrade completes a valid upgrade whose selected subprotocol is the
// request's entire Sec-WebSocket-Protocol header.
func echoUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	//nolint:gosec // RFC 6455 fixes SHA-1 for Sec-WebSocket-Accept.
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n" +
		"Sec-WebSocket-Protocol: " + r.Header.Get("Sec-WebSocket-Protocol") + "\r\n\r\n")
	_ = rw.Flush()
}

func websocketSubprotocols(header http.Header) []string {
	var out []string
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for part := range strings.SplitSeq(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
