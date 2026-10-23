package config

import (
	"errors"
	"strings"
)

// ValidateRawDerivation validates opt-in and bounded controls. Tenant binding
// remains enabled with the worker off so rollback preserves public identities.
func (p PGConfig) ValidateRawDerivation(requireAuth bool) error {
	if p.RawEmbeddings && !p.RawDerivation {
		return errors.New("raw_embeddings requires raw_derivation = true")
	}
	if !p.RawDerivation && p.RawTenant == "" {
		return nil
	}
	if strings.TrimSpace(p.RawTenant) == "" || p.RawTenant != strings.TrimSpace(p.RawTenant) || len(p.RawTenant) > 128 {
		return errors.New("hosted raw projection requires raw_tenant")
	}
	if !requireAuth {
		return errors.New("hosted raw projection requires authentication")
	}
	if p.Schema == "" {
		return errors.New("hosted raw projection requires a configured schema")
	}
	for _, r := range p.Schema {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return errors.New("hosted raw projection schema is invalid")
		}
	}
	if p.RawPollSeconds < 0 || p.RawPollSeconds > 60 || p.RawAttemptSeconds < 0 || p.RawAttemptSeconds > 300 || p.RawMaxAttempts < 0 || p.RawMaxAttempts > 10 {
		return errors.New("hosted raw worker bounds: poll 1-60 seconds, attempt 1-300 seconds, attempts 1-10; zero selects defaults")
	}
	return nil
}

// ValidatePGRawEmbeddings separates owner recipe validation from runtime
// transport validation. Runtime recipes come from provisioned generations.
func (c Config) ValidatePGRawEmbeddings(p PGConfig, provisioning bool) error {
	if !p.RawEmbeddings {
		return nil
	}
	e := c.Vector.Embeddings
	if provisioning {
		if e.Model == "" || e.Dimension <= 0 || e.MaxInputChars <= 0 {
			return errors.New("raw_embeddings provisioning requires a full [vector.embeddings] recipe: model, dimension and max_input_chars")
		}
		// https://github.com/pgvector/pgvector#hnsw
		if e.Dimension > 4000 {
			return errors.New("raw_embeddings dimension exceeds pgvector's halfvec HNSW limit of 4000")
		}
		if e.ModelContextTokens < 0 {
			return errors.New("raw_embeddings model_context_tokens must not be negative")
		}
		return nil
	}
	if c.DeploymentEmbeddings != nil {
		if len(e.Servers) != 0 {
			return errors.New("hosted embeddings transport must be configured in one place")
		}
		return c.DeploymentEmbeddings.validateTransport("deployment")
	}
	return e.validateServersWithRecipe(false)
}

// ValidatePGRawDerivation reads only the deployment authentication policy used
// by hosted raw readers and workers. Direct PG connections do not use the
// server's token file or model settings.
func (c Config) ValidatePGRawDerivation(p PGConfig) error {
	if p.RawDerivation || p.RawTenant != "" {
		if active, err := deploymentVarsActive("AGENTSVIEW_REQUIRE_AUTH"); err != nil {
			return err
		} else if active {
			if v, set, err := lookupEnvBool("AGENTSVIEW_REQUIRE_AUTH"); err != nil {
				return err
			} else if set {
				c.RequireAuth = v
			}
		}
	}
	return p.ValidateRawDerivation(c.RequireAuth)
}

func (p PGConfig) RawWorkerBounds() (poll, attempt, attempts int) {
	poll, attempt, attempts = p.RawPollSeconds, p.RawAttemptSeconds, p.RawMaxAttempts
	if poll == 0 {
		poll = 5
	}
	if attempt == 0 {
		attempt = 60
	}
	if attempts == 0 {
		attempts = 5
	}
	return
}
