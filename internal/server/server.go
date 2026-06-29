package server

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/web"
	"go.kenn.io/kit/daemon"
)

// VersionInfo holds build-time version metadata.
type VersionInfo struct {
	Version                    string `json:"version"`
	Commit                     string `json:"commit"`
	BuildDate                  string `json:"build_date"`
	ReadOnly                   bool   `json:"read_only,omitempty"`
	InsightGenerationAvailable bool   `json:"insight_generation_available,omitempty"`
	APIVersion                 int    `json:"api_version"`
	DataVersion                int    `json:"data_version"`
}

const daemonService = "agentsview"

const (
	defaultInsightLogDrainTimeout    = 2 * time.Second
	defaultInsightLogStopWaitTimeout = 500 * time.Millisecond
)

// Server is the HTTP server that serves the SPA and REST API.
type Server struct {
	mu             gosync.RWMutex
	cfg            config.Config
	db             db.Store
	engine         *sync.Engine
	onDemandEngine *sync.Engine
	sessions       service.SessionService
	broadcaster    *Broadcaster
	mux            *http.ServeMux
	api            huma.API
	httpSrv        *http.Server
	version        VersionInfo
	dataDir        string

	// baseCtx, when set, is used as the base context for all
	// incoming requests. Cancelling it causes SSE handlers to
	// exit promptly, which unblocks graceful shutdown.
	baseCtx context.Context

	generateStreamFunc insight.GenerateStreamFunc
	spaFS              fs.FS
	spaHandler         http.Handler

	insightLogDrainTimeout    time.Duration
	insightLogStopWaitTimeout time.Duration

	// handlerDelay is injected before each timeout-wrapped
	// handler, used only by tests to guarantee handlers
	// exceed a short timeout. Zero in production.
	handlerDelay time.Duration

	// updateCheckFn is the function called to check for
	// updates. Defaults to update.CheckForUpdate; tests
	// can override it via WithUpdateChecker.
	updateCheckFn UpdateCheckFunc

	// basePath is a URL prefix for reverse-proxy deployments
	// (e.g. "/agentsview"). When set, all routes are served
	// under this prefix and a <base href> tag is injected
	// into the SPA's index.html.
	basePath string
	idle     *IdleTracker
}

