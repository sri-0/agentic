package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ModelsConfig struct {
	Providers []Provider `yaml:"providers"`
}

type Provider struct {
	ID        string  `yaml:"id"`
	Name      string  `yaml:"name"`
	BaseURL   string  `yaml:"base_url"`
	APIKeyEnv string  `yaml:"api_key_env"`
	Models    []Model `yaml:"models"`

	// mTLS / custom certificate settings
	SSLClientCertEnv       string `yaml:"ssl_client_cert_env"`
	SSLClientKeyEnv        string `yaml:"ssl_client_key_env"`
	SSLClientKeyPasswordEnv string `yaml:"ssl_client_key_password_env"`
	SSLTrustStoreEnv       string `yaml:"ssl_trust_store_env"`
}

// APIKey reads the API key from the environment variable specified by APIKeyEnv.
func (p *Provider) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

// HTTPClient returns an *http.Client configured with mTLS certs if set,
// or nil if no custom TLS is needed (caller should use http.DefaultClient).
func (p *Provider) HTTPClient() (*http.Client, error) {
	certPath := envOrEmpty(p.SSLClientCertEnv)
	keyPath := envOrEmpty(p.SSLClientKeyEnv)
	trustStorePath := envOrEmpty(p.SSLTrustStoreEnv)

	if certPath == "" && keyPath == "" && trustStorePath == "" {
		return nil, nil
	}

	tlsCfg := &tls.Config{}

	// Load client certificate + key for mTLS
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("loading client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Load custom trust store (CA bundle)
	if trustStorePath != "" {
		caCert, err := os.ReadFile(trustStorePath)
		if err != nil {
			return nil, fmt.Errorf("reading trust store: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse trust store certificates")
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func envOrEmpty(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
}

// DefaultSupportedParameters is used when a model does not specify supported_parameters.
var DefaultSupportedParameters = []string{
	"temperature", "top_p", "max_tokens", "frequency_penalty",
	"presence_penalty", "stop", "seed", "response_format",
	"top_k", "reasoning_effort",
}

type Model struct {
	ID                  string        `yaml:"id"       json:"id"`
	Name                string        `yaml:"name"     json:"name"`
	OwnedBy             string        `yaml:"owned_by" json:"owned_by"`
	Description         string        `yaml:"description,omitempty" json:"description,omitempty"`
	Created             string        `yaml:"created,omitempty" json:"-"`
	ContextLength       int           `yaml:"context_length,omitempty" json:"context_length,omitempty"`
	Capabilities        []string      `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	SupportedParameters []string      `yaml:"supported_parameters,omitempty" json:"supported_parameters,omitempty"`
	Architecture        *Architecture `yaml:"architecture,omitempty" json:"architecture,omitempty"`
}

// EffectiveSupportedParameters returns the model's supported parameters,
// falling back to DefaultSupportedParameters if none are configured.
func (m *Model) EffectiveSupportedParameters() []string {
	if len(m.SupportedParameters) > 0 {
		return m.SupportedParameters
	}
	return DefaultSupportedParameters
}

// HasCapability returns true if the model lists the given capability.
func (m *Model) HasCapability(cap string) bool {
	for _, c := range m.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// IsMultimodal returns true if the model accepts more than just text input.
func (m *Model) IsMultimodal() bool {
	if m.Architecture == nil {
		return false
	}
	for _, mod := range m.Architecture.InputModalities {
		if mod != "text" {
			return true
		}
	}
	return false
}

// CreatedUnix parses Created (YYYY-MM-DD) and returns the unix timestamp.
// Returns 0 if not set or unparseable.
func (m *Model) CreatedUnix() int64 {
	if m.Created == "" {
		return 0
	}
	t, err := time.Parse("2006-01-02", m.Created)
	if err != nil {
		return 0
	}
	return t.Unix()
}

type Architecture struct {
	InputModalities  []string `yaml:"input_modalities" json:"input_modalities"`
	OutputModalities []string `yaml:"output_modalities" json:"output_modalities"`
	Tokenizer        string   `yaml:"tokenizer,omitempty" json:"tokenizer,omitempty"`
	InstructType     string   `yaml:"instruct_type,omitempty" json:"instruct_type,omitempty"`
}

// Modality returns a computed string like "text+image->text" from the
// input and output modality lists.
func (a *Architecture) Modality() string {
	if len(a.InputModalities) == 0 && len(a.OutputModalities) == 0 {
		return ""
	}
	s := ""
	for i, m := range a.InputModalities {
		if i > 0 {
			s += "+"
		}
		s += m
	}
	s += "->"
	for i, m := range a.OutputModalities {
		if i > 0 {
			s += "+"
		}
		s += m
	}
	return s
}

func LoadModels(path string) (*ModelsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading models config: %w", err)
	}
	var cfg ModelsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing models config: %w", err)
	}
	return &cfg, nil
}

// AllModels returns a flat list of all models across all providers.
func (c *ModelsConfig) AllModels() []Model {
	var all []Model
	for _, p := range c.Providers {
		all = append(all, p.Models...)
	}
	return all
}

// FindProvider returns the provider with the given ID, or nil if not found.
func (c *ModelsConfig) FindProvider(id string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

// FindProviderForModel returns the provider that hosts the given model ID.
func (c *ModelsConfig) FindProviderForModel(modelID string) *Provider {
	for i := range c.Providers {
		for _, m := range c.Providers[i].Models {
			if m.ID == modelID {
				return &c.Providers[i]
			}
		}
	}
	return nil
}
