package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	llmprovider "github.com/snowmerak/llm-provider"
	"github.com/snowmerak/llm-provider/providers/codex"
	"github.com/snowmerak/llm-provider/providers/openai"
)

var defaultRequestHeaders = []string{
	"X-OpenRouter-Cache",
	"X-OpenRouter-Cache-TTL",
	"X-OpenRouter-Cache-Clear",
	"X-Session-Id",
	"X-Grok-Conv-Id",
}

var defaultResponseHeaders = []string{
	"X-OpenRouter-Cache-Status",
	"X-OpenRouter-Cache-Age",
	"X-OpenRouter-Cache-TTL",
	"X-Generation-Id",
}

type route struct {
	id                     string
	prefix                 string
	provider               llmprovider.Provider
	models                 []string
	forwardHeaders         map[string]struct{}
	forwardResponseHeaders map[string]struct{}
}

type Gateway struct {
	routes map[string]*route
	order  []*route
	once   sync.Once
}

func New(config Config) (*Gateway, error) {
	gateway := &Gateway{routes: make(map[string]*route)}
	for _, providerConfig := range config.Providers {
		if err := providerConfig.validate(); err != nil {
			_ = gateway.Close()
			return nil, fmt.Errorf("gateway: %w", err)
		}
		if !providerConfig.Enabled {
			continue
		}
		prefix := providerConfig.Prefix
		if prefix == "" {
			prefix = providerConfig.ID
		}
		if _, exists := gateway.routes[prefix]; exists {
			_ = gateway.Close()
			return nil, fmt.Errorf("gateway: duplicate provider prefix %q", prefix)
		}
		provider, err := buildProvider(providerConfig)
		if err != nil {
			_ = gateway.Close()
			return nil, err
		}
		route := &route{
			id: providerConfig.ID, prefix: prefix, provider: provider,
			models:                 append([]string(nil), providerConfig.Models...),
			forwardHeaders:         headerSet(append(defaultRequestHeaders, providerConfig.ForwardHeaders...)),
			forwardResponseHeaders: headerSet(append(defaultResponseHeaders, providerConfig.ForwardResponseHeaders...)),
		}
		gateway.routes[prefix] = route
		gateway.order = append(gateway.order, route)
	}
	if len(gateway.routes) == 0 {
		return nil, errors.New("gateway: no providers are enabled")
	}
	sort.Slice(gateway.order, func(i, j int) bool { return gateway.order[i].prefix < gateway.order[j].prefix })
	return gateway, nil
}

func buildProvider(config ProviderConfig) (llmprovider.Provider, error) {
	switch config.Type {
	case "codex", "codex-app-server":
		options := make([]codex.Option, 0, 10)
		codexConfig := config.Codex
		if codexConfig.Command != "" || len(codexConfig.Args) > 0 {
			command := codexConfig.Command
			if command == "" {
				command = "codex"
			}
			options = append(options, codex.WithCommand(command, codexConfig.Args...))
		}
		if len(codexConfig.Environment) > 0 {
			environment := make([]string, 0, len(codexConfig.Environment))
			for key, value := range codexConfig.Environment {
				environment = append(environment, key+"="+value)
			}
			sort.Strings(environment)
			options = append(options, codex.WithEnvironment(environment...))
		}
		if codexConfig.Model != "" {
			options = append(options, codex.WithModel(codexConfig.Model))
		}
		if codexConfig.WorkingDirectory != "" {
			options = append(options, codex.WithWorkingDirectory(codexConfig.WorkingDirectory))
		}
		if codexConfig.BaseInstructions != "" {
			options = append(options, codex.WithBaseInstructions(codexConfig.BaseInstructions))
		}
		if codexConfig.Sandbox != "" {
			options = append(options, codex.WithSandbox(codex.SandboxMode(codexConfig.Sandbox)))
		}
		if codexConfig.ApprovalPolicy != "" {
			options = append(options, codex.WithApprovalPolicy(codex.ApprovalPolicy(codexConfig.ApprovalPolicy)))
		}
		if codexConfig.Ephemeral != nil {
			options = append(options, codex.WithEphemeral(*codexConfig.Ephemeral))
		}
		if codexConfig.ServiceName != "" {
			options = append(options, codex.WithServiceName(codexConfig.ServiceName))
		}
		if codexConfig.ExperimentalAPI != nil {
			options = append(options, codex.WithExperimentalAPI(*codexConfig.ExperimentalAPI))
		}
		return codex.New(options...), nil
	case "openrouter", "openai-compatible":
		baseURL := config.BaseURL
		if config.Type == "openrouter" && baseURL == "" {
			baseURL = defaultOpenRouterBaseURL
		}
		apiKey := config.APIKey
		if config.APIKeyEnv != "" {
			apiKey = os.Getenv(config.APIKeyEnv)
			if apiKey == "" {
				return nil, fmt.Errorf("gateway: provider %q environment variable %s is empty", config.ID, config.APIKeyEnv)
			}
		}
		if config.Type == "openrouter" && apiKey == "" {
			return nil, fmt.Errorf("gateway: provider %q requires api_key or api_key_env", config.ID)
		}
		options := []openai.Option{openai.WithBaseURL(baseURL), openai.WithAPIKey(apiKey)}
		for key, value := range config.Headers {
			options = append(options, openai.WithHeader(key, value))
		}
		return openai.New(options...), nil
	default:
		return nil, fmt.Errorf("gateway: provider %q has unsupported type %q", config.ID, config.Type)
	}
}

func (g *Gateway) Models(ctx context.Context) ([]llmprovider.Model, error) {
	models := make([]llmprovider.Model, 0)
	for _, route := range g.order {
		if len(route.models) > 0 {
			for _, id := range route.models {
				models = append(models, llmprovider.Model{ID: route.prefix + "/" + id, Object: "model", OwnedBy: route.id})
			}
			continue
		}
		lister, ok := route.provider.(llmprovider.ModelLister)
		if !ok {
			return nil, fmt.Errorf("gateway: provider %q cannot list models and has no static models", route.id)
		}
		listed, err := lister.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway: list models for %q: %w", route.id, err)
		}
		for _, model := range listed {
			model.ID = route.prefix + "/" + model.ID
			if model.Object == "" {
				model.Object = "model"
			}
			if model.OwnedBy == "" {
				model.OwnedBy = route.id
			}
			models = append(models, model)
		}
	}
	return models, nil
}

func (g *Gateway) resolve(model string) (*route, string, error) {
	prefix, backendModel, found := strings.Cut(model, "/")
	if !found || prefix == "" || backendModel == "" {
		return nil, "", fmt.Errorf("model %q must use a configured provider prefix", model)
	}
	route := g.routes[prefix]
	if route == nil {
		return nil, "", fmt.Errorf("model %q uses unknown provider prefix %q", model, prefix)
	}
	if len(route.models) > 0 && !contains(route.models, backendModel) {
		return nil, "", fmt.Errorf("model %q is not enabled for provider %q", backendModel, route.id)
	}
	return route, backendModel, nil
}

func (g *Gateway) Close() error {
	var result error
	g.once.Do(func() {
		for _, route := range g.order {
			result = errors.Join(result, route.provider.Close())
		}
	})
	return result
}

func headerSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			result[http.CanonicalHeaderKey(value)] = struct{}{}
		}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
