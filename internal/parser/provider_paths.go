package parser

import (
	"go.kenn.io/agentsview/internal/pathutil"
)

// ResolveProviderRoot runs during configuration loading. Providers own the
// layout of files outside their transcript roots.
func ResolveProviderRoot(agent AgentType, path string) (string, string, error) {
	root, err := pathutil.ResolveAbsolute(path)
	if err != nil {
		return "", "", err
	}
	if def, ok := AgentByType(agent); ok {
		factory := providerFactoryForDef(def)
		if resolver, ok := factory.(interface{ ResolveMetadataDir(string) (string, error) }); ok {
			dir, err := resolver.ResolveMetadataDir(path)
			return root, dir, err
		}
	}
	return root, "", nil
}

func cloneMetadataDirs(dirs map[string][]string) map[string][]string {
	if dirs == nil {
		return nil
	}
	copy := make(map[string][]string, len(dirs))
	for root, list := range dirs {
		copy[root] = append([]string(nil), list...)
	}
	return copy
}

type configuredProviderFactory struct {
	ProviderFactory
	metadata map[string][]string
}

// ConfigureProviderFactory binds resolved metadata to every instance, including
// lightweight instances used for fingerprints and temporary watch scopes.
func ConfigureProviderFactory(factory ProviderFactory, metadata map[string][]string) ProviderFactory {
	if len(metadata) == 0 {
		return factory
	}
	return configuredProviderFactory{factory, cloneMetadataDirs(metadata)}
}

func (f configuredProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg.MetadataDirs = f.metadata
	return f.ProviderFactory.NewProvider(cfg)
}
