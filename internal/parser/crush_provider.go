package parser

import (
	"context"
	"path/filepath"
)

type crushProviderFactory struct {
	def AgentDef
}

func newCrushProviderFactory(def AgentDef) ProviderFactory {
	return &crushProviderFactory{def: cloneAgentDef(def)}
}

func (f *crushProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f *crushProviderFactory) Capabilities() Capabilities {
	return withDBBackedRawCapture(crushProviderCapabilities())
}

func (f *crushProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	cfg.Roots = normalizeCrushRoots(cfg.Roots)
	spec := crushProviderSpec()
	base := &dbBackedProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    withDBBackedRawCapture(spec.caps),
		Config:  cfg,
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
	return base
}

func crushProviderCapabilities() Capabilities {
	source := dbBackedSourceCapabilities(CapabilityNotApplicable)
	// Crush does not consume stored source hints; scheduling them would
	// enumerate every session for each WAL event.
	source.StoredSourceHints = CapabilityUnsupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}

func crushProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentCrush,
		dbName: CrushDBName,
		findDB: crushDBPath,
		streamMeta: func(
			ctx context.Context, dbPath string, yield func(dbBackedSessionMeta) error,
		) error {
			return forEachCrushSessionMeta(ctx, dbPath, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return crushSessionMeta(ctx, dbPath, sessionID)
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			sess, msgs, err := parseCrushSession(dbPath, sessionID, machine)
			if err != nil || sess == nil {
				return nil, err
			}
			return []ParseResult{{Session: *sess, Messages: msgs}}, nil
		},
		caps: crushProviderCapabilities(),
	}
}

// normalizeCrushRoots expands configured roots into per-project data
// directories. A root is one of:
//   - a directory directly holding crush.db (a <project>/.crush data dir)
//   - the path to a crush.db file itself
//   - a Crush data directory holding projects.json, whose listed data
//     dirs are each expanded (deduplicated); an unreadable or empty
//     registry leaves the root in place rather than failing discovery
func normalizeCrushRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	add := func(root string) {
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	for _, root := range cleaned {
		root = filepath.Clean(root)
		if root == "" || root == "." {
			continue
		}
		if filepath.Base(root) == CrushDBName {
			add(filepath.Dir(root))
			continue
		}
		if IsRegularFile(filepath.Join(root, CrushDBName)) {
			add(root)
			continue
		}
		expanded := CrushProjectsDataDirs(filepath.Join(root, CrushProjectsFileName))
		if len(expanded) == 0 {
			add(root)
			continue
		}
		for _, dir := range expanded {
			add(dir)
		}
	}
	return out
}

func crushDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, CrushDBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}
