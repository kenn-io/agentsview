package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestAssetsIncludesIndex(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	if _, err := fs.ReadFile(assets, "index.html"); err != nil {
		t.Fatalf("ReadFile(index.html): %v", err)
	}
}

func TestFallbackAssetsIncludePlaceholderIndex(t *testing.T) {
	fallback, err := fs.Sub(assetFS, "fallback")
	if err != nil {
		t.Fatalf("fs.Sub(fallback): %v", err)
	}

	raw, err := fs.ReadFile(fallback, "index.html")
	if err != nil {
		t.Fatalf("ReadFile(fallback/index.html): %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "AgentsView frontend assets are not built.") {
		t.Fatalf("fallback body missing placeholder heading: %s", body)
	}
}
