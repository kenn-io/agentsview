package config

import (
	"encoding/json/v2"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestDocbankIntegrationValidation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, key string
		wantErr             string
	}{
		{"disabled", "", "", ""},
		{"missing key", "https://example.com", "", "[integrations.docbank] url and api_key_env are required together"},
		{"remote plaintext", "http://example.com", "DOCBANK_TEST_KEY", "[integrations.docbank] url must be https, or http to a loopback host; allow_http = true also permits trusted private addresses and DNS names"},
		{"loopback", "http://127.0.0.1", "DOCBANK_TEST_KEY", ""},
		{"https", "https://example.com", "DOCBANK_TEST_KEY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (DocbankIntegration{URL: tc.endpoint, APIKeyEnv: tc.key}).Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tc.wantErr)
			}
		})
	}
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML("[integrations.docbank]\nurl = 'http://127.0.0.1:9191'\napi_key_env = 'DOCBANK_TEST_KEY'"))
	assert.Equal(t, "http://127.0.0.1:9191", cfg.Integrations.Docbank.URL)
	assert.True(t, cfg.Integrations.Docbank.Configured())
}

// A trusted DNS name needs an explicit opt-in through the real TOML decoder.
func TestDocbankDNSHTTPRequiresOptIn(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		port := hegel.Draw(ht, hegel.Integers(1, 65535))
		allowHTTP := hegel.Draw(ht, hegel.Booleans())
		endpoint := fmt.Sprintf("http://node-%x.example.test:%d/base/", id, port)
		field := hegel.Draw(ht, hegel.SampledFrom([]string{"url", "web_url"}))
		apiURL, webURL := endpoint, ""
		if field == "web_url" {
			apiURL, webURL = "https://api.example.test", endpoint
		}
		var cfg Config
		require.NoError(ht, cfg.applyConfigTOML(fmt.Sprintf("[integrations.docbank]\nurl = %q\nweb_url = %q\napi_key_env = 'DOCBANK_TEST_KEY'\nallow_http = %t\n", apiURL, webURL, allowHTTP)))
		assert.Equal(ht, allowHTTP, cfg.Integrations.Docbank.AllowHTTP)
		assert.True(ht, cfg.Integrations.Docbank.Configured())
		assert.Equal(ht, allowHTTP, cfg.Integrations.Docbank.Enabled())
		if allowHTTP {
			assert.NoError(ht, cfg.Integrations.Docbank.Validate())
		} else {
			assert.Error(ht, cfg.Integrations.Docbank.Validate())
		}
	}, hegel.WithTestCases(1000))
}

func docbankEndpointConfig(endpoint string, webURL, allowHTTP bool) DocbankIntegration {
	cfg := DocbankIntegration{URL: endpoint, APIKeyEnv: "DOCBANK_TEST_KEY", AllowHTTP: allowHTTP}
	if webURL {
		cfg.URL, cfg.WebURL = "https://api.example.test", endpoint
	}
	return cfg
}

func TestDocbankHTTPHostPolicy(t *testing.T) {
	// This checks Docbank's use of the kit policy, not the kit's address classification.
	for _, tc := range []struct {
		host              string
		loopback, trusted bool
	}{
		{"localhost", true, true},
		{"node.example.test", false, true},
		{"[fd00::1]", false, true},
		{"8.8.8.8", false, false},
	} {
		for _, webURL := range []bool{false, true} {
			for _, allowHTTP := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/web=%t/allow=%t", tc.host, webURL, allowHTTP), func(t *testing.T) {
					cfg := docbankEndpointConfig("http://"+tc.host+"/base/", webURL, allowHTTP)
					before := cfg
					err := cfg.Validate()
					if tc.loopback || (tc.trusted && allowHTTP) {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, "allow_http")
					}
					assert.Equal(t, tc.loopback || (tc.trusted && allowHTTP), cfg.Enabled())
					assert.Equal(t, before, cfg)
				})
			}
		}
	}
}

func TestDocbankHTTPSAndLoopbackPreserveURLs(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		scheme, host := "https", "Public.Example.TEST"
		if hegel.Draw(ht, hegel.Booleans()) {
			scheme = "http"
			host = hegel.Draw(ht, hegel.SampledFrom([]string{"localhost", "LOCALHOST", "127.0.0.1", "::1"}))
		}
		port := hegel.Draw(ht, hegel.Integers(1, 65535))
		path := "/" + hegel.Draw(ht, hegel.Text()) + "/"
		endpoint := (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: path}).String()
		cfg := docbankEndpointConfig(endpoint, hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans()))
		before := cfg
		require.NoError(ht, cfg.Validate())
		assert.Equal(ht, before, cfg)
	}, hegel.WithTestCases(1000))
}

