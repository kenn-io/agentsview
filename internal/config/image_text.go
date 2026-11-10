package config

import (
	"fmt"
	"net/url"
	"strconv"

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
	policy, err := glmocr.NewPolicy(glmocr.PolicyConfig{Endpoint: c.Endpoint, NormalizePolicy: normalize, MaxDocumentBytes: glmocr.MaxDocumentBytes, MaxResponseBytes: glmocr.MaxResponseBytes, MaxUnits: glmocr.MaxUnits})
	if err != nil {
		return glmocr.Policy{}, err
	}
	endpoint, err := url.Parse(policy.Values().Endpoint)
	if err != nil {
		return glmocr.Policy{}, err
	}
	if port := endpoint.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return glmocr.Policy{}, fmt.Errorf("invalid endpoint port %q: %w", port, err)
		}
	}
	return policy, nil
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