// New creates a new Server.
func New(
	cfg config.Config, database db.Store, engine *sync.Engine,
	opts ...Option,
) *Server {
	dist, err := web.Assets()
	if err != nil {
		log.Fatalf("embedded frontend not found: %v", err)
	}

	// Pick the backend that matches the concrete store. A local
	// *db.DB plus a sync engine yields a full read/write backend;
	// any other combination (PG reader, or local DB with nil
	// engine when used by a read-only daemon) yields a read-only
	// backend whose Sync returns db.ErrReadOnly.
	var sessions service.SessionService
	if local, ok := database.(*db.DB); ok && engine != nil {
		sessions = service.NewDirectBackend(local, engine)
	} else {
		sessions = service.NewReadOnlyBackend(database)
	}

	s := &Server{
		cfg:                       cfg,
		db:                        database,
		engine:                    engine,
		sessions:                  sessions,
		mux:                       http.NewServeMux(),
		insightLogDrainTimeout:    defaultInsightLogDrainTimeout,
		insightLogStopWaitTimeout: defaultInsightLogStopWaitTimeout,
		generateStreamFunc: func(
			ctx context.Context, agent, prompt string,
			onLog insight.LogFunc,
		) (insight.Result, error) {
			return insight.GenerateStreamWithOptions(
				ctx, agent, prompt, onLog,
				insight.GenerateOptions{
					Agents: insightAgentConfig(cfg.Agent),
				},
			)
		},
		spaFS:      dist,
		spaHandler: http.FileServerFS(dist),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.version.APIVersion == 0 {
		s.version.APIVersion = 1
	}
	if s.version.DataVersion == 0 {
		s.version.DataVersion = db.CurrentDataVersion()
	}
	s.routes()
	return s
}

// Option configures a Server.
type Option func(*Server)

func insightAgentConfig(
	cfg map[string]config.AgentConfig,
) map[string]insight.AgentConfig {
	if len(cfg) == 0 {
		return nil
	}
	agents := make(map[string]insight.AgentConfig, len(cfg))
	for name, agentCfg := range cfg {
		agents[name] = insight.AgentConfig{
			Binary:      agentCfg.Binary,
			Sandbox:     agentCfg.Sandbox,
			AllowUnsafe: agentCfg.AllowUnsafe,
		}
	}
	return agents
}

// WithVersion sets the build-time version metadata. Zero-valued fields are
// filled with defaults in New after all options have been applied.
func WithVersion(v VersionInfo) Option {
	return func(s *Server) {
		s.version = v
	}
}

// WithDataDir sets the data directory used for update caching.
func WithDataDir(dir string) Option {
	return func(s *Server) { s.dataDir = dir }
}

// WithBaseContext sets the base context for all incoming HTTP
// requests. When this context is cancelled, request contexts
// are also cancelled, causing long-lived handlers (SSE) to
// exit and unblocking graceful shutdown.
func WithBaseContext(ctx context.Context) Option {
	return func(s *Server) { s.baseCtx = ctx }
}

// WithBroadcaster wires an event broadcaster into the server so the
// /api/v1/events handler has something to subscribe to. Required for
// live-refresh SSE; absent in PG serve mode where the engine is nil.
func WithBroadcaster(b *Broadcaster) Option {
	return func(s *Server) { s.broadcaster = b }
}

// WithUpdateChecker overrides the update check function,
// allowing tests to substitute a deterministic stub.
func WithUpdateChecker(f UpdateCheckFunc) Option {
	return func(s *Server) { s.updateCheckFn = f }
}

// WithBasePath sets a URL prefix for reverse-proxy deployments.
// The path must start with "/" and not end with "/" (e.g.
// "/agentsview"). When set, the server strips this prefix from
// incoming requests and injects a <base href> tag into the SPA.
func WithBasePath(path string) Option {
	return func(s *Server) {
		s.basePath = strings.TrimRight(path, "/")
	}
}

// WithGenerateFunc overrides the insight generation function,
// allowing tests to substitute a stub. Nil is ignored.
func WithGenerateFunc(f insight.GenerateFunc) Option {
	return func(s *Server) {
		if f != nil {
			s.generateStreamFunc = func(
				ctx context.Context, agent, prompt string,
				_ insight.LogFunc,
			) (insight.Result, error) {
				return f(ctx, agent, prompt)
			}
		}
	}
}

// WithGenerateStreamFunc overrides the streaming insight
// generation function used by the SSE handler. Nil is ignored.
func WithGenerateStreamFunc(f insight.GenerateStreamFunc) Option {
	return func(s *Server) {
		if f != nil {
			s.generateStreamFunc = f
		}
	}
}

// WithInsightLogDrainTimeouts overrides SSE insight log stream drain timeouts.
// Zero or negative values keep the production defaults.
func WithInsightLogDrainTimeouts(drain, stopWait time.Duration) Option {
	return func(s *Server) {
		if drain > 0 {
			s.insightLogDrainTimeout = drain
		}
		if stopWait > 0 {
			s.insightLogStopWaitTimeout = stopWait
		}
	}
}

func WithIdleTracker(t *IdleTracker) Option {
	return func(s *Server) { s.idle = t }
}

func (s *Server) humaConfig() huma.Config {
	version := s.version.Version
	if version == "" {
		version = "dev"
	}
	cfg := huma.DefaultConfig("AgentsView API", version)
	cfg.Info.Description = "HTTP API for browsing, searching, syncing, and managing local agent sessions."
	cfg.OpenAPIPath = "/api/openapi"
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil
	cfg.Components.Schemas = huma.NewMapRegistry(
		"#/components/schemas/",
		agentsViewSchemaNamer,
	)
	if s.basePath != "" {
		cfg.Servers = []*huma.Server{{
			URL:         s.basePath,
			Description: "Configured reverse-proxy base path",
		}}
	}
	return cfg
}

func (s *Server) routes() {
	configureHumaErrors()
	s.api = humago.New(s.mux, s.humaConfig())
	s.registerTypedAPIRoutes()

	// SPA fallback: serve embedded frontend
	// Do not use timeout handler for static assets to avoid buffering.
	s.mux.Handle("/", http.HandlerFunc(s.handleSPA))
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	// Try to serve the exact file
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	f, err := s.spaFS.Open(path)
	if err == nil {
		f.Close()
		// For index.html with a base path, inject <base href>.
		if s.basePath != "" && path == "index.html" {
			s.serveIndexWithBase(w, r)
			return
		}
		s.spaHandler.ServeHTTP(w, r)
		return
	}

	// SPA fallback: serve index.html for all routes
	if s.basePath != "" {
		s.serveIndexWithBase(w, r)
		return
	}
	r.URL.Path = "/"
	s.spaHandler.ServeHTTP(w, r)
}

// serveIndexWithBase reads the embedded index.html, injects a
// <base href> tag, and rewrites root-relative asset paths so
// everything resolves correctly behind a reverse proxy subpath.
func (s *Server) serveIndexWithBase(
	w http.ResponseWriter, _ *http.Request,
) {
	f, err := s.spaFS.Open("index.html")
	if err != nil {
		http.Error(w, "index.html not found",
			http.StatusInternalServerError)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "reading index.html",
			http.StatusInternalServerError)
		return
	}
	html := string(data)

	// Rewrite root-relative asset paths (href="/...", src="/...")
	// to include the base path prefix so the browser fetches
	// assets through the reverse proxy.
	bp := s.basePath
	html = strings.ReplaceAll(html, `href="/`, `href="`+bp+`/`)
	html = strings.ReplaceAll(html, `src="/`, `src="`+bp+`/`)

	// Inject <base href> AFTER rewriting paths so it doesn't
	// get double-prefixed by the replacement above.
	baseTag := fmt.Sprintf(
		`<base href="%s/">`, bp,
	)
	html = strings.Replace(
		html, "<head>", "<head>\n    "+baseTag, 1,
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// SetPort updates the listen port (for testing).
func (s *Server) SetPort(port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Port = port
}

// SetGithubToken updates the GitHub token for testing.
func (s *Server) SetGithubToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.GithubToken = token
}

// githubToken returns the configured GitHub token or a fallback token
// from the process environment / GitHub CLI (thread-safe).
func (s *Server) githubToken(ctx context.Context) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolveGitHubToken(ctx, s.cfg.GithubToken)
}

