package docbank

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

const (
	testHash     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testVersion  = "11111111-1111-4111-8111-111111111111"
	otherVersion = "22222222-2222-4222-8222-222222222222"
)

func rawItem(node int, version string) map[string]any {
	return map[string]any{"version": map[string]any{"id": version, "node_id": node, "blob_hash": testHash, "size": 7}, "node": map[string]any{"id": node, "kind": "file", "current_version_id": version, "name": "example.txt"}, "path": "/docs/example.txt", "is_current": true}
}

func rawPage(items ...map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"items": items, "total": len(items), "limit": 100, "offset": 0}
}

func resolverFor(t *testing.T, h http.HandlerFunc) (Resolver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	r, err := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: srv.URL + "/prefix", APIKeyEnv: "DOCBANK_TEST_KEY", WebURL: "http://docbank-test.docbank.localhost:8081"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r, srv
}

func TestDocbankOnlyDigestLeavesProcess(t *testing.T) {
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/prefix/api/v1/content-references", r.URL.Path)
		assert.Equal(t, "limit=100&offset=0&sha256="+testHash, r.URL.RawQuery)
		assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		assert.Zero(t, r.ContentLength)
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage(rawItem(9, testVersion))))
	})
	p, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, err)
	require.Len(t, p.Matches, 1)
	assert.Equal(t, "found", p.State)
	assert.Equal(t, "current", p.Matches[0].State)
	assert.Equal(t, "http://docbank-test.docbank.localhost:8081/#node=9&version="+testVersion, p.Matches[0].OpenURL)
}

func TestDocbankRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	p, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, err)
	assert.Equal(t, "unavailable", p.State)
	assert.Zero(t, leaked.Load())
}

func TestDocbankIgnoresInheritedProxy(t *testing.T) {
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	var proxyCalls atomic.Int32
	transport.Proxy = func(*http.Request) (*url.URL, error) {
		proxyCalls.Add(1)
		return nil, nil
	}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		transport.CloseIdleConnections()
	})
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage(rawItem(9, testVersion))))
	})
	p, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, err)
	assert.Equal(t, "found", p.State)
	assert.Zero(t, proxyCalls.Load())
}

func TestDocbankMalformedPage(t *testing.T) {
	for _, tc := range []string{"hash", "size", "pair", "current", "id", "missing_total", "negative_total", "offset", "empty", "long_string", "trailing", "too_many", "missing_current", "missing_size", "missing_items", "null_items"} {
		t.Run(tc, func(t *testing.T) {
			item := rawItem(9, testVersion)
			page := rawPage(item)
			v := item["version"].(map[string]any)
			n := item["node"].(map[string]any)
			switch tc {
			case "hash":
				v["blob_hash"] = strings.Repeat("b", 64)
			case "size":
				v["size"] = 8
			case "pair":
				v["node_id"] = 10
			case "current":
				item["is_current"] = false
			case "id":
				v["id"] = "x#secret"
			case "missing_total":
				delete(page, "total")
			case "negative_total":
				page["total"] = -1
			case "offset":
				page["offset"] = 1
			case "empty":
				page["items"] = []any{}
			case "long_string":
				n["name"] = strings.Repeat("x", 4097)
			case "too_many":
				page["total"] = 101
				a := make([]any, 101)
				for i := range a {
					a[i] = item
				}
				page["items"] = a
			case "missing_current":
				delete(item, "is_current")
			case "missing_size":
				delete(v, "size")
			case "missing_items":
				delete(page, "items")
				page["total"] = 0
			case "null_items":
				page["items"] = nil
				page["total"] = 0
			}
			r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				if !assert.NoError(t, json.NewEncoder(w).Encode(page)) {
					return
				}
				if tc == "trailing" {
					_, _ = fmt.Fprint(w, `{}`)
				}
			})
			p, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
			require.NoError(t, err)
			assert.Equal(t, "unavailable", p.State)
			assert.Empty(t, p.Matches)
			assert.Nil(t, p.ReferenceTotal)
		})
	}
}

