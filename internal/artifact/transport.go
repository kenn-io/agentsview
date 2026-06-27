package artifact

import (
	"context"
	"fmt"
	"os"
)

// Transport exchanges immutable, content-addressed artifacts between the local
// store and a remote target using set-union semantics: publish local-only
// artifacts to the remote and fetch remote-only artifacts into the local store.
// Because every artifact is write-once and content-addressed, exchange is
// order-independent and idempotent, so folder, HTTP peer, and object-store
// targets are interchangeable behind this interface.
type Transport interface {
	// Prepare validates the target against the local store and creates any
	// remote-side structure required before exchange. It runs before the local
	// export so a misconfigured target fails fast.
	Prepare(localRoot string) error
	// Exchange performs the set-union publish (local-only -> remote) and fetch
	// (remote-only -> local).
	Exchange(ctx context.Context, localRoot string) error
}

// folderTransport exchanges artifacts with a local filesystem folder: a synced
// share such as Syncthing, Dropbox, NFS, or an rclone mount.
type folderTransport struct {
	target string
}

func (t *folderTransport) Prepare(localRoot string) error {
	if err := validateDisjointRoots(localRoot, t.target); err != nil {
		return err
	}
	if err := os.MkdirAll(t.target, 0o755); err != nil {
		return fmt.Errorf("creating artifact sync target: %w", err)
	}
	return nil
}

func (t *folderTransport) Exchange(_ context.Context, localRoot string) error {
	if err := CopyUnion(localRoot, t.target); err != nil {
		return fmt.Errorf("publishing artifacts: %w", err)
	}
	if err := CopyUnion(t.target, localRoot); err != nil {
		return fmt.Errorf("fetching artifacts: %w", err)
	}
	return nil
}
