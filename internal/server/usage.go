package server

// Comparison holds the prior-period cost comparison returned by
// GET /api/v1/usage/comparison.
type Comparison struct {
	PriorFrom      string  `json:"priorFrom"`
	PriorTo        string  `json:"priorTo"`
	PriorTotalCost float64 `json:"priorTotalCost"`
	DeltaPct       float64 `json:"deltaPct"`
}
