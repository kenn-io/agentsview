// Package katatest is an in-process fake of the Kata HTTP API surface that
// agentsview uses. It records every request so tests can assert exact
// request sequences, and it models the idempotency rules that matter to
// filing (reused create, idempotency_mismatch, idempotency_deleted, comment
// keys, idempotent labels and reopen).
package katatest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kg "go.kenn.io/kata/pkg/client/generated"
)

const (
	DefaultProjectID   int64 = 17
	DefaultProjectName       = "agentsview"
	defaultProjectUID        = "01J00000000000000000000001"
)

type Project struct {
	ID              int64
	UID, Name       string
	Active, Deleted bool
}

type Issue struct {
	UID, ShortID, Title, Body, Status, ClosedReason string
	Priority                                        *int64
	Labels                                          []string
	Metadata                                        map[string]any
	Comments                                        []string
	ProjectID                                       int64
	Deleted                                         bool
}

type Request struct {
	Method, Path, RawQuery, IdempotencyKey, Authorization string
	Body                                                  []byte
}

type Fault struct {
	Status            int
	Code, Message     string
	Data              map[string]any
	RawBody, Location string
}

type (
	idemEntry    struct{ uid, digest string }
	commentEntry struct {
		actor, body, teammate string
		ordinal               int
	}
)

type Server struct {
	*httptest.Server
	endpoint   string
	socketPath string

	mu          sync.Mutex
	schema      string
	instanceUID string
	version     string
	token       string
	webOrigin   string
	projects    []Project
	issues      []*Issue
	requests    []Request
	faults      map[string][]Fault
	createIdem  map[string]idemEntry
	commentIdem map[string]commentEntry
	nextShort   int
}

var fixedTime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func newState() *Server {
	return &Server{
		schema:      "0.22.0",
		instanceUID: "01J00000000000000000000002",
		version:     "0.18.0",
		projects:    []Project{{ID: DefaultProjectID, UID: defaultProjectUID, Name: DefaultProjectName, Active: true}},
		faults:      map[string][]Fault{},
		createIdem:  map[string]idemEntry{},
		commentIdem: map[string]commentEntry{},
	}
}

// New starts a loopback HTTP fake.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := newState()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	s.endpoint = s.URL
	tb.Cleanup(s.Close)
	return s
}

// NewUnix starts the fake on a Unix socket and returns its unix:// endpoint.
func NewUnix(tb testing.TB) (*Server, string) {
	tb.Helper()
	//nolint:usetesting // Short paths keep Unix socket names within platform limits.
	dir, err := os.MkdirTemp("", "kt")
	require.NoError(tb, err, "katatest: temp dir")
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := new(net.ListenConfig).Listen(tb.Context(), "unix", sock)
	require.NoError(tb, err, "katatest: listen")
	s := newState()
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Listener = l
	s.Start()
	s.endpoint, s.socketPath = UnixEndpoint(sock), sock
	tb.Cleanup(s.Close)
	return s, s.endpoint
}

// UnixEndpoint encodes a native socket path as an absolute unix:// URL.
func UnixEndpoint(sock string) string {
	path := filepath.ToSlash(sock)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "unix", Path: path}).String()
}

func (s *Server) Endpoint() string { return s.endpoint }

func (s *Server) SetSchemaVersion(v string) { s.mu.Lock(); s.schema = v; s.mu.Unlock() }
func (s *Server) SetInstance(uid, version string) {
	s.mu.Lock()
	s.instanceUID, s.version = uid, version
	s.mu.Unlock()
}
func (s *Server) SetToken(token string)      { s.mu.Lock(); s.token = token; s.mu.Unlock() }
func (s *Server) SetWebOrigin(origin string) { s.mu.Lock(); s.webOrigin = origin; s.mu.Unlock() }
func (s *Server) SetProjects(ps ...Project) {
	s.mu.Lock()
	s.projects = slices.Clone(ps)
	s.mu.Unlock()
}

func (s *Server) AddIssue(is Issue) Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneIssue(*s.addIssueLocked(is))
}

func (s *Server) addIssueLocked(is Issue) *Issue {
	s.nextShort++
	if is.ShortID == "" {
		is.ShortID = fmt.Sprintf("f%03d", s.nextShort)
	}
	if is.UID == "" {
		is.UID = fmt.Sprintf("01J0ABCDEF%016d", s.nextShort) // Crockford base32: a valid ULID shape
	}
	if is.ProjectID == 0 {
		is.ProjectID = DefaultProjectID
	}
	if is.Status == "" {
		is.Status = "open"
	}
	if is.Metadata == nil {
		is.Metadata = map[string]any{}
	}
	cp := cloneIssue(is)
	s.issues = append(s.issues, &cp)
	return &cp
}