// Handler returns the http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	allowedOrigins := buildAllowedOrigins(
		s.cfg.Host, s.cfg.Port, s.cfg.PublicOrigins,
	)
	allowedHosts := buildAllowedHosts(
		s.cfg.Host, s.cfg.Port,
		s.cfg.PublicURL, s.cfg.PublicOrigins,
	)
	bindAll := isBindAll(s.cfg.Host)
	bindAllIPs := map[string]bool(nil)
	if bindAll {
		bindAllIPs = localInterfaceIPs()
	}
	h := cspMiddleware(
		s.cfg.Host, s.cfg.Port, s.basePath,
		s.cfg.PublicURL, s.cfg.PublicOrigins,
		s.authMiddleware(
			hostCheckMiddleware(
				allowedHosts, bindAll, s.cfg.Port, bindAllIPs,
				corsMiddleware(
					allowedOrigins, bindAll, s.cfg.Port, bindAllIPs,
					gzipMiddleware(logMiddleware(s.mux)),
				),
			),
		),
	)
	if s.basePath != "" {
		inner := h
		prefix := s.basePath
		h = http.HandlerFunc(func(
			w http.ResponseWriter, r *http.Request,
		) {
			p := r.URL.Path
			// Redirect /basepath to /basepath/ for the SPA.
			if p == prefix {
				http.Redirect(w, r,
					prefix+"/", http.StatusMovedPermanently)
				return
			}
			// Only match full path-segment prefixes to
			// prevent /basepathFOO from being handled.
			if !strings.HasPrefix(p, prefix+"/") {
				http.NotFound(w, r)
				return
			}
			http.StripPrefix(prefix, inner).
				ServeHTTP(w, r)
		})
	}
	h = s.idle.Wrap(h)
	return h
}

