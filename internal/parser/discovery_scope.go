package parser

import "path/filepath"

// DiscoveryScope identifies an unexamined part of a filesystem discovery.
// Root is the original configured root, not a replacement root for Path.
// Subtree protects descendants as well as Path when its contents are unknown.
type DiscoveryScope struct {
	Root    string
	Path    string
	Subtree bool
}

type scopedDiscoveryError struct {
	scope DiscoveryScope
	cause error
}

func (err scopedDiscoveryError) Error() string                   { return err.cause.Error() }
func (err scopedDiscoveryError) Unwrap() error                   { return err.cause }
func (err scopedDiscoveryError) incompleteScope() DiscoveryScope { return err.scope }

func incompleteDiscoveryScope(
	provider AgentType, scope DiscoveryScope, reason string, cause error,
) error {
	return scopedDiscoveryError{
		scope: scope,
		cause: incompleteDiscoveryError(provider, reason, cause),
	}
}

// IncompleteDiscoveryScopes returns bounded filesystem scopes only when every
// failure is localised. An opaque or mixed failure requires the caller to keep
// its original retry and deletion safeguards.
func IncompleteDiscoveryScopes(err error) ([]DiscoveryScope, bool) {
	const limit = 256
	pending := []error{err}
	var scopes []DiscoveryScope
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if next == nil {
			continue
		}
		if scoped, ok := next.(interface{ incompleteScope() DiscoveryScope }); ok {
			scope := scoped.incompleteScope()
			if scope.Root == "" || scope.Path == "" ||
				!reconciliationScopeWithinOrSame(
					cleanReconciliationScopeRoot(scope.Path), cleanReconciliationScopeRoot(scope.Root),
				) {
				return nil, false
			}
			scope.Path = filepath.Clean(scope.Path)
			covered := false
			for _, previous := range scopes {
				if samePath(previous.Root, scope.Root) &&
					(samePath(previous.Path, scope.Path) && (previous.Subtree || !scope.Subtree) ||
						previous.Subtree && pathUnderRoot(previous.Path, scope.Path)) {
					covered = true
					break
				}
			}
			if !covered {
				if len(scopes) == limit {
					return nil, false
				}
				scopes = append(scopes, scope)
			}
			continue
		}
		if joined, ok := next.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return nil, false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := next.(interface{ Unwrap() error }); ok {
			cause := wrapped.Unwrap()
			if cause == nil {
				return nil, false
			}
			pending = append(pending, cause)
			continue
		}
		return nil, false
	}
	return scopes, len(scopes) > 0
}