func (s *Server) Issue(uid string) (Issue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, is := range s.issues {
		if is.UID == uid {
			return cloneIssue(*is), true
		}
	}
	return Issue{}, false
}

func cloneIssue(is Issue) Issue {
	if is.Priority != nil {
		p := *is.Priority
		is.Priority = &p
	}
	is.Labels = slices.Clone(is.Labels)
	is.Comments = slices.Clone(is.Comments)
	if is.Metadata != nil {
		is.Metadata = cloneMap(is.Metadata)
	}
	return is
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = cloneValue(value)
		}
		return out
	case []string:
		return slices.Clone(x)
	case []byte:
		return slices.Clone(x)
	default:
		return v
	}
}

func actorValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func (s *Server) Fail(method, pathSuffix string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + pathSuffix
	if f.Data != nil {
		f.Data = cloneMap(f.Data)
	}
	s.faults[key] = append(s.faults[key], f)
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.requests)
	for i := range out {
		out[i].Body = slices.Clone(out[i].Body)
	}
	return out
}

func (s *Server) RequestsMatching(method, pathSuffix string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) ResetRequests() { s.mu.Lock(); s.requests = nil; s.mu.Unlock() }

var (
	reIssues     = regexp.MustCompile(`^/api/v1/projects/(\d+)/issues$`)
	reIssue      = regexp.MustCompile(`^/api/v1/projects/(\d+)/issues/([^/]+)$`)
	reIssueChild = regexp.MustCompile(`^/api/v1/projects/(\d+)/issues/([^/]+)/(actions/reopen|comments|labels)$`)
	reByUID      = regexp.MustCompile(`^/api/v1/issues/([^/]+)$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Authorization: r.Header.Get("Authorization"), Body: body,
	})
	if f, ok := s.popFaultLocked(r.Method, r.URL.Path); ok {
		writeFault(w, f)
		return
	}
	if s.token != "" && r.URL.Path != "/api/v1/health" {
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, "Bearer ") {
			writeErr(w, 401, "auth_required", "Authorization bearer required", nil)
			return
		}
		if strings.TrimPrefix(got, "Bearer ") != s.token {
			writeErr(w, 403, "auth_invalid", "token mismatch", nil)
			return
		}
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api/v1/health":
		h := map[string]any{"ok": true, "schema_version": 1, "version": s.version, "started_at": fixedTime, "uptime": "1s"}
		if s.schema != "" {
			h["api_schema_version"] = s.schema
		}
		writeJSON(w, 200, h)
	case r.Method == http.MethodGet && p == "/api/v1/instance":
		writeJSON(w, 200, map[string]any{"instance_uid": s.instanceUID, "version": s.version, "schema_version": 1, "auth": map[string]any{}, "web_ui_capabilities": map[string]any{}, "issue_subtree_tokens": false})
	case r.Method == http.MethodGet && p == "/api/v1/projects":
		out := make([]map[string]any, 0, len(s.projects))
		for _, pr := range s.projects {
			m := map[string]any{"id": pr.ID, "uid": pr.UID, "name": pr.Name, "active": pr.Active, "created_at": fixedTime, "metadata": map[string]any{}, "revision": 1}
			if pr.Deleted {
				m["deleted_at"] = fixedTime
			}
			out = append(out, m)
		}
		writeJSON(w, 200, map[string]any{"projects": out})
	case r.Method == http.MethodGet && p == "/api/v1/issues":
		s.listLocked(w, r, 0, true)
	case r.Method == http.MethodGet && reByUID.MatchString(p):
		uid := reByUID.FindStringSubmatch(p)[1]
		is := s.findLocked(0, uid)
		if is == nil {
			writeErr(w, 404, "issue_not_found", "issue not found", nil)
			return
		}
		writeJSON(w, 200, s.showLocked(is))
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && reIssues.MatchString(p):
		pid, ok := s.projectLocked(w, reIssues.FindStringSubmatch(p)[1])
		if !ok {
			return
		}
		if r.Method == http.MethodPost {
			s.createLocked(w, r, pid, body)
			return
		}
		s.listLocked(w, r, pid, false)
	case r.Method == http.MethodPost && reIssueChild.MatchString(p):
		m := reIssueChild.FindStringSubmatch(p)
		pid, ok := s.projectLocked(w, m[1])
		if !ok {
			return
		}
		is := s.findLocked(pid, m[2])
		if is == nil {
			writeErr(w, 404, "issue_not_found", "issue not found", nil)
			return
		}
		switch m[3] {
		case "actions/reopen":
			var req kg.ActionRequestBody
			if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(true)); err != nil {
				writeErr(w, 400, "validation", err.Error(), nil)
				return
			}
			changed := is.Status != "open"
			is.Status, is.ClosedReason = "open", ""
			writeJSON(w, 200, map[string]any{"changed": changed, "event": nil, "issue": s.issueJSONLocked(is)})
		case "comments":
			var req kg.CommentRequestBody
			if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(true)); err != nil || strings.TrimSpace(req.Body) == "" {
				writeErr(w, 400, "validation", "comment body is required", nil)
				return
			}
			key := r.Header.Get("Idempotency-Key")
			if key != "" {
				k := is.UID + "\x00" + key
				if prev, seen := s.commentIdem[k]; seen {
					if prev.actor != actorValue(req.Actor) || prev.body != req.Body || prev.teammate != actorValue(req.Teammate) {
						writeErr(w, 409, "idempotency_mismatch", "comment key reused with a different body", nil)
						return
					}
					writeJSON(w, 200, map[string]any{"changed": false, "event": nil, "issue": s.issueJSONLocked(is), "comment": commentJSON(prev.body, prev.ordinal)})
					return
				}
				s.commentIdem[k] = commentEntry{actor: actorValue(req.Actor), body: req.Body, teammate: actorValue(req.Teammate), ordinal: len(is.Comments) + 1}
			}
			is.Comments = append(is.Comments, req.Body)
			writeJSON(w, 200, map[string]any{"changed": true, "event": nil, "issue": s.issueJSONLocked(is), "comment": commentJSON(req.Body, len(is.Comments))})
		case "labels":
			var req kg.AddLabelRequestBody
			if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(true)); err != nil || req.Label == "" {
				writeErr(w, 400, "validation", "label is required", nil)
				return
			}
			changed := !slices.Contains(is.Labels, req.Label)
			if changed {
				is.Labels = append(is.Labels, req.Label)
			}
			writeJSON(w, 200, map[string]any{"changed": changed, "event": nil, "issue": s.issueJSONLocked(is), "label": map[string]any{"author": "agentsview", "created_at": fixedTime, "issue_id": 1, "label": req.Label}})
		}
	case r.Method == http.MethodGet && reIssue.MatchString(p):
		m := reIssue.FindStringSubmatch(p)
		pid, ok := s.projectLocked(w, m[1])
		if !ok {
			return
		}
		is := s.findLocked(pid, m[2])
		if is == nil {
			writeErr(w, 404, "issue_not_found", "issue not found", nil)
			return
		}
		writeJSON(w, 200, s.showLocked(is))
	default:
		writeErr(w, 404, "not_found", "route not found", nil)
	}
}

func (s *Server) popFaultLocked(method, path string) (Fault, bool) {
	for key, queue := range s.faults {
		m, suffix, _ := strings.Cut(key, " ")
		if m == method && strings.HasSuffix(path, suffix) && len(queue) > 0 {
			s.faults[key] = queue[1:]
			return queue[0], true
		}
	}
	return Fault{}, false
}

func (s *Server) projectLocked(w http.ResponseWriter, raw string) (int64, bool) {
	id, _ := strconv.ParseInt(raw, 10, 64)
	for _, pr := range s.projects {
		if pr.ID == id && pr.Active && !pr.Deleted {
			return id, true
		}
	}
	writeErr(w, 404, "project_not_found", "project not found", nil)
	return 0, false
}

func (s *Server) projectName(pid int64) string {
	for _, pr := range s.projects {
		if pr.ID == pid {
			return pr.Name
		}
	}
	return DefaultProjectName
}

// findLocked matches a UID, short id or project#short ref. pid 0 = any project.
func (s *Server) findLocked(pid int64, ref string) *Issue {
	for _, is := range s.issues {
		if pid != 0 && is.ProjectID != pid {
			continue
		}
		q := s.projectName(is.ProjectID) + "#" + is.ShortID
		if !is.Deleted && (is.UID == ref || is.ShortID == ref || q == ref) {
			return is
		}
	}
	return nil
}

func (s *Server) listLocked(w http.ResponseWriter, r *http.Request, pid int64, global bool) {
	q := r.URL.Query()
	out := []map[string]any{}
	for _, is := range s.issues {
		if is.Deleted || (pid != 0 && is.ProjectID != pid) {
			continue
		}
		if st := q.Get("status"); st != "" && is.Status != st {
			continue
		}
		if !metaMatches(is, q["meta"]) {
			continue
		}
		m := s.issueOutLocked(is)
		if global {
			m["project_name"] = s.projectName(is.ProjectID)
		}
		out = append(out, m)
	}
	writeJSON(w, 200, map[string]any{"issues": out})
}

func metaMatches(is *Issue, filters []string) bool {
	for _, f := range filters {
		key, value, hasValue := strings.Cut(f, "=")
		got, present := is.Metadata[key]
		if !present {
			return false
		}
		if hasValue {
			str, ok := got.(string)
			if !ok || str != value {
				return false
			}
		}
	}
	return true
}

func (s *Server) createLocked(w http.ResponseWriter, r *http.Request, pid int64, body []byte) {
	var req kg.CreateIssueRequestBody
	if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(true)); err != nil {
		writeErr(w, 400, "validation", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeErr(w, 400, "validation", "title is required", nil)
		return
	}
	if req.Priority != nil && (*req.Priority < 0 || *req.Priority > 4) {
		writeErr(w, 400, "validation", "priority must be 0..4", nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	fingerprintReq := req
	fingerprintReq.Actor = nil    // the idempotency fingerprint ignores the actor
	fingerprintReq.ForceNew = nil // force_new affects look-alike checks, not idempotency
	fingerprintReq.Labels = slices.Clone(req.Labels)
	slices.Sort(fingerprintReq.Labels)
	canon, _ := json.Marshal(fingerprintReq, json.Deterministic(true))
	sum := sha256.Sum256(canon)
	digest := hex.EncodeToString(sum[:])
	if key != "" {
		if prev, seen := s.createIdem[strconv.FormatInt(pid, 10)+"\x00"+key]; seen {
			prevIssue := s.findLocked(0, prev.uid)
			if prevIssue == nil {
				writeErr(w, 409, "idempotency_deleted", "idempotency key matches a deleted issue", nil)
				return
			}
			if prev.digest != digest {
				writeErr(w, 409, "idempotency_mismatch", "idempotency key reused with a different payload",
					map[string]any{"uid": prevIssue.UID, "short_id": prevIssue.ShortID, "qualified_id": s.projectName(pid) + "#" + prevIssue.ShortID})
				return
			}
			writeJSON(w, 200, map[string]any{"changed": false, "reused": true, "event": nil, "issue": s.issueJSONLocked(prevIssue)})
			return
		}
	}
	bodyText := ""
	if req.Body != nil {
		bodyText = *req.Body
	}
	is := s.addIssueLocked(Issue{Title: req.Title, Body: bodyText, Status: "open", Priority: req.Priority, Labels: req.Labels, Metadata: req.Metadata, ProjectID: pid})
	if key != "" {
		s.createIdem[strconv.FormatInt(pid, 10)+"\x00"+key] = idemEntry{uid: is.UID, digest: digest}
	}
	writeJSON(w, 200, map[string]any{"changed": true, "event": nil, "issue": s.issueJSONLocked(is)})
}

func (s *Server) issueJSONLocked(is *Issue) map[string]any {
	m := map[string]any{
		"id": 1, "uid": is.UID, "project_id": is.ProjectID, "short_id": is.ShortID, "title": is.Title,
		"body": is.Body, "status": is.Status, "author": "agentsview", "metadata": is.Metadata,
		"revision": 1, "created_at": fixedTime, "updated_at": fixedTime,
	}
	if is.ClosedReason != "" {
		m["closed_reason"] = is.ClosedReason
	}
	if is.Priority != nil {
		m["priority"] = *is.Priority
	}
	return m
}

func (s *Server) issueOutLocked(is *Issue) map[string]any {
	m := s.issueJSONLocked(is)
	m["qualified_id"] = s.projectName(is.ProjectID) + "#" + is.ShortID
	m["labels"] = slices.Clone(is.Labels)
	if s.webOrigin != "" {
		m["web_url"] = s.webOrigin + "/issues/" + is.UID
	}
	return m
}

func (s *Server) showLocked(is *Issue) map[string]any {
	labels := []map[string]any{}
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"author": "agentsview", "created_at": fixedTime, "issue_id": 1, "label": l})
	}
	comments := make([]map[string]any, 0, len(is.Comments))
	for i, body := range is.Comments {
		comments = append(comments, commentJSON(body, i+1))
	}
	m := map[string]any{"issue": s.issueJSONLocked(is), "comments": comments, "labels": labels, "links": []any{}}
	if s.webOrigin != "" {
		m["web_url"] = s.webOrigin + "/issues/" + is.UID
	}
	return m
}

func commentJSON(body string, number int) map[string]any {
	return map[string]any{"author": "agentsview", "body": body, "created_at": fixedTime, "id": number, "issue_id": 1, "uid": fmt.Sprintf("01J0C%021d", number)}
}

func writeFault(w http.ResponseWriter, f Fault) {
	if f.Location != "" {
		w.Header().Set("Location", f.Location)
	}
	if f.RawBody != "" || f.Code == "" {
		w.WriteHeader(f.Status)
		_, _ = io.Copy(w, bytes.NewBufferString(f.RawBody))
		return
	}
	writeErr(w, f.Status, f.Code, f.Message, f.Data)
}

func writeErr(w http.ResponseWriter, status int, code, message string, data map[string]any) {
	body := map[string]any{"code": code, "message": message}
	if data != nil {
		body["data"] = data
	}
	writeJSON(w, status, map[string]any{"status": status, "error": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, v)
}
