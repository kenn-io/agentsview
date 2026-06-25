package parser

import (
	"fmt"
	"maps"
)

// ProviderMigrationMode describes which runtime path owns a provider during
// the facade migration.
type ProviderMigrationMode string

const (
	ProviderMigrationLegacyOnly            ProviderMigrationMode = "legacy-only"
	ProviderMigrationShadowCompare         ProviderMigrationMode = "shadow-compare"
	ProviderMigrationProviderAuthoritative ProviderMigrationMode = "provider-authoritative"
	ProviderMigrationImportOnly            ProviderMigrationMode = "import-only"
)

var providerMigrationModes = map[AgentType]ProviderMigrationMode{
	AgentClaude:         ProviderMigrationLegacyOnly,
	AgentCowork:         ProviderMigrationLegacyOnly,
	AgentCodex:          ProviderMigrationLegacyOnly,
	AgentCopilot:        ProviderMigrationLegacyOnly,
	AgentGemini:         ProviderMigrationLegacyOnly,
	AgentMiMoCode:       ProviderMigrationLegacyOnly,
	AgentOpenCode:       ProviderMigrationLegacyOnly,
	AgentKilo:           ProviderMigrationLegacyOnly,
	AgentOpenHands:      ProviderMigrationLegacyOnly,
	AgentCursor:         ProviderMigrationLegacyOnly,
	AgentIflow:          ProviderMigrationProviderAuthoritative,
	AgentAmp:            ProviderMigrationProviderAuthoritative,
	AgentZencoder:       ProviderMigrationProviderAuthoritative,
	AgentVSCodeCopilot:  ProviderMigrationLegacyOnly,
	AgentVSCopilot:      ProviderMigrationLegacyOnly,
	AgentPi:             ProviderMigrationProviderAuthoritative,
	AgentQwen:           ProviderMigrationProviderAuthoritative,
	AgentCommandCode:    ProviderMigrationProviderAuthoritative,
	AgentDeepSeekTUI:    ProviderMigrationProviderAuthoritative,
	AgentOpenClaw:       ProviderMigrationLegacyOnly,
	AgentQClaw:          ProviderMigrationLegacyOnly,
	AgentKimi:           ProviderMigrationProviderAuthoritative,
	AgentClaudeAI:       ProviderMigrationLegacyOnly,
	AgentChatGPT:        ProviderMigrationLegacyOnly,
	AgentKiro:           ProviderMigrationLegacyOnly,
	AgentKiroIDE:        ProviderMigrationLegacyOnly,
	AgentCortex:         ProviderMigrationProviderAuthoritative,
	AgentHermes:         ProviderMigrationLegacyOnly,
	AgentWorkBuddy:      ProviderMigrationProviderAuthoritative,
	AgentForge:          ProviderMigrationLegacyOnly,
	AgentPiebald:        ProviderMigrationLegacyOnly,
	AgentWarp:           ProviderMigrationLegacyOnly,
	AgentPositron:       ProviderMigrationLegacyOnly,
	AgentAntigravity:    ProviderMigrationLegacyOnly,
	AgentAntigravityCLI: ProviderMigrationLegacyOnly,
	AgentVibe:           ProviderMigrationLegacyOnly,
	AgentZed:            ProviderMigrationLegacyOnly,
	AgentQwenPaw:        ProviderMigrationLegacyOnly,
	AgentGptme:          ProviderMigrationProviderAuthoritative,
	AgentShelley:        ProviderMigrationLegacyOnly,
	AgentAider:          ProviderMigrationLegacyOnly,
	AgentOMP:            ProviderMigrationProviderAuthoritative,
	AgentReasonix:       ProviderMigrationLegacyOnly,
}

// ProviderMigrationModes returns the current provider migration manifest.
func ProviderMigrationModes() map[AgentType]ProviderMigrationMode {
	modes := make(map[AgentType]ProviderMigrationMode, len(providerMigrationModes))
	maps.Copy(modes, providerMigrationModes)
	return modes
}

// ValidateProviderMigrationModes checks that provider factories and the
// migration manifest move in lockstep during the staged facade migration.
func ValidateProviderMigrationModes(
	factories []ProviderFactory,
	modes map[AgentType]ProviderMigrationMode,
) error {
	seen := make(map[AgentType]bool, len(factories))
	for _, factory := range factories {
		def := factory.Definition()
		seen[def.Type] = true

		mode, ok := modes[def.Type]
		if !ok {
			return fmt.Errorf("%s: missing provider migration mode", def.Type)
		}
		if err := validateProviderMigrationMode(factory, mode); err != nil {
			return err
		}
	}

	for agent := range modes {
		if !seen[agent] {
			return fmt.Errorf("%s: provider migration mode has no factory", agent)
		}
	}
	return nil
}

func validateProviderMigrationMode(
	factory ProviderFactory,
	mode ProviderMigrationMode,
) error {
	def := factory.Definition()
	legacy := isLegacyProviderFactory(factory)
	switch mode {
	case ProviderMigrationLegacyOnly:
		if !legacy {
			return fmt.Errorf(
				"%s: concrete provider must opt into %s before leaving %s",
				def.Type,
				ProviderMigrationShadowCompare,
				ProviderMigrationLegacyOnly,
			)
		}
	case ProviderMigrationShadowCompare, ProviderMigrationProviderAuthoritative:
		if legacy {
			return fmt.Errorf(
				"%s: %s requires a concrete provider; keep %s while using the legacy adapter",
				def.Type,
				mode,
				ProviderMigrationLegacyOnly,
			)
		}
	case ProviderMigrationImportOnly:
		if !isImportOnlyAgentType(def.Type) {
			return fmt.Errorf(
				"%s: %s is only valid for import-only providers",
				def.Type,
				ProviderMigrationImportOnly,
			)
		}
		if legacy {
			return fmt.Errorf(
				"%s: %s requires a concrete import-only provider; keep %s while using the legacy adapter",
				def.Type,
				ProviderMigrationImportOnly,
				ProviderMigrationLegacyOnly,
			)
		}
	default:
		return fmt.Errorf("%s: invalid provider migration mode %q", def.Type, mode)
	}
	return nil
}

func isLegacyProviderFactory(factory ProviderFactory) bool {
	_, ok := factory.(legacyProviderFactory)
	return ok
}

func isImportOnlyAgentType(agent AgentType) bool {
	switch agent {
	case AgentClaudeAI, AgentChatGPT:
		return true
	default:
		return false
	}
}
