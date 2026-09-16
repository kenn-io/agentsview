package main

import (
	"context"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/service"
)

// machineLabelCatalog owns optional machine-label enrichment for CLI documents
// that print machine keys.
func machineLabelCatalog(
	ctx context.Context,
	stderr io.Writer,
	read func(context.Context) (service.MachineLabelCatalog, error),
) service.MachineLabelCatalog {
	labels, err := read(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "warning: machine labels unavailable: %v\n", err)
		return service.MachineLabelCatalog{}
	}
	if labels == nil {
		return service.MachineLabelCatalog{}
	}
	return labels
}

func machineLabelsForKeys(
	labels service.MachineLabelCatalog, keys map[string]struct{},
) service.MachineLabelCatalog {
	filtered := make(service.MachineLabelCatalog, len(keys))
	for key := range keys {
		if label, ok := labels[key]; ok {
			filtered[key] = label
		}
	}
	return filtered
}
