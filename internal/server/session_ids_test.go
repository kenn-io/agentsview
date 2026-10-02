package server_test

import (
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestListSessionsIDsHTTP(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "codex:shared", "app", 2, func(s *db.Session) { s.UserMessageCount = 1 })
	te.seedSession(t, "node-a~codex:shared", "app", 2, func(s *db.Session) { s.Machine = "node-a"; s.IsAutomated = true; s.UserMessageCount = 1 })
	te.seedSession(t, "node-b~codex:shared", "other", 0, func(s *db.Session) {
		s.Machine = "node-b"
		s.RelationshipType = "subagent"
		s.ParentSessionID = new("unrelated")
	})
	te.seedSession(t, `openclaw:main:part,"quoted"`, "app", 1)
	te.seedSession(t, "unrelated", "app", 3)
	cases := []struct {
		name, query string
		want        []string
	}{
		{"raw", "ids=codex:shared", []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared"}},
		{"qualified", "ids=node-a~codex:shared", []string{"node-a~codex:shared"}},
		{"unknown", "ids=missing", []string{}},
		{"intersection", "ids=codex:shared&project=app", []string{"codex:shared", "node-a~codex:shared"}},
		{"duplicates whitespace", "ids=" + url.QueryEscape(" codex:shared , node-a~codex:shared,missing,codex:shared"), []string{"codex:shared", "node-a~codex:shared", "node-b~codex:shared"}},
		{"quoted comma and quote", "ids=" + url.QueryEscape(`"openclaw:main:part,""quoted"""`), []string{`openclaw:main:part,"quoted"`}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, "/api/v1/sessions?"+tt.query)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var page db.SessionPage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			ids := make([]string, 0, len(page.Sessions))
			for _, s := range page.Sessions {
				ids = append(ids, s.ID)
			}
			assert.ElementsMatch(t, tt.want, ids)
		})
	}
	for _, ids := range []string{"", ",", "codex:shared,", ",codex:shared", "codex:shared, ,missing", "openclaw:main:bad\r\nid", strings.Repeat("x,", 100) + "x"} {
		t.Run("invalid_"+ids, func(t *testing.T) {
			w := te.get(t, "/api/v1/sessions?ids="+url.QueryEscape(ids))
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
	t.Run("handwritten HTTP client", func(t *testing.T) {
		remote := httptest.NewUnstartedServer(nil)
		te.srv.SetPort(remote.Listener.Addr().(*net.TCPAddr).Port)
		remote.Config.Handler = te.srv.Handler()
		remote.Start()
		t.Cleanup(remote.Close)
		backend := servicehttp.NewHTTPBackend(remote.URL, "", false, "")
		page, err := backend.List(t.Context(), service.ListFilter{IDs: []string{"node-a~codex:shared"}})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, "node-a~codex:shared", page.Sessions[0].ID)

		page, err = backend.List(t.Context(), service.ListFilter{IDs: []string{`openclaw:main:part,"quoted"`}})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, `openclaw:main:part,"quoted"`, page.Sessions[0].ID)
	})
}
