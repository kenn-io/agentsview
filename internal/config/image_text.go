package config

import (
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
)

type ImageTextConfig struct {
	Enabled  bool   `json:"enabled" toml:"enabled"`
	Endpoint string `json:"endpoint" toml:"endpoint"`
}

func (c ImageTextConfig) Policy() (glmocr.Policy, error) {
	normalize, err := document.NewNormalizePolicy(1_000_000)
	if err != nil {
		return glmocr.Policy{}, err
	}
	return glmocr.NewPolicy(glmocr.PolicyConfig{Endpoint: c.Endpoint, NormalizePolicy: normalize, MaxDocumentBytes: glmocr.MaxDocumentBytes, MaxResponseBytes: glmocr.MaxResponseBytes, MaxUnits: glmocr.MaxUnits})
}

func (c ImageTextConfig) Validate() error {
	if !c.Enabled && c.Endpoint == "" {
		return nil
	}
	_, err := c.Policy()
	if err != nil {
		return fmt.Errorf("image_text: %w", err)
	}
	return nil
}