func TestDocbankPagination(t *testing.T) {
	item := rawItem(1, testVersion)
	prior := rawItem(2, otherVersion)
	prior["node"].(map[string]any)["current_version_id"] = testVersion
	prior["is_current"] = false
	trash := rawItem(3, testVersion)
	trash["node"].(map[string]any)["trashed_at"] = "2026-01-01T00:00:00Z"
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		p := rawPage(item, item, prior, trash)
		p["total"] = 104
		assert.NoError(t, json.NewEncoder(w).Encode(p))
	})
	p, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, err)
	require.Len(t, p.Matches, 3)
	assert.Equal(t, []int{0, 2, 3}, p.MatchOffsets)
	assert.Equal(t, 4, *p.NextReferenceOffset)
	assert.Equal(t, "prior", p.Matches[1].State)
	assert.Equal(t, "trash", p.Matches[2].State)
	assert.Empty(t, p.Matches[2].Path)
	// The cached page retains raw positions and is returned as an independent copy.
	p.Matches[0].Name = "changed"
	p.MatchOffsets[0] = 99
	*p.ReferenceTotal = 999
	again, err := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, err)
	assert.Equal(t, "example.txt", again.Matches[0].Name)
	assert.Equal(t, []int{0, 2, 3}, again.MatchOffsets)
	assert.Equal(t, 104, *again.ReferenceTotal)
}

func TestDocbankExhaustedPage(t *testing.T) {
	for _, total := range []int{0, 1, 100} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				p := rawPage()
				p["total"] = total
				p["offset"] = 100
				assert.NoError(t, json.NewEncoder(w).Encode(p))
			})
			p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7, ReferenceOffset: 100})
			require.NoError(t, e)
			assert.Equal(t, "found", p.State)
			assert.Empty(t, p.Matches)
			assert.Nil(t, p.NextReferenceOffset)
			assert.Equal(t, total, *p.ReferenceTotal)
		})
	}
}

func FuzzDocbankPage(f *testing.F) {
	f.Add([]byte(`{"items":[],"total":0,"limit":100,"offset":0}`), 0)
	seed, err := json.Marshal(rawPage(rawItem(1, testVersion)))
	require.NoError(f, err)
	f.Add(seed, 0)
	f.Add([]byte(`{"items":[],"total":3,"limit":100,"offset":100}`), 100)
	f.Fuzz(func(t *testing.T, data []byte, offset int) {
		if offset < 0 || len(data) > maxPageBytes+1 {
			t.Skip()
		}
		c := newClient(config.DocbankIntegration{URL: "https://example.test"})
		c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
		})
		p, _ := c.fetch(t.Context(), Lookup{SHA256: testHash, Size: 7, ReferenceOffset: offset}, "test-key", time.Now())
		assert.Len(t, p.MatchOffsets, len(p.Matches))
		assert.LessOrEqual(t, len(p.Matches), 100)
		if p.State == "not_found" {
			require.NotNil(t, p.ReferenceTotal)
			assert.Zero(t, *p.ReferenceTotal)
			assert.Zero(t, offset)
		}
		for i, m := range p.Matches {
			assert.Equal(t, testHash, m.SHA256)
			assert.EqualValues(t, 7, m.Size)
			assert.GreaterOrEqual(t, p.MatchOffsets[i], offset)
			assert.Less(t, p.MatchOffsets[i]-offset, 100)
			if i > 0 {
				assert.Greater(t, p.MatchOffsets[i], p.MatchOffsets[i-1])
			}
		}
		if p.NextReferenceOffset != nil {
			assert.Greater(t, *p.NextReferenceOffset, offset)
			require.NotNil(t, p.ReferenceTotal)
			assert.Less(t, *p.NextReferenceOffset, *p.ReferenceTotal)
		}
	})
}

func TestDocbankDecodedPageCap(t *testing.T) {
	body := `{"items":[],"total":0,"limit":100,"offset":0}`
	for _, extra := range []int{0, 1} {
		t.Run(strconv.Itoa(extra), func(t *testing.T) {
			r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				z := gzip.NewWriter(w)
				_, _ = io.WriteString(z, strings.Repeat(" ", (1<<20)-len(body)+extra)+body)
				assert.NoError(t, z.Close())
			})
			p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
			require.NoError(t, e)
			expected := "not_found"
			if extra != 0 {
				expected = "unavailable"
			}
			assert.Equal(t, expected, p.State)
		})
	}
}

