package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestResolveDuckDBPushProjects(t *testing.T) {
	tests := []struct {
		name        string
		duck        config.DuckDBConfig
		cfg         DuckDBPushConfig
		wantInclude []string
		wantExclude []string
		wantErr     bool
	}{
		{
			name:        "config include used when no flags",
			duck:        config.DuckDBConfig{Projects: []string{"a", "b"}},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			duck:        config.DuckDBConfig{ExcludeProjects: []string{"x"}},
			cfg:         DuckDBPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name: "all-projects clears both",
			duck: config.DuckDBConfig{Projects: []string{"a"}},
			cfg:  DuckDBPushConfig{AllProjects: true},
		},
		{
			name:    "both flags is an error",
			cfg:     DuckDBPushConfig{ProjectsFlag: "a", ExcludeProjects: "b"},
			wantErr: true,
		},
		{
			name:    "all-projects with include is an error",
			cfg:     DuckDBPushConfig{AllProjects: true, ProjectsFlag: "a"},
			wantErr: true,
		},
		{
			name: "config has both projects and exclude is an error",
			duck: config.DuckDBConfig{
				Projects:        []string{"a"},
				ExcludeProjects: []string{"x"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc, exc, err := resolveDuckDBPushProjects(tt.duck, tt.cfg)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantInclude, inc)
			assert.Equal(t, tt.wantExclude, exc)
		})
	}
}

func TestResolveQuackServeToken(t *testing.T) {
	generateErr := errors.New("generate failed")
	tests := []struct {
		name       string
		flagToken  string
		configured string
		generated  string
		genErr     error
		wantToken  string
		wantGen    bool
		wantErr    bool
	}{
		{
			name:       "flag token wins",
			flagToken:  "flag-token",
			configured: "config-token",
			generated:  "generated-token",
			wantToken:  "flag-token",
		},
		{
			name:       "configured token used before generation",
			configured: "config-token",
			generated:  "generated-token",
			wantToken:  "config-token",
		},
		{
			name:      "generates token when none configured",
			generated: "generated-token",
			wantToken: "generated-token",
			wantGen:   true,
		},
		{
			name:    "generator error returned",
			genErr:  generateErr,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			token, generated, err := resolveQuackServeToken(
				tt.flagToken, tt.configured,
				func() (string, error) {
					called = true
					return tt.generated, tt.genErr
				},
			)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, called)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantToken, token)
			assert.Equal(t, tt.wantGen, generated)
			assert.Equal(t, tt.wantGen, called)
		})
	}
}
