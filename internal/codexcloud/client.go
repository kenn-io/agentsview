package codexcloud

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// DialOptions configures one connection to the thread host.
type DialOptions struct {
	Endpoint *url.URL
	Login    Login
	Mode     Mode
	// Version is AgentsView's version, sent in User-Agent and clientInfo.
	Version string
	// ReadLimit bounds one incoming message; zero means DefaultReadLimit.
	ReadLimit int64
	// HTTPClient carries the handshake. Its redirect policy is replaced.
	HTTPClient *http.Client
	// CallTimeout bounds the handshake and each call; zero means
	// DefaultCallTimeout.
	CallTimeout time.Duration
}

// Conn is one initialized JSON-RPC connection to the thread host. It holds
// no token after Dial returns.
type Conn struct {
	ws    *websocket.Conn
	mode  Mode
	ids   atomic.Int64
	notes chan Notification
	done  chan struct{}

	timeout time.Duration

	mu      sync.Mutex
	pending map[int64]chan wireMessage
	err     error
}

// Dial opens and initializes a connection. Every error it returns is free
// of the token.
func Dial(ctx context.Context, o DialOptions) (*Conn, error) {
	if o.Endpoint == nil {
		return nil, errors.New("codex-cloud: no Codex service endpoint")
	}
	if o.Login.Token.Empty() {
		return nil, errors.New("codex-cloud: no ChatGPT access token")
	}
	header := http.Header{}
	header.Set("X-OpenAI-Product-Sku", "codex")
	header.Set("X-OpenAI-Allow-Environmentless-Fallback", "true")
	header.Set("originator", "agentsview")
	header.Set("User-Agent", "agentsview/"+o.Version)
	if o.Login.AccountID != "" {
		header.Set("ChatGPT-Account-Id", o.Login.AccountID)
	}
	client := http.Client{}
	if o.HTTPClient != nil {
		client = *o.HTTPClient
	}
	// A redirect would carry the subprotocol, and with it the token, to a
	// URL the endpoint check never saw.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	timeout := o.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	//nolint:bodyclose // coder/websocket owns the handshake response body.
	ws, resp, err := websocket.Dial(dialCtx, o.Endpoint.String(), &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: header,
		Subprotocols: []string{
			ServerSubprotocol, ClientSubprotocol, bearerSubprotocol + o.Login.Token.Reveal(),
		},
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, newHTTPError(resp, time.Now())
		}
		return nil, Redact(fmt.Errorf("codex-cloud: connect to %s: %w", o.Endpoint.Host, err), o.Login.Token)
	}
	// The server may select its own subprotocol or none. Anything else is a
	// protocol change; the selected value is not echoed because it could be
	// the bearer entry.
	if selected := ws.Subprotocol(); selected != "" && selected != ServerSubprotocol {
		_ = ws.CloseNow() // the protocol error is the one worth reporting
		return nil, fmt.Errorf("%w: OpenAI selected an unexpected subprotocol", ErrProtocolChange)
	}
	limit := o.ReadLimit
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	ws.SetReadLimit(limit)
	c := &Conn{
		ws:      ws,
		mode:    o.Mode,
		done:    make(chan struct{}),
		timeout: timeout,
		pending: map[int64]chan wireMessage{},
	}
	if o.Mode == ModeMonitor {
		c.notes = make(chan Notification, 256)
	}
	go c.readLoop()
	init := map[string]any{"clientInfo": map[string]string{"name": "agentsview", "version": o.Version}}
	if err := c.call(ctx, "initialize", init, nil); err != nil {
		c.fail(err)
		return nil, err
	}
	if err := c.send(ctx, request{JSONRPC: "2.0", Method: "initialized"}); err != nil {
		c.fail(err)
		return nil, err
	}
	return c, nil
}

// Call sends one allowed request and decodes its result into result, which
// may be nil. A refused call sends nothing.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if !allowed(c.mode, method, params) {
		return fmt.Errorf("%w: %s", ErrNotAllowed, method)
	}
	return c.call(ctx, method, params, result)
}

func (c *Conn) call(ctx context.Context, method string, params, result any) error {
	id := c.ids.Add(1)
	reply := make(chan wireMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(ctx, request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case msg := <-reply:
		return decodeReply(method, msg, result)
	case <-c.done:
		// The reply may have arrived just before the connection ended.
		select {
		case msg := <-reply:
			return decodeReply(method, msg, result)
		default:
			return c.Err()
		}
	case <-timer.C:
		// A server that holds the request open would otherwise stall every
		// later account until shutdown.
		c.fail(fmt.Errorf("%w: %s", ErrCallTimeout, method))
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// decodeReply returns a reply's error or decodes its result. A result that
// does not decode means OpenAI changed the reply.
func decodeReply(method string, msg wireMessage, result any) error {
	if msg.Error != nil {
		return msg.Error
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Result, result); err != nil {
		return fmt.Errorf("%w: decode %s reply: %w", ErrProtocolChange, method, err)
	}
	return nil
}

func (c *Conn) send(ctx context.Context, req request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("codex-cloud: encode %s: %w", req.Method, err)
	}
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		// A failed write leaves the websocket unusable. Ending the connection
		// here, not only when the read loop notices, lets callers treat the
		// failure as the network problem it is.
		c.fail(fmt.Errorf("codex-cloud: send %s: %w", req.Method, err))
		return c.Err()
	}
	return nil
}

// Notifications delivers server notifications in monitor mode. It is closed
// when the connection ends. Fetch mode returns nil and drops notifications.
func (c *Conn) Notifications() <-chan Notification { return c.notes }

// Done is closed when the connection ends for any reason.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection. Pending calls return ErrClosed.
func (c *Conn) Close() error {
	if !c.setErr(ErrClosed) {
		return nil
	}
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// fail ends the connection with err unless it already ended.
func (c *Conn) fail(err error) {
	if c.setErr(err) {
		_ = c.ws.CloseNow() // err already records why the connection ended
	}
}

func (c *Conn) setErr(err error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false
	}
	c.err = err
	close(c.done)
	return true
}

func (c *Conn) readLoop() {
	if c.notes != nil {
		defer close(c.notes)
	}
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				c.fail(ErrReplyTooLarge)
			} else {
				c.fail(fmt.Errorf("codex-cloud: connection ended: %w", err))
			}
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.fail(fmt.Errorf("%w: unreadable message from OpenAI: %w", ErrProtocolChange, err))
			return
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			// Server requests stay unanswered so the user's own apps answer
			// them. A refresh request can never be met, so stop here.
			if msg.Method == refreshMethod {
				c.fail(ErrRefreshRequested)
				return
			}
		case msg.Method != "":
			if c.notes != nil {
				select {
				case c.notes <- Notification{Method: msg.Method, Params: msg.Params}:
				case <-c.done:
					return
				}
			}
		default:
			c.deliver(msg)
		}
	}
}

func (c *Conn) deliver(msg wireMessage) {
	id, err := strconv.ParseInt(string(msg.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	reply, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		select {
		case reply <- msg:
		default: // a duplicate reply for one ID is dropped
		}
	}
}
