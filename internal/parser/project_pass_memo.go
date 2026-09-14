package parser

import (
	"context"
	"sync"
)

type projectRootMemo struct {
	roots sync.Map
	scans sync.Map
}

type gitRootResult struct {
	root   string
	linked bool
}

type siblingScanResult struct {
	selfIsRepo    bool
	agreedRoot    string
	singleDirRoot string
	worktreeCount int
	dirCount      int
}

type projectRootMemoContextKey struct{}

func WithProjectRootMemo(ctx context.Context) context.Context {
	if projectRootMemoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, projectRootMemoContextKey{}, &projectRootMemo{})
}

func projectRootMemoFrom(ctx context.Context) *projectRootMemo {
	memo, _ := ctx.Value(projectRootMemoContextKey{}).(*projectRootMemo)
	return memo
}

func (memo *projectRootMemo) root(
	key string, fn func() gitRootResult,
) gitRootResult {
	if memo == nil {
		return fn()
	}
	return memoizedProjectResult(&memo.roots, key, fn)
}

func (memo *projectRootMemo) scan(
	key string, fn func() siblingScanResult,
) siblingScanResult {
	if memo == nil {
		return fn()
	}
	return memoizedProjectResult(&memo.scans, key, fn)
}

func memoizedProjectResult[T any](cache *sync.Map, key string, fn func() T) T {
	value, ok := cache.Load(key)
	if !ok {
		value, _ = cache.LoadOrStore(key, sync.OnceValue(fn))
	}
	return value.(func() T)()
}