// cspMiddleware sets a Content-Security-Policy header on non-API
// responses. The policy pins the exact host:port origin so that
// even if Tauri's compile-time CSP uses a wildcard port, the
// intersection narrows to the actual runtime port.
func cspMiddleware(
	host string,
	port int,
	basePath string,
	publicURL string,
	publicOrigins []string,
	next http.Handler,
) http.Handler {
	policy := buildCSPPolicy(host, port, basePath, publicURL, publicOrigins)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Security-Policy", policy)
			w.Header().Set("X-Frame-Options", "DENY")
		}
		next.ServeHTTP(w, r)
	})
}

// buildCSPPolicy constructs the Content-Security-Policy string.
//
// The server's own origin (host:port) is pinned in the resource
// directives (default/script/img/style/font) because WebKitGTK in a
// Tauri webview may not resolve 'self' to the Go server origin after
// navigating from tauri://localhost.
//
// connect-src is intentionally widened to any http/https/ws/wss
// origin. The "Connect to Remote Server" feature (see
// frontend/src/lib/api/client.ts) lets the user point the SPA at an
// arbitrary remote agentsview API origin stored client-side, which
// this server cannot know when the policy is built. This mirrors the
// backend, where authenticated remote requests already bypass the
// host-check and CORS restrictions (see isRemoteAuth in auth.go and
// corsMiddleware). Security tradeoff: a broad connect-src means that
// if an XSS ever executed in the app, exfiltration would be easier;
// the other directives stay pinned so script execution remains gated
// to 'self'.
func buildCSPPolicy(
	host string,
	port int,
	basePath string,
	publicURL string,
	publicOrigins []string,
) string {
	// serverOrigins are the pinned origins used in the resource
	// directives so resources load correctly regardless of how the
	// webview resolves 'self'. Public origins are included for
	// reverse-proxy deployments; concrete local origins are preserved
	// for desktop webviews that need the backend socket pinned.
	serverOrigins := cspPinnedOrigins(host, port, publicURL, publicOrigins)
	resourceSrc := "'self' " + strings.Join(serverOrigins, " ")

	baseURI := "'none'"
	if basePath != "" {
		baseURI = "'self'"
	}

	return fmt.Sprintf(
		"default-src %[1]s; "+
			"script-src %[1]s; "+
			"connect-src 'self' http: https: ws: wss:; "+
			"img-src %[1]s data:; "+
			"style-src %[1]s 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src %[1]s data: https://fonts.gstatic.com; "+
			"object-src 'none'; "+
			"base-uri %[2]s; "+
			"frame-ancestors 'none'",
		resourceSrc, baseURI,
	)
}

