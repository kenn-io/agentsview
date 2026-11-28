package catalog

import (
	"net/http"
	"time"
)

// Transport carries every catalog download; nil uses http.DefaultTransport.
// Test processes set it to serve fixed catalogs without replacing the
// default transport that other HTTP clients clone.
var Transport http.RoundTripper

func newClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: Transport}
}
