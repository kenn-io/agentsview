package parser

import (
	"fmt"
	"maps"
)

// ProviderMigrationMode describes which runtime path owns a provider during
// the facade migration.
type ProviderMigrationMode string

const (
	ProviderMigrationProviderAuthoritative ProviderMigrationMode = "provider-authoritative"
	ProviderMigrationImportOnly            ProviderMigrationMode = "import-only"
)

var providerMigrationModes = map[AgentType]ProviderMigrationMode{
	AgentClaude:         ProviderMigrationProviderAuthoritative,
	AgentOpenClaude:     ProviderMigrationProviderAuthoritative,
	AgentCowork:         ProviderMigrationProviderAuthoritative,
	AgentCodex:          ProviderMigrationProviderAuthoritative,
	AgentCopilot:        ProviderMigrationProviderAuthoritative,
	AgentGemini:         ProviderMigrationProviderAuthoritative,
	AgentOpenHands:      ProviderMigrationProviderAuthoritative,
	AgentCursor:         ProviderMigrationProviderAuthoritative,
	AgentMiMoCode:       ProviderMigrationProviderAuthoritative,
	AgentOpenCode:       ProviderMigrationProviderAuthoritative,
	AgentKilo:           ProviderMigrationProviderAuthoritative,
	AgentKiloLegacy:     ProviderMigrationProviderAuthoritative,
	AgentIcodemate:      ProviderMigrationProviderAuthoritative,
	AgentIflow:          ProviderMigrationProviderAuthoritative,
	AgentAmp:            ProviderMigrationProviderAuthoritative,
	AgentZencoder:       ProviderMigrationProviderAuthoritative,
	AgentVSCodeCopilot:  ProviderMigrationProviderAuthoritative,
	AgentWindsurf:       ProviderMigrationProviderAuthoritative,
	AgentTrae:           ProviderMigrationProviderAuthoritative,
	AgentVSCopilot:      ProviderMigrationProviderAuthoritative,
	AgentPi:             ProviderMigrationProviderAuthoritative,
	AgentQwen:           ProviderMigrationProviderAuthoritative,
	AgentCommandCode:    ProviderMigrationProviderAuthoritative,
	AgentDeepSeekTUI:    ProviderMigrationProviderAuthoritative,
	AgentOpenClaw:       ProviderMigrationProviderAuthoritative,
	AgentQClaw:          ProviderMigrationProviderAuthoritative,
	AgentKimi:           ProviderMigrationProviderAuthoritative,
	AgentClaudeAI:       ProviderMigrationImportOnly,
	AgentChatGPT:        ProviderMigrationImportOnly,
	AgentKiro:           ProviderMigrationProviderAuthoritative,
	AgentKiroIDE:        ProviderMigrationProviderAuthoritative,
	AgentCortex:         ProviderMigrationProviderAuthoritative,
	AgentHermes:         ProviderMigrationProviderAuthoritative,
	AgentGrok:           ProviderMigrationProviderAuthoritative,
	AgentWorkBuddy:      ProviderMigrationProviderAuthoritative,
	AgentForge:          ProviderMigrationProviderAuthoritative,
	AgentDevin:          ProviderMigrationProviderAuthoritative,
	AgentPiebald:        ProviderMigrationProviderAuthoritative,
	AgentWarp:           ProviderMigrationProviderAuthoritative,
	AgentPositron:       ProviderMigrationProviderAuthoritative,
	AgentPositAssistant: ProviderMigrationProviderAuthoritative,
	AgentZCode:          ProviderMigrationProviderAuthoritative,
	AgentAntigravity:    ProviderMigrationProviderAuthoritative,
	AgentAntigravityCLI: ProviderMigrationProviderAuthoritative,
	AgentVibe:           ProviderMigrationProviderAuthoritative,
	AgentZed:            ProviderMigrationProviderAuthoritative,
	AgentQwenPaw:        ProviderMigrationProviderAuthoritative,
	AgentGptme:          ProviderMigrationProviderAuthoritative,
	AgentQoder:          ProviderMigrationProviderAuthoritative,
	AgentShelley:        ProviderMigrationProviderAuthoritative,
	AgentAider:          ProviderMigrationProviderAuthoritative,
	AgentOMP:            ProviderMigrationProviderAuthoritative,
	AgentReasonix:       ProviderMigrationProviderAuthoritative,
	AgentRooCode:        ProviderMigrationProviderAuthoritative,
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
	switch mode {
	case ProviderMigrationProviderAuthoritative:
		caps := factory.Capabilities().Source
		if caps.DiscoverSources != CapabilitySupported {
			return fmt.Errorf(
				"%s: %s requires provider source discovery",
				def.Type, mode,
			)
		}
		if caps.FindSource != CapabilitySupported {
			return fmt.Errorf(
				"%s: %s requires provider source lookup",
				def.Type, mode,
			)
		}
		if caps.StreamingDiscovery == CapabilitySupported {
			provider := factory.NewProvider(ProviderConfig{})
			if sourceSetProvider, ok := provider.(*SourceSetProvider); ok {
				if _, ok := sourceSetProvider.sources.(StreamingDiscoverer); !ok {
					return fmt.Errorf(
						"%s: streaming discovery capability requires underlying source set StreamingDiscoverer",
						def.Type,
					)
				}
			}
			if _, ok := provider.(StreamingDiscoverer); !ok {
				return fmt.Errorf(
					"%s: streaming discovery capability requires StreamingDiscoverer",
					def.Type,
				)
			}
			if caps.SharedContainerSource == CapabilitySupported {
				if sourceSetProvider, ok := provider.(*SourceSetProvider); ok {
					if _, ok := sourceSetProvider.sources.(ReconciliationSourceResolver); !ok {
						return fmt.Errorf(
							"%s: shared-container streaming requires underlying source set exact reconciliation rehydration",
							def.Type,
						)
					}
				}
				if _, ok := provider.(ReconciliationSourceResolver); !ok {
					return fmt.Errorf(
						"%s: shared-container streaming requires exact reconciliation rehydration",
						def.Type,
					)
				}
			}
		}
	case ProviderMigrationImportOnly:
		if !isImportOnlyAgentType(def.Type) {
			return fmt.Errorf(
				"%s: %s is only valid for import-only providers",
				def.Type,
				ProviderMigrationImportOnly,
			)
		}
	default:
		return fmt.Errorf("%s: invalid provider migration mode %q", def.Type, mode)
	}
	return nil
}

func isImportOnlyAgentType(agent AgentType) bool {
	switch agent {
	case AgentClaudeAI, AgentChatGPT:
		return true
	default:
		return false
	}
}
