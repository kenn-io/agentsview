package main

import (
	"context"
	"fmt"
	"io"
)

// machineLabelCatalog owns optional machine-label enrichment for CLI documents
// that print machine keys.
func machineLabelCatalog(
	ctx context.Context,
	stderr io.Writer,
	read func(context.Context) (map[string]string, error),
) map[string]string {
	labels, err := read(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "warning: machine labels unavailable: %v\n", err)
		return map[string]string{}
	}
	if labels == nil {
		return map[string]string{}
	}
	return labels
}