func TestDocbankURLSecretsStayRejected(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		secret := "synthetic-secret-" + hegel.Draw(ht, hegel.Text())
		u := &url.URL{Scheme: "http", Host: "docbank.test"}
		switch hegel.Draw(ht, hegel.Integers(0, 2)) {
		case 0:
			u.User = url.UserPassword("test", secret)
		case 1:
			u.RawQuery = url.Values{"token": {secret}}.Encode()
		case 2:
			u.Fragment = secret
		}
		cfg := docbankEndpointConfig(u.String(), hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans()))
		err := cfg.Validate()
		require.Error(ht, err)
		assert.NotContains(ht, err.Error(), secret)
	}, hegel.WithTestCases(1000))
}

func TestDocbankBasePathsStayUnchanged(t *testing.T) {
	for _, endpoint := range []string{
		"https://EXAMPLE.test:443/a/../b/", " \thttps://example.test//a/./b/ \n", "https://example.test/a/%2e%2e/b/",
		"https://example.test/a%2Fb/%20/", "http://localhost:80/a/../b/",
		"http://docbank.test:8485/a/%2e%2e/b/",
	} {
		for _, webURL := range []bool{false, true} {
			cfg := docbankEndpointConfig(endpoint, webURL, true)
			before := cfg
			require.NoError(t, cfg.Validate(), endpoint)
			assert.Equal(t, before, cfg)
		}
	}
}

func TestDocbankMalformedURLsStayRejected(t *testing.T) {
	for _, endpoint := range []string{"https://:443", "http://", "https://example.test/%zz", "ftp://example.test", "/relative", (&url.URL{Scheme: "http", Host: "docbank.test", User: url.User("test")}).String(), "http://docbank.test?key=synthetic", "http://docbank.test#synthetic"} {
		for _, webURL := range []bool{false, true} {
			for _, allowHTTP := range []bool{false, true} {
				assert.Error(t, docbankEndpointConfig(endpoint, webURL, allowHTTP).Validate(), endpoint)
			}
		}
	}
}

func TestDocbankAllowHTTPConfigLoaders(t *testing.T) {
	for _, setting := range []string{"", "allow_http = false", "allow_http = true"} {
		t.Run(setting, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			data := map[string]any{"url": "http://docbank:8485", "api_key_env": "DOCBANK_TEST_KEY"}
			if setting != "" {
				data["allow_http"] = setting == "allow_http = true"
			}
			writeConfig(t, dir, map[string]any{"integrations": map[string]any{"docbank": data}})
			for _, load := range []func() (Config, error){
				func() (Config, error) { return LoadMinimalFor(ScopeDocbank) },
				func() (Config, error) { return LoadReadOnlyFor(ScopeDocbank) },
				func() (Config, error) {
					return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeDocbank)
				},
			} {
				cfg, err := load()
				if setting == "allow_http = true" {
					require.NoError(t, err)
					assert.True(t, cfg.Integrations.Docbank.AllowHTTP)
					assert.True(t, cfg.Integrations.Docbank.Enabled())
					assert.Equal(t, "http://docbank:8485", cfg.Integrations.Docbank.URL)
				} else {
					require.ErrorContains(t, err, "allow_http")
				}
			}
		})
	}
	cfg := DocbankIntegration{AllowHTTP: true}
	require.NoError(t, cfg.Validate())
	assert.False(t, cfg.Configured())
	assert.False(t, cfg.Enabled())
	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.JSONEq(t, `{"allow_http":true}`, string(encoded))
}

func TestDocbankIncompleteConfigStaysDisabled(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		cfg := DocbankIntegration{URL: "https://example.test", APIKeyEnv: "KEY_" + hegel.Draw(ht, hegel.Text()), AllowHTTP: hegel.Draw(ht, hegel.Booleans())}
		missing := hegel.Draw(ht, hegel.SampledFrom([]string{"", " ", "\t\n"}))
		if hegel.Draw(ht, hegel.Booleans()) {
			cfg.URL = missing
		} else {
			cfg.APIKeyEnv = missing
		}
		require.ErrorContains(ht, cfg.Validate(), "url and api_key_env are required together")
		assert.False(ht, cfg.Configured())
		assert.False(ht, cfg.Enabled())
	}, hegel.WithTestCases(1000))
}
