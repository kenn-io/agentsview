package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestFrictionPublicURLIncludesBasePath(t *testing.T) {
	tests := []struct {
		name, publicURL, basePath, want string
	}{
		{"no_public_url", "", "/agentsview", ""},
		{"no_base_path", "https://example.com/", "", "https://example.com"},
		{"adds_base_path", "https://example.com", "/agentsview", "https://example.com/agentsview"},
		{"host_matches_base_path", "https://agentsview", "/agentsview", "https://agentsview/agentsview"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{cfg: config.Config{PublicURL: tt.publicURL}, basePath: tt.basePath}
			assert.Equal(t, tt.want, s.frictionPublicURL())
		})
	}
}
