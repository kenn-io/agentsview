package artifact

import (
	"context"
	"errors"
	"io"
)

type testOriginIterator struct {
	next  func(context.Context, int) ([]string, error)
	close func() error
}

func (i *testOriginIterator) Next(ctx context.Context, limit int) ([]string, error) {
	return i.next(ctx, limit)
}

func (i *testOriginIterator) Close() error {
	if i.close == nil {
		return nil
	}
	return i.close()
}

type testEntryIterator struct {
	next  func(context.Context, int) ([]Entry, error)
	close func() error
}

func (i *testEntryIterator) Next(ctx context.Context, limit int) ([]Entry, error) {
	return i.next(ctx, limit)
}

func (i *testEntryIterator) Close() error {
	if i.close == nil {
		return nil
	}
	return i.close()
}

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

func firstStoreOriginPage(
	ctx context.Context, store ArtifactStore, limit int,
) (_ []string, retErr error) {
	iterator, err := store.Origins(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	items, err := iterator.Next(ctx, limit)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return items, err
}

func firstStoreQuarantinePage(
	ctx context.Context, store ArtifactQuarantineStore, limit int,
) (_ []QuarantinedEntry, retErr error) {
	iterator, err := store.Quarantined(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	items, err := iterator.Next(ctx, limit)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return items, err
}