func cspPinnedOrigins(
	host string,
	port int,
	publicURL string,
	publicOrigins []string,
) []string {
	origins := make([]string, 0, 1+len(publicOrigins)+1)
	seen := make(map[string]bool)
	add := func(origin string) {
		if origin == "" || seen[origin] {
			return
		}
		seen[origin] = true
		origins = append(origins, origin)
	}
	for _, raw := range append([]string{publicURL}, publicOrigins...) {
		add(normalizedOrigin(raw))
	}
	if !isUnspecifiedHost(host) {
		add("http://" + net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if len(origins) == 0 {
		add("http://" + net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return origins
}

func isUnspecifiedHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func normalizedOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// buildAllowedHosts returns the set of Host header values that
// are legitimate for this server. This defends against DNS
// rebinding attacks where an attacker's domain resolves to
// 127.0.0.1 — the browser sends the attacker's domain as the
// Host header, which we reject.
func buildAllowedHosts(
	host string, port int,
	publicURL string, publicOrigins []string,
) map[string]bool {
	hosts := make(map[string]bool)
	add := func(h string) {
		hosts[net.JoinHostPort(h, strconv.Itoa(port))] = true
		// Browsers may omit port 80 from the Host header.
		// IPv6 literals need brackets (e.g., [::1]).
		if port == 80 {
			if strings.Contains(h, ":") {
				hosts["["+h+"]"] = true
			} else {
				hosts[h] = true
			}
		}
	}
	add(host)
	switch host {
	case "127.0.0.1":
		add("localhost")
	case "localhost":
		add("127.0.0.1")
	case "0.0.0.0", "::":
		add("127.0.0.1")
		add("localhost")
		add("::1")
	case "::1":
		add("127.0.0.1")
		add("localhost")
	}
	if publicURL != "" {
		addHostHeadersFromOrigin(hosts, publicURL)
	}
	for _, origin := range publicOrigins {
		addHostHeadersFromOrigin(hosts, origin)
	}
	return hosts
}

// hostCheckMiddleware validates the Host header against expected
// values to prevent DNS rebinding attacks. Only applied to /api/
// routes — the SPA fallback is left accessible for flexibility.
func hostCheckMiddleware(
	allowedHosts map[string]bool, bindAll bool, port int, allowedIPs map[string]bool, next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Authenticated remote requests bypass host checks.
			if isRemoteAuth(r) {
				next.ServeHTTP(w, r)
				return
			}
			hostAllowed := allowedHosts[r.Host]
			// In bind-all mode, also allow local-interface IP-literal
			// hosts on the configured port so LAN clients can reach the
			// API while still rejecting rebinding via attacker-controlled
			// domains.
			if !hostAllowed && bindAll {
				hostAllowed = isAllowedBindAllHost(r.Host, port, allowedIPs)
			}
			if !hostAllowed {
				allowed := sortedHosts(allowedHosts)
				log.Printf(
					"host check rejected %s %s: Host %q not in allowed "+
						"set %v; if reaching agentsview through a forwarded "+
						"port or remote host, restart with --public-url "+
						"<origin> matching your browser URL",
					r.Method, r.URL.Path, r.Host, allowed,
				)
				http.Error(
					w, hostRejectionMessage(r.Host, allowed),
					http.StatusForbidden,
				)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sortedHosts returns the allowed Host header values as a sorted
// slice for deterministic log and error output.
func sortedHosts(hosts map[string]bool) []string {
	out := make([]string, 0, len(hosts))
	for h := range hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// hostRejectionMessage builds a self-explaining 403 body for a
// rejected Host header. It names the offending Host, lists the
// allowed values, and points at --public-url so users behind SSH
// port-forwarding, reverse proxies, or remote dev environments
// (exe.dev, Codespaces, Coder, WSL2) can diagnose without devtools.
func hostRejectionMessage(host string, allowed []string) string {
	return fmt.Sprintf(
		"Forbidden: request Host %q is not in the allowed set %v. "+
			"If you are reaching agentsview through SSH port-forwarding, "+
			"a reverse proxy, or a remote dev environment, restart the "+
			"server with --public-url <origin> matching the URL in your "+
			"browser (for example --public-url http://%s).",
		host, allowed, host,
	)
}

// httpOrigin formats an HTTP origin string. It uses
// net.JoinHostPort to handle IPv6 bracket formatting correctly
// (e.g., [::1]:8080). Browsers omit the port from the Origin
// header for default ports (80 for HTTP), so for port 80 both
// forms are returned.
func httpOrigin(host string, port int) []string {
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	origin := "http://" + hp
	if port == 80 {
		// net.JoinHostPort brackets IPv6, so use it for the
		// portless form too: JoinHostPort("::1","") is not
		// valid, so bracket manually when needed.
		bare := host
		if strings.Contains(host, ":") {
			bare = "[" + host + "]"
		}
		return []string{origin, "http://" + bare}
	}
	return []string{origin}
}

// buildAllowedOrigins returns the set of origins that should be
// permitted by CORS. For loopback addresses, both "127.0.0.1"
// and "localhost" are allowed because browsers treat them as
// distinct origins.
func buildAllowedOrigins(host string, port int, publicOrigins []string) map[string]bool {
	origins := make(map[string]bool)
	add := func(h string) {
		for _, o := range httpOrigin(h, port) {
			origins[o] = true
		}
	}
	add(host)
	// When binding to a loopback address, also allow the other
	// loopback variants because browsers treat them as distinct
	// origins. When binding to 0.0.0.0 or :: (all interfaces),
	// allow all loopback origins since that's how browsers will
	// access a bind-all server.
	switch host {
	case "127.0.0.1":
		add("localhost")
	case "localhost":
		add("127.0.0.1")
	case "0.0.0.0", "::":
		add("127.0.0.1")
		add("localhost")
		add("::1")
	case "::1":
		add("127.0.0.1")
		add("localhost")
	}
	for _, origin := range publicOrigins {
		origins[origin] = true
	}
	return origins
}

func addHostHeadersFromOrigin(hosts map[string]bool, origin string) {
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Host == "" {
		return
	}
	hosts[u.Host] = true
	if u.Port() != "" {
		return
	}
	defaultPort := "80"
	if u.Scheme == "https" {
		defaultPort = "443"
	}
	hosts[net.JoinHostPort(u.Hostname(), defaultPort)] = true
}

// isBindAll returns true when the server is listening on all
// interfaces (0.0.0.0 or ::), meaning LAN clients may connect
// via the machine's real IP.
func isBindAll(host string) bool {
	return host == "0.0.0.0" || host == "::"
}

// isAllowedBindAllHost returns true for Host header values that are
// local-interface IP literals on the server's configured port.
func isAllowedBindAllHost(
	hostHeader string, port int, allowedIPs map[string]bool,
) bool {
	host, ok := parseHostHeader(hostHeader, port)
	if !ok {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return allowedIPs[ip.String()]
}

// parseHostHeader validates and normalizes an HTTP Host header for
// the configured server port, returning the host portion.
func parseHostHeader(hostHeader string, port int) (string, bool) {
	if hostHeader == "" {
		return "", false
	}
	host, gotPort, err := net.SplitHostPort(hostHeader)
	if err == nil {
		return host, gotPort == strconv.Itoa(port)
	}
	// Browsers may omit :80 from Host for default HTTP port.
	if port != 80 {
		return "", false
	}
	host = hostHeader
	// Strip IPv6 brackets for ParseIP.
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host, true
}

// localInterfaceIPs returns canonical IP strings assigned to local
// network interfaces (including loopback).
func localInterfaceIPs() map[string]bool {
	ips := map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil {
				continue
			}
			ips[ip.String()] = true
		}
	}
	return ips
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	listenCtx := context.Background()
	if s.baseCtx != nil {
		listenCtx = s.baseCtx
	}
	ln, err := daemon.Listen(
		listenCtx,
		daemon.Endpoint{
			Network: daemon.NetworkTCP,
			Address: addr,
		},
		daemon.WithRuntimeStore(daemon.RuntimeStore{Dir: s.dataDir}),
	)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve starts the HTTP server on an existing listener.
func (s *Server) Serve(ln net.Listener) error {
	addr := ln.Addr().String()
	srv := &http.Server{
		Addr:        addr,
		Handler:     s.Handler(),
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second,
	}
	if s.baseCtx != nil {
		ctx := s.baseCtx
		srv.BaseContext = func(_ net.Listener) context.Context {
			return ctx
		}
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()
	log.Printf("Starting server at http://%s", addr)
	return srv.Serve(ln)
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.httpSrv
	s.mu.RUnlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// FindAvailablePort finds an available port starting from the
// given port, binding to the specified host.
func FindAvailablePort(host string, start int) int {
	if start == 0 {
		addr := net.JoinHostPort(host, "0")
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			defer ln.Close()
			if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
				return tcpAddr.Port
			}
		}
		return start
	}

	for port := start; port < start+100; port++ {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return port
		}
	}
	return start
}

// isMutating returns true for HTTP methods that change state.
func isMutating(method string) bool {
	return method == http.MethodPost ||
		method == http.MethodPut ||
		method == http.MethodPatch ||
		method == http.MethodDelete
}

func corsMiddleware(
	allowedOrigins map[string]bool, bindAll bool, port int, allowedIPs map[string]bool, next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			origin := r.Header.Get("Origin")

			// Authenticated remote requests: allow any origin.
			if isRemoteAuth(r) {
				if origin != "" {
					w.Header().Set(
						"Access-Control-Allow-Origin", origin,
					)
				}
				ensureVaryHeader(w.Header(), "Origin")
				w.Header().Set(
					"Access-Control-Allow-Methods",
					"GET, POST, PUT, PATCH, DELETE, OPTIONS",
				)
				w.Header().Set(
					"Access-Control-Allow-Headers",
					"Content-Type, Authorization",
				)
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// For reads (GET/HEAD), allow empty Origin (same-origin
			// requests often omit it). For mutating methods and
			// preflights, require Origin to be present and allowed.
			originAllowed := allowedOrigins[origin]
			// In bind-all mode, allow local-interface IP-literal
			// origins on the configured port so LAN UI access works
			// without opening wildcard cross-origin access.
			if !originAllowed && bindAll {
				originAllowed = isAllowedBindAllOrigin(origin, port, allowedIPs)
			}
			safeForReads := origin == "" || originAllowed

			if originAllowed {
				w.Header().Set(
					"Access-Control-Allow-Origin", origin,
				)
			}
			// Always set Vary so caches don't serve a
			// response without CORS headers to a
			// legitimate origin.
			ensureVaryHeader(w.Header(), "Origin")
			w.Header().Set(
				"Access-Control-Allow-Methods",
				"GET, POST, PUT, PATCH, DELETE, OPTIONS",
			)
			w.Header().Set(
				"Access-Control-Allow-Headers",
				"Content-Type, Authorization",
			)
			if r.Method == http.MethodOptions {
				if !safeForReads {
					http.Error(
						w, "Forbidden", http.StatusForbidden,
					)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// Block state-changing requests unless Origin
			// is present and recognized. This prevents
			// CSRF via simple requests (e.g., <form> POST)
			// and DNS rebinding where Origin is absent.
			if !originAllowed && isMutating(r.Method) {
				http.Error(
					w, "Forbidden", http.StatusForbidden,
				)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isAllowedBindAllOrigin returns true when Origin is an http://
// local-interface IP-literal origin using the configured server port.
func isAllowedBindAllOrigin(origin string, port int, allowedIPs map[string]bool) bool {
	u, err := url.Parse(origin)
	if err != nil || u == nil {
		return false
	}
	if u.Scheme != "http" || u.Host == "" {
		return false
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return false
	}
	gotPort := u.Port()
	portOK := false
	if port == 80 {
		portOK = gotPort == "" || gotPort == "80"
	} else {
		portOK = gotPort == strconv.Itoa(port)
	}
	if !portOK {
		return false
	}
	return allowedIPs[ip.String()]
}

// ensureVaryHeader appends token to Vary if not already present,
// preserving any existing Vary values.
func ensureVaryHeader(h http.Header, token string) {
	if token == "" {
		return
	}
	seen := make(map[string]bool)
	values := make([]string, 0, 4)
	for _, vary := range h.Values("Vary") {
		for part := range strings.SplitSeq(vary, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			key := strings.ToLower(p)
			if seen[key] {
				continue
			}
			seen[key] = true
			values = append(values, p)
		}
	}
	tokenKey := strings.ToLower(token)
	if !seen[tokenKey] {
		values = append(values, token)
	}
	if len(values) == 0 {
		return
	}
	h.Set("Vary", strings.Join(values, ", "))
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
