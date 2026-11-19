package config

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/secretref"
)

// readSettingSecretFile reads the secret a *_file setting names. The file must
// pass secretref's ownership and permission checks; trailing CR/LF is dropped.
// Errors name the setting, the path and the reason, never the contents.
func readSettingSecretFile(setting, path string) (string, error) {
	secret, err := secretref.Ref{File: strings.TrimSpace(path)}.Resolve()
	if err != nil {
		if sourceErr, ok := errors.AsType[*secretref.SourceError](err); ok {
			return "", fmt.Errorf("%s: cannot use secret file %s: %s",
				setting, strings.TrimSpace(path), sourceErr.Reason)
		}
		return "", fmt.Errorf("%s: %w", setting, err)
	}
	return secret.Value, nil
}

// oneSecretSource rejects a secret configured through both keyA and keyB
// instead of picking one silently. Any nonempty value counts as set, matching
// how resolvers choose a source. section prefixes the error; top-level keys
// pass "".
func oneSecretSource(section, keyA, valueA, keyB, valueB string) error {
	if valueA != "" && valueB != "" {
		return errors.New(strings.TrimSpace(
			fmt.Sprintf("%s set only one of %s and %s", section, keyA, keyB)))
	}
	return nil
}