func TestDocbankVerifiedLoopbackDial(t *testing.T) {
	for _, loopback := range []bool{true, false} {
		t.Run(strconv.FormatBool(loopback), func(t *testing.T) {
			// The DNS boundary returns either an owned listener's address or unspecified
			// IPv4 (which must be rejected before dialing). No external DNS is used.
			var listen net.ListenConfig
			dns, e := listen.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
			require.NoError(t, e)
			defer dns.Close()
			var queries atomic.Int32
			go func() {
				buf := make([]byte, 4096)
				for {
					n, addr, e := dns.ReadFrom(buf)
					if e != nil {
						return
					}
					var m dnsmessage.Message
					if m.Unpack(buf[:n]) != nil {
						continue
					}
					queries.Add(1)
					reply := dnsmessage.Message{ID: m.ID, Response: true, RecursionAvailable: true, Questions: m.Questions}
					for _, q := range m.Questions {
						if q.Type == dnsmessage.TypeA {
							ip := [4]byte{127, 0, 0, 1}
							if !loopback {
								ip = [4]byte{0, 0, 0, 0}
							}
							reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: ip}})
						}
					}
					b, e := reply.Pack()
					if e == nil {
						_, _ = dns.WriteTo(b, addr)
					}
				}
			}()
			saved := net.DefaultResolver
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "udp", dns.LocalAddr().String())
			}}
			defer func() { net.DefaultResolver = saved }()
			var calls atomic.Int32
			_, srv := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
				assert.NoError(t, json.NewEncoder(w).Encode(rawPage()))
			})
			u, e := url.Parse(srv.URL)
			require.NoError(t, e)
			r, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: "http://resolver-test.localhost:" + u.Port(), APIKeyEnv: "DOCBANK_TEST_KEY"})
			require.NoError(t, e)
			defer r.Close()
			p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
			require.NoError(t, e)
			if loopback {
				assert.Equal(t, "not_found", p.State)
				assert.EqualValues(t, 1, calls.Load())
			} else {
				assert.Equal(t, "unavailable", p.State)
				assert.Zero(t, calls.Load())
			}
			assert.Positive(t, queries.Load())
		})
	}
}

func TestDocbankLoopbackAddressFallback(t *testing.T) {
	v4, v6, external := net.IPAddr{IP: net.ParseIP("127.0.0.1")}, net.IPAddr{IP: net.ParseIP("::1")}, net.IPAddr{IP: net.ParseIP("192.0.2.1")}
	for _, tc := range []struct {
		name                string
		ips                 []net.IPAddr
		failures, wantCalls int
		cancel, expire      bool
	}{
		{"IPv4 then IPv6", []net.IPAddr{v4, v6}, 1, 2, false, false},
		{"IPv6 then IPv4", []net.IPAddr{v6, v4}, 1, 2, false, false},
		{"first success", []net.IPAddr{v4, v6}, 0, 1, false, false},
		{"all fail", []net.IPAddr{v4, v6}, 2, 2, false, false},
		{"verify all first", []net.IPAddr{v4, external}, 0, 0, false, false},
		{"cancel", []net.IPAddr{v4, v6}, 1, 1, true, false},
		{"shared deadline", []net.IPAddr{v4, v6}, 1, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				started := time.Now()
				calls := 0
				failure := errors.New("owned dial failure")
				var peer net.Conn
				conn, err := dialLoopback(ctx, "tcp", "8080", tc.ips, func(got context.Context, network, address string) (net.Conn, error) {
					assert.Same(t, ctx, got)
					assert.Equal(t, "tcp", network)
					assert.Equal(t, net.JoinHostPort(tc.ips[calls].String(), "8080"), address)
					calls++
					if tc.cancel {
						cancel()
					}
					if tc.expire {
						<-got.Done()
					}
					if calls <= tc.failures {
						return nil, failure
					}
					a, b := net.Pipe()
					peer = b
					return a, nil
				})
				assert.Equal(t, tc.wantCalls, calls)
				switch {
				case tc.cancel:
					require.ErrorIs(t, err, context.Canceled)
				case tc.expire:
					require.ErrorIs(t, err, context.DeadlineExceeded)
					assert.Equal(t, 2*time.Second, time.Since(started))
				case tc.wantCalls == 0:
					require.ErrorIs(t, err, errRemote)
				case tc.failures == 2:
					require.ErrorIs(t, err, failure)
				default:
					require.NoError(t, err)
					require.NotNil(t, conn)
					require.NoError(t, conn.Close())
					require.NoError(t, peer.Close())
				}
			})
		})
	}
}
