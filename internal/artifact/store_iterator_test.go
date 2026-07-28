package artifact

import (
	"context"
	"errors"
	"io"
)

func firstStoreEntryPage(
	ctx context.Context, store ArtifactStore, origin string, kind Kind, limit int,
) (_ Page, retErr error) {
	iterator, err := store.Entries(ctx, origin, kind)
	if err != nil {
		return Page{}, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	items, err := iterator.Next(ctx, limit)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return Page{Items: items}, err
}
