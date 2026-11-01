package gittrailer

import "context"

// StampHook processes one installed-hook invocation.
func StampHook(ctx context.Context, messageFile, source string, session Session, parentPID string) error {
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	invocation, err := invocationForPID(parentPID)
	if err != nil {
		return err
	}
	return stamp(ctx, messageFile, &source, session, invocation)
}
