package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type sourceMissingPolicyProvider struct {
	parser.ProviderBase
	validVirtual bool
}

func (sourceMissingPolicyProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func (p sourceMissingPolicyProvider) PersistentArchiveSource(
	string, string,
) (string, bool) {
	if p.validVirtual {
		return "state.db", true
	}
	return "", false
}

func TestPreserveConfiguredMissingSourceUsesProviderValidation(t *testing.T) {
	physicalHashPath := `C:\workspace\project#1\session.jsonl`
	tests := []struct {
		name     string
		provider parser.Provider
		path     string
		want     bool
	}{
		{
			name: "physical hash path",
			provider: sourceMissingPolicyProvider{ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{Type: parser.AgentClaude},
			}},
			path: physicalHashPath,
			want: true,
		},
		{
			name: "validated Devin virtual path",
			provider: sourceMissingPolicyProvider{
				ProviderBase: parser.ProviderBase{Def: parser.AgentDef{Type: parser.AgentDevin}},
				validVirtual: true,
			},
			path: `C:\workspace\db#session-1`,
			want: false,
		},
		{
			name: "validated Windsurf virtual path",
			provider: sourceMissingPolicyProvider{
				ProviderBase: parser.ProviderBase{Def: parser.AgentDef{Type: parser.AgentWindsurf}},
				validVirtual: true,
			},
			path: `C:\workspace\state.vscdb#session-1`,
			want: false,
		},
		{
			name: "unvalidated Devin virtual path",
			provider: sourceMissingPolicyProvider{ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{Type: parser.AgentDevin},
			}},
			path: `C:\workspace\db#unknown`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, preserveConfiguredMissingSource(
				context.Background(), tt.provider, tt.path, "session-1",
			))
		})
	}
}

func TestConfiguredSourceMissingMembersKeepsVirtualMembersOnTombstonePolicy(t *testing.T) {
	members := []sourceMissingMember{
		{sessionID: "physical", virtual: false},
		{sessionID: "virtual", virtual: true},
	}

	got := configuredSourceMissingMembers(true, members)
	require.Len(t, got, 1)
	assert.Equal(t, "virtual", got[0].sessionID)
	assert.Equal(t, members, configuredSourceMissingMembers(false, members))
}
