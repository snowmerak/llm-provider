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
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
	"github.com/snowmerak/llm-provider/providers/anthropic"
	"github.com/snowmerak/llm-provider/providers/codex"
	"github.com/snowmerak/llm-provider/providers/openai"
)

var defaultRequestHeaders = []string{
	"X-OpenRouter-Cache",
	"X-OpenRouter-Cache-TTL",
	"X-OpenRouter-Cache-Clear",
	"X-Session-Id",
	"X-Grok-Conv-Id",
	"Anthropic-Beta",
}

var defaultResponseHeaders = []string{
	"X-OpenRouter-Cache-Status",
	"X-OpenRouter-Cache-Age",
	"X-OpenRouter-Cache-TTL",
	"X-Generation-Id",
	"Request-Id",
}

var defaultReasoningEfforts = []string{
	"none",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

const (
	defaultModelCacheRefreshInterval = 15 * time.Minute
	defaultModelCacheRefreshTimeout  = 10 * time.Second
	minModelCacheRefreshInterval     = 5 * time.Minute
	maxModelCacheRefreshInterval     = 30 * time.Minute
)

type route struct {
	id                     string
	prefix                 string
	provider               llmprovider.Provider
	models                 []string
	modelMetadata          map[string]llmprovider.ModelMetadata
	forwardHeaders         map[string]struct{}
	forwardResponseHeaders map[string]struct{}
	modelMu                sync.RWMutex
	cachedModels           []llmprovider.Model
}

type Gateway struct {
	routes               map[string]*route
	order                []*route
	modelRefreshInterval time.Duration
	modelRefreshTimeout  time.Duration
	cacheCancel          context.CancelFunc
	cacheWG              sync.WaitGroup
	once                 sync.Once
}

func New(config Config) (*Gateway, error) {
	return NewContext(context.Background(), config)
}

// NewContext constructs a Gateway and warms its model cache. Cancelling ctx
// aborts startup discovery and closes any providers that were already built.
func NewContext(ctx context.Context, config Config) (*Gateway, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	refreshInterval, refreshTimeout, err := modelCacheSettings(config)
	if err != nil {
		return nil, err
	}
	gateway := &Gateway{
		routes:               make(map[string]*route),
		modelRefreshInterval: refreshInterval,
		modelRefreshTimeout:  refreshTimeout,
	}
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
			modelMetadata:          cloneModelMetadata(providerConfig.ModelMetadata),
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

	// Warm the cache before the Gateway is returned so the first /v1/models
	// request never waits on backend discovery. Individual provider failures
	// produce an empty cache entry and do not prevent startup.
	warmupContext, cancelWarmup := context.WithTimeout(ctx, gateway.modelRefreshTimeout)
	gateway.refreshModelCache(warmupContext)
	cancelWarmup()
	if err := ctx.Err(); err != nil {
		_ = gateway.Close()
		return nil, fmt.Errorf("gateway: initialize: %w", err)
	}

	cacheContext, cancelCache := context.WithCancel(context.Background())
	gateway.cacheCancel = cancelCache
	gateway.cacheWG.Add(1)
	go gateway.refreshModelCacheLoop(cacheContext)
	return gateway, nil
}

func modelCacheSettings(config Config) (time.Duration, time.Duration, error) {
	refreshInterval := defaultModelCacheRefreshInterval
	if config.ModelCacheRefreshInterval != "" {
		parsed, err := time.ParseDuration(config.ModelCacheRefreshInterval)
		if err != nil {
			return 0, 0, fmt.Errorf("gateway: parse model_cache_refresh_interval: %w", err)
		}
		refreshInterval = parsed
	}
	if refreshInterval < minModelCacheRefreshInterval || refreshInterval > maxModelCacheRefreshInterval {
		return 0, 0, fmt.Errorf("gateway: model_cache_refresh_interval must be between %s and %s", minModelCacheRefreshInterval, maxModelCacheRefreshInterval)
	}

	refreshTimeout := defaultModelCacheRefreshTimeout
	if config.ModelCacheRefreshTimeout != "" {
		parsed, err := time.ParseDuration(config.ModelCacheRefreshTimeout)
		if err != nil {
			return 0, 0, fmt.Errorf("gateway: parse model_cache_refresh_timeout: %w", err)
		}
		refreshTimeout = parsed
	}
	if refreshTimeout <= 0 {
		return 0, 0, errors.New("gateway: model_cache_refresh_timeout must be positive")
	}
	return refreshInterval, refreshTimeout, nil
}

func buildProvider(config ProviderConfig) (llmprovider.Provider, error) {
	switch config.Type {
	case "anthropic", "claude":
		apiKey := config.APIKey
		if config.APIKeyEnv != "" {
			apiKey = os.Getenv(config.APIKeyEnv)
			if apiKey == "" {
				return nil, fmt.Errorf("gateway: provider %q environment variable %s is empty", config.ID, config.APIKeyEnv)
			}
		}
		if apiKey == "" {
			return nil, fmt.Errorf("gateway: provider %q requires api_key or api_key_env", config.ID)
		}
		options := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
		if config.BaseURL != "" {
			options = append(options, anthropic.WithBaseURL(config.BaseURL))
		}
		for key, value := range config.Headers {
			options = append(options, anthropic.WithHeader(key, value))
		}
		for key, value := range config.Body {
			options = append(options, anthropic.WithBodyField(key, value))
		}
		return anthropic.New(options...), nil
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
		if codexConfig.Minimal != nil {
			options = append(options, codex.WithMinimalEnabled(*codexConfig.Minimal))
		}
		if codexConfig.BaseInstructions != "" {
			options = append(options, codex.WithBaseInstructions(codexConfig.BaseInstructions))
		}
		if len(codexConfig.ThreadStart) > 0 {
			options = append(options, codex.WithThreadStartParams(codexConfig.ThreadStart))
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
		cacheConfig := codexConfig.ConversationCache
		if cacheConfig.Type != "" || cacheConfig.TTL != "" || cacheConfig.MaxEntries > 0 {
			var ttl time.Duration
			if cacheConfig.TTL != "" {
				ttl, _ = time.ParseDuration(cacheConfig.TTL)
			}
			var conversationCache codex.ConversationCache
			if cacheConfig.Type == "redis" {
				redisConfig := cacheConfig.Redis
				if redisConfig.KeyPrefix == "" {
					redisConfig.KeyPrefix = "llm-provider:codex:" + config.ID + ":conversation:"
				}
				redisCache, cacheErr := codex.NewRedisConversationCache(codex.RedisConversationCacheOptions{
					Addresses: redisConfig.Addresses, Username: redisConfig.Username,
					Password: redisConfig.Password, Database: redisConfig.Database,
					ClientName: redisConfig.ClientName, KeyPrefix: redisConfig.KeyPrefix,
				})
				if cacheErr != nil {
					return nil, fmt.Errorf("gateway: provider %q create Redis conversation cache: %w", config.ID, cacheErr)
				}
				conversationCache = redisCache
			} else {
				conversationCache = codex.NewMemoryConversationCache(cacheConfig.MaxEntries)
			}
			options = append(options, codex.WithConversationCache(conversationCache, ttl))
		}
		return codex.New(options...), nil
	case "grok", "xai", "openrouter", "openai-compatible":
		baseURL := config.BaseURL
		if config.Type == "openrouter" && baseURL == "" {
			baseURL = defaultOpenRouterBaseURL
		}
		if (config.Type == "grok" || config.Type == "xai") && baseURL == "" {
			baseURL = defaultXAIBaseURL
		}
		apiKey := config.APIKey
		if config.APIKeyEnv != "" {
			apiKey = os.Getenv(config.APIKeyEnv)
			if apiKey == "" {
				return nil, fmt.Errorf("gateway: provider %q environment variable %s is empty", config.ID, config.APIKeyEnv)
			}
		}
		if (config.Type == "openrouter" || config.Type == "grok" || config.Type == "xai") && apiKey == "" {
			return nil, fmt.Errorf("gateway: provider %q requires api_key or api_key_env", config.ID)
		}
		options := []openai.Option{openai.WithBaseURL(baseURL), openai.WithAPIKey(apiKey)}
		if config.Type == "openrouter" {
			options = append(options, openai.WithReasoningEffortObject())
		}
		for key, value := range config.Headers {
			options = append(options, openai.WithHeader(key, value))
		}
		for key, value := range config.Body {
			options = append(options, openai.WithBodyField(key, value))
		}
		return openai.New(options...), nil
	default:
		return nil, fmt.Errorf("gateway: provider %q has unsupported type %q", config.ID, config.Type)
	}
}

func (g *Gateway) Models(ctx context.Context) ([]llmprovider.Model, error) {
	models := make([]llmprovider.Model, 0)
	for _, route := range g.order {
		models = append(models, route.modelsFromCache()...)
	}
	return models, nil
}

// Model returns the OpenAI-compatible model object for a prefixed model ID.
func (g *Gateway) Model(ctx context.Context, id string) (llmprovider.Model, error) {
	prefix, _, found := strings.Cut(id, "/")
	if !found || prefix == "" {
		return llmprovider.Model{}, fmt.Errorf("gateway: model %q must use a configured provider prefix", id)
	}
	route := g.routes[prefix]
	if route == nil {
		return llmprovider.Model{}, fmt.Errorf("gateway: model %q uses unknown provider prefix %q", id, prefix)
	}
	models := route.modelsFromCache()
	for _, model := range models {
		if model.ID == id {
			return model, nil
		}
	}
	return llmprovider.Model{}, fmt.Errorf("gateway: model %q was not found", id)
}

func (g *Gateway) discoverRouteModels(ctx context.Context, route *route) ([]llmprovider.Model, error) {
	lister, canList := route.provider.(llmprovider.ModelLister)
	if len(route.models) == 0 {
		if !canList {
			return nil, fmt.Errorf("gateway: provider %q cannot list models and has no static models", route.id)
		}
		listed, err := lister.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway: list models for %q: %w", route.id, err)
		}
		applyModelOverrides(route, listed)
		applyDefaultReasoningCapabilities(listed)
		return prefixedModels(route, listed), nil
	}

	// Static models remain the authoritative allowlist. If discovery works, use
	// it only to enrich the allowlisted entries with upstream metadata.
	metadata := make(map[string]llmprovider.Model)
	if canList {
		if listed, err := lister.ListModels(ctx); err == nil {
			for _, model := range listed {
				metadata[model.ID] = model
			}
		}
	}
	listed := make([]llmprovider.Model, 0, len(route.models))
	for _, id := range route.models {
		model := llmprovider.Model{ID: id, Object: "model", OwnedBy: route.id}
		if upstream, ok := metadata[id]; ok {
			model.Created = upstream.Created
			model.ContextLength = upstream.ContextLength
			model.MaxOutputTokens = upstream.MaxOutputTokens
			model.Capabilities = cloneModelCapabilities(upstream.Capabilities)
		}
		listed = append(listed, model)
	}
	applyModelOverrides(route, listed)
	applyDefaultReasoningCapabilities(listed)
	return prefixedModels(route, listed), nil
}

func applyDefaultReasoningCapabilities(models []llmprovider.Model) {
	for index := range models {
		if models[index].Capabilities == nil {
			models[index].Capabilities = &llmprovider.ModelCapabilities{}
		}
		if models[index].Capabilities.Reasoning != nil {
			continue
		}
		models[index].Capabilities.Reasoning = &llmprovider.ReasoningCapabilities{
			Supported:        true,
			Control:          llmprovider.ReasoningControlEffort,
			SupportedEfforts: append([]string(nil), defaultReasoningEfforts...),
		}
	}
}

func (g *Gateway) refreshModelCache(ctx context.Context) {
	var wait sync.WaitGroup
	for _, routeEntry := range g.order {
		wait.Add(1)
		go func(routeEntry *route) {
			defer wait.Done()
			models, err := g.discoverRouteModels(ctx, routeEntry)
			if err != nil {
				models = nil
			}
			routeEntry.storeModels(models)
		}(routeEntry)
	}
	wait.Wait()
}

func (g *Gateway) refreshModelCacheLoop(ctx context.Context) {
	defer g.cacheWG.Done()
	ticker := time.NewTicker(g.modelRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshContext, cancel := context.WithTimeout(ctx, g.modelRefreshTimeout)
			g.refreshModelCache(refreshContext)
			cancel()
		}
	}
}

