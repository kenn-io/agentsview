package friction

import (
	"fmt"

	"go.kenn.io/agentsview/internal/serdejson"
)

// CanonicalSummaryJSON re-renders a summary JSON document in the byte form
// RenderSummaryJSON produces (serde pretty, sorted keys, trailing newline),
// so a summary that crossed the HTTP API prints the same bytes as the store.
func CanonicalSummaryJSON(raw []byte) ([]byte, error) {
	v, err := serdejson.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding friction summary: %w", err)
	}
	out, err := serdejson.Pretty(v)
	if err != nil {
		return nil, fmt.Errorf("encoding friction summary: %w", err)
	}
	return append(out, '\n'), nil
}
