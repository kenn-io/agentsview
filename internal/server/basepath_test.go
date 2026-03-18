package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBasePath_StripsPrefixForAPI(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	req := httptest.NewRequest("GET", "/app/api/v1/sessions", nil)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	// 200 or 503 (timeout) both confirm the route was matched
	// and prefix was stripped. 404 or 403 would indicate a
	// base-path routing failure.
	if w.Code == http.StatusNotFound ||
		w.Code == http.StatusForbidden {
		t.Fatalf("GET /app/api/v1/sessions = %d, want route match; body: %s",
			w.Code, w.Body.String())
	}
}

func TestBasePath_RedirectsBarePrefix(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	req := httptest.NewRequest("GET", "/app", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /app = %d, want 301", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/app/" {
		t.Fatalf("Location = %q, want /app/", loc)
	}
}

func TestBasePath_InjectsBaseHrefIntoHTML(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/viewer"))

	req := httptest.NewRequest("GET", "/viewer/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /viewer/ = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<base href="/viewer/">`) {
		t.Error("missing <base href> tag in response")
	}
}

func TestBasePath_RewritesAssetPaths(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/viewer"))

	req := httptest.NewRequest("GET", "/viewer/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	body := w.Body.String()

	// Asset paths should be prefixed.
	if strings.Contains(body, `src="/assets/`) {
		t.Error("found unprefixed src=\"/assets/ in HTML")
	}
	if strings.Contains(body, `href="/assets/`) {
		t.Error("found unprefixed href=\"/assets/ in HTML")
	}
	if strings.Contains(body, `href="/favicon`) {
		t.Error("found unprefixed href=\"/favicon in HTML")
	}

	// External URLs must NOT be prefixed.
	if strings.Contains(body, `href="/viewer/https://`) {
		t.Error("external URL was incorrectly prefixed")
	}
}

func TestBasePath_SPAFallbackServesIndex(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	// A non-existent path should fall back to index.html
	// with the base tag injected.
	req := httptest.NewRequest("GET", "/app/some/route", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /app/some/route = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `<base href="/app/">`) {
		t.Error("SPA fallback missing <base href> tag")
	}
}

func TestBasePath_RejectsSiblingPath(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app"))

	// /appfoo should NOT be handled — only /app or /app/...
	req := httptest.NewRequest("GET", "/appfoo/bar", nil)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf(
			"GET /appfoo/bar = %d, want 404", w.Code,
		)
	}
}

func TestBasePath_TrailingSlashNormalized(t *testing.T) {
	s := testServer(t, 0, WithBasePath("/app/"))

	// WithBasePath trims trailing slash.
	if s.basePath != "/app" {
		t.Fatalf("basePath = %q, want /app", s.basePath)
	}
}
