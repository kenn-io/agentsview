package pricing

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/pricing/catalog"
)

type GenAIPrices = catalog.GenAIPrices

// GenAIDocument carries the upstream JSON and its provenance through the
// existing pricing catalog refresh and storage lifecycle.
type GenAIDocument struct {
	Version   string
	SourceRef string
	Prices    *GenAIPrices
}

func NewGenAIDocument(
	prices *GenAIPrices, sourceRef string,
) (GenAIDocument, error) {
	if prices == nil {
		return GenAIDocument{}, fmt.Errorf("missing GenAI Prices document")
	}
	return GenAIDocument{
		Version: prices.Version(), SourceRef: sourceRef, Prices: prices,
	}, nil
}

func ParseGenAIDocument(
	data []byte, version, sourceRef string,
) (GenAIDocument, error) {
	prices, err := ParseGenAIPrices(data)
	if err != nil {
		return GenAIDocument{}, err
	}
	if version != "" && version != prices.Version() {
		return GenAIDocument{}, fmt.Errorf(
			"GenAI Prices version %q does not match content version %q",
			version, prices.Version(),
		)
	}
	return NewGenAIDocument(prices, sourceRef)
}

func (d GenAIDocument) RawJSON() []byte {
	if d.Prices == nil {
		return nil
	}
	return d.Prices.RawJSON()
}

func FetchGenAIPricesContext(ctx context.Context) (*GenAIPrices, error) {
	return catalog.FetchGenAIPricesContext(ctx)
}

func FetchGenAIPricesAtRef(
	ctx context.Context, ref string,
) (*GenAIPrices, error) {
	return catalog.FetchGenAIPricesAtRef(ctx, ref)
}

func ParseGenAIPrices(data []byte) (*GenAIPrices, error) {
	return catalog.ParseGenAIPrices(data)
}
