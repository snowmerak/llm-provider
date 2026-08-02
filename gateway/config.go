// Package gateway exposes configured LLM providers through one
// OpenAI-compatible HTTP surface.
package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	llmprovider "github.com/snowmerak/llm-provider"
)

const defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

type Config struct {
	Listen                    string           `json:"listen"`
	ModelCacheRefreshInterval string           `json:"model_cache_refresh_interval,omitempty"`
	ModelCacheRefreshTimeout  string           `json:"model_cache_refresh_timeout,omitempty"`
	Providers                 []ProviderConfig `json:"providers"`
}

type ProviderConfig struct {
	ID                     string                               `json:"id"`
	Type                   string                               `json:"type"`
	Prefix                 string                               `json:"prefix"`
	Enabled                bool                                 `json:"enabled"`
	BaseURL                string                               `json:"base_url,omitempty"`
	APIKey                 string                               `json:"api_key,omitempty"`
	APIKeyEnv              string                               `json:"api_key_env,omitempty"`
	Headers                map[string]string                    `json:"headers,omitempty"`
	Body                   map[string]any                       `json:"body,omitempty"`
	ForwardHeaders         []string                             `json:"forward_headers,omitempty"`
	ForwardResponseHeaders []string                             `json:"forward_response_headers,omitempty"`
	Models                 []string                             `json:"models,omitempty"`
	ModelMetadata          map[string]llmprovider.ModelMetadata `json:"model_metadata,omitempty"`
	Codex                  CodexConfig                          `json:"codex,omitempty"`
}

type CodexConfig struct {
	Command          string            `json:"command,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	Model            string            `json:"model,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	BaseInstructions string            `json:"base_instructions,omitempty"`
	Minimal          bool              `json:"minimal,omitempty"`
	ThreadStart      map[string]any    `json:"thread_start,omitempty"`
	Sandbox          string            `json:"sandbox,omitempty"`
	ApprovalPolicy   string            `json:"approval_policy,omitempty"`
	Ephemeral        *bool             `json:"ephemeral,omitempty"`
	ServiceName      string            `json:"service_name,omitempty"`
	ExperimentalAPI  *bool             `json:"experimental_api,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("gateway: open config: %w", err)
	}
	defer file.Close()
	var config Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("gateway: decode config: %w", err)
	}
	config.expandEnvironment()
	return config, nil
}

func (c *Config) expandEnvironment() {
	c.Listen = os.ExpandEnv(c.Listen)
	c.ModelCacheRefreshInterval = os.ExpandEnv(c.ModelCacheRefreshInterval)
	c.ModelCacheRefreshTimeout = os.ExpandEnv(c.ModelCacheRefreshTimeout)
	for index := range c.Providers {
		provider := &c.Providers[index]
		provider.BaseURL = os.ExpandEnv(provider.BaseURL)
		provider.APIKey = os.ExpandEnv(provider.APIKey)
		for key, value := range provider.Headers {
			provider.Headers[key] = os.ExpandEnv(value)
		}
		if provider.Body != nil {
			provider.Body = expandAny(provider.Body).(map[string]any)
		}
		for key, value := range provider.Codex.Environment {
			provider.Codex.Environment[key] = os.ExpandEnv(value)
		}
		provider.Codex.WorkingDirectory = os.ExpandEnv(provider.Codex.WorkingDirectory)
		provider.Codex.BaseInstructions = os.ExpandEnv(provider.Codex.BaseInstructions)
		if provider.Codex.ThreadStart != nil {
			provider.Codex.ThreadStart = expandAny(provider.Codex.ThreadStart).(map[string]any)
		}
	}
}

func expandAny(value any) any {
	switch typed := value.(type) {
	case string:
		return os.ExpandEnv(typed)
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = expandAny(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = expandAny(item)
		}
		return result
	default:
		return value
	}
}

func (p ProviderConfig) validate() error {
	if !p.Enabled {
		return nil
	}
	if p.ID == "" {
		return fmt.Errorf("enabled provider has no id")
	}
	prefix := p.Prefix
	if prefix == "" {
		prefix = p.ID
	}
	if strings.Contains(prefix, "/") {
		return fmt.Errorf("provider %q prefix %q contains '/'", p.ID, prefix)
	}
	switch p.Type {
	case "anthropic", "claude", "codex", "codex-app-server", "openrouter", "openai-compatible":
	default:
		return fmt.Errorf("provider %q has unsupported type %q", p.ID, p.Type)
	}
	if p.Type == "openai-compatible" && p.BaseURL == "" {
		return fmt.Errorf("provider %q requires base_url", p.ID)
	}
	for model, metadata := range p.ModelMetadata {
		if model == "" {
			return fmt.Errorf("provider %q has metadata for an empty model id", p.ID)
		}
		if metadata.ContextLength < 0 || metadata.MaxOutputTokens < 0 {
			return fmt.Errorf("provider %q model %q metadata values cannot be negative", p.ID, model)
		}
	}
	return nil
}
