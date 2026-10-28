package parser

import "context"

type importOnlyProviderFactory struct {
	def AgentDef
}

func newImportOnlyProviderFactory(def AgentDef) ProviderFactory {
	return importOnlyProviderFactory{def: cloneAgentDef(def)}
}

func (f importOnlyProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f importOnlyProviderFactory) Capabilities() Capabilities {
	return Capabilities{Content: importOnlyContentCapabilities(f.def.Type)}
}

// importOnlyContentCapabilities declares what each export parser extracts.
// Chat app exports have no working directory or git branch.
func importOnlyContentCapabilities(agent AgentType) ContentCapabilities {
	caps := ContentCapabilities{
		FirstMessage: CapabilitySupported,
		SessionName:  CapabilitySupported,
		Cwd:          CapabilityNotApplicable,
		GitBranch:    CapabilityNotApplicable,
	}
	switch agent {
	case AgentChatGPT:
		caps.Thinking = CapabilitySupported
		caps.ToolCalls = CapabilitySupported
		caps.ToolResultEvents = CapabilitySupported
		caps.Model = CapabilitySupported
	case AgentClaudeAI:
		caps.Thinking = CapabilitySupported
	default:
		// Gemini Apps activity records hold only the prompt.
	}
	return caps
}

func (f importOnlyProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	base := importOnlyProvider{
		Def:    cloneAgentDef(f.def),
		Caps:   f.Capabilities(),
		Config: cfg,
	}

	switch f.def.Type {
	case AgentChatGPT:
		return &chatGPTImportOnlyProvider{importOnlyProvider: base}
	case AgentClaudeAI:
		return &claudeAIImportOnlyProvider{importOnlyProvider: base}
	case AgentGeminiApps:
		return &geminiAppsImportOnlyProvider{importOnlyProvider: base}
	default:
		return &base
	}
}

type importOnlyProvider struct {
	ProviderBase
}

func (p *importOnlyProvider) Parse(
	context.Context,
	ParseRequest,
) (ParseOutcome, error) {
	return ParseOutcome{}, p.unsupported(ProviderFeatureParse)
}

type chatGPTImportOnlyProvider struct {
	importOnlyProvider
}

type claudeAIImportOnlyProvider struct {
	importOnlyProvider
}

type geminiAppsImportOnlyProvider struct {
	importOnlyProvider
}
