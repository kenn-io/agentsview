package config

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

func normalizeTargetName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}

func isReservedMirrorTargetName(name string) bool {
	switch normalizeTargetName(name) {
	case "all", "local":
		return true
	default:
		return false
	}
}

func decodeNamedTargetMap[T any](raw map[string]any, noun string) (T, error) {
	var zero T
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return zero, fmt.Errorf("encoding %s config: %w", noun, err)
	}
	var cfg T
	if _, err := toml.Decode(buf.String(), &cfg); err != nil {
		return zero, fmt.Errorf("decoding %s config: %w", noun, err)
	}
	return cfg, nil
}

func parseNamedTargetSection[T any](
	section, noun string,
	value any,
	fieldKeys map[string]struct{},
	reserved func(string) bool,
) (T, map[string]T, error) {
	var zero T
	if value == nil {
		return zero, nil, nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return zero, nil, fmt.Errorf("expected [%s] to be a table", section)
	}
	hasLegacyFields := false
	hasNamedTargets := false
	legacyRaw := make(map[string]any)
	namedTargets := make(map[string]T)
	seenNames := make(map[string]string)
	for rawName, rawValue := range table {
		name := normalizeTargetName(rawName)
		if _, ok := fieldKeys[name]; ok {
			if _, nested := rawValue.(map[string]any); nested {
				return zero, nil, fmt.Errorf(
					"[%s].%s must be a scalar or array field, not a nested table",
					section, rawName,
				)
			}
			hasLegacyFields = true
			legacyRaw[rawName] = rawValue
			continue
		}
		targetRaw, ok := rawValue.(map[string]any)
		if !ok {
			return zero, nil, fmt.Errorf(
				"[%s].%s must be a named target table",
				section, rawName,
			)
		}
		hasNamedTargets = true
		if name == "" {
			return zero, nil, fmt.Errorf(
				"named %s targets must not be blank",
				noun,
			)
		}
		if reserved(name) {
			return zero, nil, fmt.Errorf(
				"named %s target %q is reserved",
				noun, name,
			)
		}
		if prev, exists := seenNames[name]; exists {
			return zero, nil, fmt.Errorf(
				"named %s targets %q and %q normalize to the same name %q",
				noun, prev, rawName, name,
			)
		}
		seenNames[name] = rawName
		targetCfg, err := decodeNamedTargetMap[T](targetRaw, noun)
		if err != nil {
			return zero, nil, fmt.Errorf(
				"[%s].%s: %w", section, rawName, err,
			)
		}
		namedTargets[name] = targetCfg
	}
	if hasLegacyFields && hasNamedTargets {
		return zero, nil, fmt.Errorf(
			"cannot mix legacy [%s] fields with named [%s.NAME] targets",
			section, section,
		)
	}
	if hasLegacyFields {
		legacyCfg, err := decodeNamedTargetMap[T](legacyRaw, noun)
		if err != nil {
			return zero, nil, err
		}
		return legacyCfg, nil, nil
	}
	if hasNamedTargets {
		return zero, namedTargets, nil
	}
	return zero, nil, nil
}

func defaultNamedTargetName[T any](
	section, defaultKey, defaultName string, targets map[string]T,
) (string, error) {
	if len(targets) == 0 {
		if defaultName != "" {
			return "", fmt.Errorf(
				"%s requires named [%s.NAME] targets",
				defaultKey, section,
			)
		}
		return "", nil
	}
	if defaultName != "" {
		if _, ok := targets[defaultName]; !ok {
			return "", fmt.Errorf(
				"%s %q does not match any named [%s.NAME] target",
				defaultKey, defaultName, section,
			)
		}
		return defaultName, nil
	}
	if len(targets) == 1 {
		for name := range targets {
			return name, nil
		}
	}
	return "", fmt.Errorf(
		"%s is required when more than one [%s.NAME] target is defined",
		defaultKey, section,
	)
}

func namedTargetNames[T any](
	section, defaultKey, defaultName string, targets map[string]T,
) ([]string, string, error) {
	name, err := defaultNamedTargetName(section, defaultKey, defaultName, targets)
	if err != nil {
		return nil, "", err
	}
	if len(targets) == 0 {
		return nil, "", nil
	}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == name {
			return true
		}
		if names[j] == name {
			return false
		}
		return names[i] < names[j]
	})
	return names, name, nil
}
