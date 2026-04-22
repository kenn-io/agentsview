package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestAssetsFallbackIncludesPlaceholderIndex(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	raw, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatalf("ReadFile(index.html): %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "AgentsView frontend assets are not built.") {
		t.Fatalf("fallback body missing placeholder heading: %s", body)
	}
}