func (r *route) storeModels(models []llmprovider.Model) {
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	r.cachedModels = cloneModels(models)
}

func (r *route) modelsFromCache() []llmprovider.Model {
	r.modelMu.RLock()
	defer r.modelMu.RUnlock()
	return cloneModels(r.cachedModels)
}

func cloneModels(source []llmprovider.Model) []llmprovider.Model {
	result := append([]llmprovider.Model(nil), source...)
	for index := range result {
		result[index].Capabilities = cloneModelCapabilities(result[index].Capabilities)
	}
	return result
}

func applyModelOverrides(route *route, models []llmprovider.Model) {
	for index := range models {
		override, ok := route.modelMetadata[models[index].ID]
		if !ok {
			continue
		}
		if override.ContextLength > 0 {
			models[index].ContextLength = override.ContextLength
		}
		if override.MaxOutputTokens > 0 {
			models[index].MaxOutputTokens = override.MaxOutputTokens
		}
		if override.Capabilities != nil {
			models[index].Capabilities = cloneModelCapabilities(override.Capabilities)
		}
	}
}

func cloneModelMetadata(source map[string]llmprovider.ModelMetadata) map[string]llmprovider.ModelMetadata {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]llmprovider.ModelMetadata, len(source))
	for model, metadata := range source {
		metadata.Capabilities = cloneModelCapabilities(metadata.Capabilities)
		result[model] = metadata
	}
	return result
}

func cloneModelCapabilities(source *llmprovider.ModelCapabilities) *llmprovider.ModelCapabilities {
	if source == nil {
		return nil
	}
	result := &llmprovider.ModelCapabilities{}
	if source.Reasoning != nil {
		reasoning := *source.Reasoning
		reasoning.SupportedEfforts = append([]string(nil), source.Reasoning.SupportedEfforts...)
		if source.Reasoning.DefaultEnabled != nil {
			value := *source.Reasoning.DefaultEnabled
			reasoning.DefaultEnabled = &value
		}
		result.Reasoning = &reasoning
	}
	return result
}

func prefixedModels(route *route, listed []llmprovider.Model) []llmprovider.Model {
	models := make([]llmprovider.Model, 0, len(listed))
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
	return models
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
		if g.cacheCancel != nil {
			g.cacheCancel()
			g.cacheWG.Wait()
		}
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
