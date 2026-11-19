package postgres

import (
	"context"
	"database/sql"

	"go.kenn.io/kit/vector/pgvector"
)

func openVectorStore(ctx context.Context, pg *sql.DB) (*pgvector.Store[string, string], error) {
	return pgvector.New[string, string](ctx, pg, pgvector.Schema{
		DocsTable: "vector_documents", IDColumn: "doc_key", ContentColumn: "content",
		RevisionColumn: "content_hash", Prefix: "vector",
	})
}
