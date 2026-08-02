package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestModelCacheWarmsAtStartupAndServesReadsWithoutDiscovery(t *testing.T) {
	var modelRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			http.NotFound(writer, request)
			return
		}
		modelRequests.Add(1)
		_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"cached-model"}]}`)
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL + "/v1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if got := modelRequests.Load(); got != 1 {
		t.Fatalf("startup model requests = %d, want 1", got)
	}

	for range 3 {
		models, err := gateway.Models(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 || models[0].ID != "local/cached-model" {
			t.Fatalf("models = %#v", models)
		}
		if reasoning := models[0].Capabilities.Reasoning; reasoning.Control != llmprovider.ReasoningControlEffort ||
			!slices.Equal(reasoning.SupportedEfforts, defaultReasoningEfforts) {
			t.Fatalf("fallback reasoning = %#v", reasoning)
		}
	}
	if _, err := gateway.Model(t.Context(), "local/cached-model"); err != nil {
		t.Fatal(err)
	}
	if got := modelRequests.Load(); got != 1 {
		t.Fatalf("model requests after cache reads = %d, want 1", got)
	}
}

func TestUnavailableDynamicProviderIsOmitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"healthy-model"}]}`)
	}))
	defer upstream.Close()
	deadUpstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadUpstream.URL
	deadUpstream.Close()

	gateway, err := New(Config{
		ModelCacheRefreshTimeout: "1s",
		Providers: []ProviderConfig{
			{ID: "dead", Type: "openai-compatible", Enabled: true, BaseURL: deadURL + "/v1"},
			{ID: "healthy", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL + "/v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	models, err := gateway.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "healthy/healthy-model" {
		t.Fatalf("models = %#v", models)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("model endpoint status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFailedRefreshRemovesDynamicProviderFromCache(t *testing.T) {
	var unavailable atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if unavailable.Load() {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"temporary-model"}]}`)
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL + "/v1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	unavailable.Store(true)
	gateway.refreshModelCache(t.Context())
	models, err := gateway.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("models after failed refresh = %#v", models)
	}
}

func TestModelCacheRefreshLoopRefreshesPeriodically(t *testing.T) {
	var modelRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		modelRequests.Add(1)
		_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"periodic-model"}]}`)
	}))
	defer upstream.Close()
	provider, err := buildProvider(ProviderConfig{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL + "/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	routeEntry := &route{id: "local", prefix: "local", provider: provider}
	gateway := &Gateway{
		order: []*route{routeEntry}, modelRefreshInterval: 10 * time.Millisecond,
		modelRefreshTimeout: time.Second,
	}
	ctx, cancel := context.WithCancel(t.Context())
	gateway.cacheWG.Add(1)
	go gateway.refreshModelCacheLoop(ctx)
	t.Cleanup(func() {
		cancel()
		gateway.cacheWG.Wait()
		_ = provider.Close()
	})

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("periodic model refresh did not run; requests = %d", modelRequests.Load())
		case <-poll.C:
			models := routeEntry.modelsFromCache()
			if len(models) == 1 && models[0].ID == "local/periodic-model" {
				return
			}
		}
	}
}

func TestModelCacheRefreshIntervalBounds(t *testing.T) {
	for _, interval := range []string{"4m59s", "30m1s"} {
		if _, _, err := modelCacheSettings(Config{ModelCacheRefreshInterval: interval}); err == nil {
			t.Fatalf("model cache interval %q was accepted", interval)
		}
	}
	interval, timeout, err := modelCacheSettings(Config{ModelCacheRefreshInterval: "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if interval != 5*time.Minute || timeout != defaultModelCacheRefreshTimeout {
		t.Fatalf("model cache settings = %s, %s", interval, timeout)
	}
}

func TestModelsAndChatRouting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			if request.Header.Get("Authorization") != "Bearer backend-secret" {
				t.Errorf("model authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"vendor/model-a","object":"model","owned_by":"vendor","max_model_len":262144,"reasoning":{"supported_efforts":["high","medium","low"],"default_effort":"medium"}}]}`)
		case "/v1/chat/completions":
			if request.Header.Get("Authorization") != "Bearer backend-secret" {
				t.Errorf("chat authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("X-OpenRouter-Cache") != "true" ||
				request.Header.Get("X-OpenRouter-Cache-TTL") != "600" ||
				request.Header.Get("X-Grok-Conv-Id") != "conversation-1" {
				t.Errorf("forwarded headers = %#v", request.Header)
			}
			if request.Header.Get("X-Not-Allowed") != "" {
				t.Errorf("unexpected forwarded header = %q", request.Header.Get("X-Not-Allowed"))
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "vendor/model-a" || body["prompt_cache_key"] != "cache-key-1" {
				t.Errorf("upstream body = %#v", body)
			}
			if body["reasoning"].(map[string]any)["effort"] != "high" {
				t.Errorf("reasoning = %#v", body["reasoning"])
			}
			if body["cache_control"].(map[string]any)["type"] != "ephemeral" {
				t.Errorf("default cache body = %#v", body["cache_control"])
			}
			messages := body["messages"].([]any)
			content := messages[0].(map[string]any)["content"].([]any)
			if content[0].(map[string]any)["cache_control"].(map[string]any)["type"] != "ephemeral" {
				t.Errorf("content cache breakpoint = %#v", content)
			}
			writer.Header().Set("X-OpenRouter-Cache-Status", "HIT")
			_, _ = io.WriteString(writer, `{"id":"chat_1","model":"vendor/model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens":1,"total_tokens":11}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "router", Type: "openrouter", Prefix: "openrouter", Enabled: true,
		BaseURL: upstream.URL + "/v1", APIKey: "backend-secret",
		Body: map[string]any{"cache_control": map[string]any{"type": "ephemeral"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	modelResponse, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer modelResponse.Body.Close()
	var models struct {
		Data []struct {
			ID            string                         `json:"id"`
			ContextLength int64                          `json:"context_length"`
			Capabilities  *llmprovider.ModelCapabilities `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(modelResponse.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	modelReasoning := models.Data[0].Capabilities.Reasoning
	if len(models.Data) != 1 || models.Data[0].ID != "openrouter/vendor/model-a" ||
		models.Data[0].ContextLength != 262144 || modelReasoning.DefaultEffort != "medium" ||
		!slices.Equal(modelReasoning.SupportedEfforts, []string{"high", "medium", "low"}) {
		t.Fatalf("models = %#v", models.Data)
	}
	detailResponse, err := http.Get(server.URL + "/v1/models/openrouter/vendor/model-a")
	if err != nil {
		t.Fatal(err)
	}
	defer detailResponse.Body.Close()
	var detail struct {
		ID            string                         `json:"id"`
		ContextLength int64                          `json:"context_length"`
		Capabilities  *llmprovider.ModelCapabilities `json:"capabilities"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	detailReasoning := detail.Capabilities.Reasoning
	if detailResponse.StatusCode != http.StatusOK || detail.ID != "openrouter/vendor/model-a" ||
		detail.ContextLength != 262144 || detailReasoning.DefaultEffort != "medium" ||
		!slices.Equal(detailReasoning.SupportedEfforts, []string{"high", "medium", "low"}) {
		t.Fatalf("model detail status=%d body=%#v", detailResponse.StatusCode, detail)
	}

	body := `{"model":"openrouter/vendor/model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"prompt_cache_key":"cache-key-1","reasoning_effort":"high"}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer gateway-client-secret")
	request.Header.Set("X-OpenRouter-Cache", "true")
	request.Header.Set("X-OpenRouter-Cache-TTL", "600")
	request.Header.Set("X-Grok-Conv-Id", "conversation-1")
	request.Header.Set("X-Not-Allowed", "do-not-forward")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d: %s", response.StatusCode, data)
	}
	if response.Header.Get("X-OpenRouter-Cache-Status") != "HIT" {
		t.Fatalf("cache status = %q", response.Header.Get("X-OpenRouter-Cache-Status"))
	}
	var completion struct {
		Model string `json:"model"`
		Usage struct {
			PromptDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&completion); err != nil {
		t.Fatal(err)
	}
	if completion.Model != "openrouter/vendor/model-a" || completion.Usage.PromptDetails.CachedTokens != 8 {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestStaticModelAllowlistIsEnrichedFromUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"object":"list","data":[`+
			`{"id":"allowed","context_window":131072,"max_tokens":4096,"capabilities":{"reasoning":{"supported":true,"control":"effort","supported_efforts":["low","medium"],"default_effort":"medium"}}},`+
			`{"id":"not-allowed","context_window":262144}]}`)
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true,
		BaseURL: upstream.URL + "/v1", Models: []string{"allowed"},
		ModelMetadata: map[string]llmprovider.ModelMetadata{
			"allowed": {
				ContextLength: 200000, MaxOutputTokens: 8192,
				Capabilities: &llmprovider.ModelCapabilities{Reasoning: &llmprovider.ReasoningCapabilities{
					Supported: true, Control: llmprovider.ReasoningControlEffort,
					SupportedEfforts: []string{"low", "high"}, DefaultEffort: "high",
				}},
			},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	models, err := gateway.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reasoning := models[0].Capabilities.Reasoning
	if len(models) != 1 || models[0].ID != "local/allowed" ||
		models[0].ContextLength != 200000 || models[0].MaxOutputTokens != 8192 ||
		reasoning.DefaultEffort != "high" ||
		!slices.Equal(reasoning.SupportedEfforts, []string{"low", "high"}) {
		t.Fatalf("models = %#v", models)
	}
}

func TestXAIModelsUseDefaultReasoningCapabilitiesWithoutDiscovery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"object":"list","data":[`+
			`{"id":"grok-4.5","object":"model","owned_by":"xai"},`+
			`{"id":"grok-4.20-multi-agent","object":"model","owned_by":"xai"},`+
			`{"id":"grok-unknown","object":"model","owned_by":"xai"}]}`)
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "grok", Type: "xai", Enabled: true, BaseURL: upstream.URL + "/v1", APIKey: "secret",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	models, err := gateway.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %#v", models)
	}
	for _, model := range models {
		reasoning := model.Capabilities.Reasoning
		if reasoning.Control != llmprovider.ReasoningControlEffort || reasoning.DefaultEffort != "" || reasoning.Mandatory ||
			!slices.Equal(reasoning.SupportedEfforts, defaultReasoningEfforts) {
			t.Fatalf("Grok fallback = %#v", model)
		}
	}
}

func TestStreamingPreservesPrefixedModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-OpenRouter-Cache-Status", "MISS")
		_, _ = io.WriteString(writer, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"backend\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL,
		Models: []string{"backend"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		bytes.NewBufferString(`{"model":"local/backend","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("X-OpenRouter-Cache-Status") != "MISS" {
		t.Fatalf("stream cache status = %q", response.Header.Get("X-OpenRouter-Cache-Status"))
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"model":"local/backend"`) || !strings.Contains(string(data), "data: [DONE]") {
		t.Fatalf("stream = %s", data)
	}
}

func TestLoadConfigExpandsEnvironment(t *testing.T) {
	t.Setenv("TEST_GATEWAY_API_KEY", "expanded-secret")
	t.Setenv("TEST_CACHE_KEY", "tenant:cache-v1")
	file, err := os.CreateTemp(t.TempDir(), "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, `{"listen":":8080","providers":[{"id":"local","type":"openai-compatible","enabled":true,"base_url":"http://localhost","api_key":"${TEST_GATEWAY_API_KEY}","body":{"prompt_cache_key":"${TEST_CACHE_KEY}","nested":{"value":"${TEST_CACHE_KEY}"}},"model_metadata":{"test-model":{"context_length":123456,"max_output_tokens":8192,"capabilities":{"reasoning":{"supported":true,"control":"effort","supported_efforts":["low","high"],"default_effort":"high"}}}}}]}`)
	_ = file.Close()
	config, err := LoadConfig(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if config.Providers[0].APIKey != "expanded-secret" {
		t.Fatalf("api key = %q", config.Providers[0].APIKey)
	}
	if config.Providers[0].Body["prompt_cache_key"] != "tenant:cache-v1" ||
		config.Providers[0].Body["nested"].(map[string]any)["value"] != "tenant:cache-v1" {
		t.Fatalf("expanded body = %#v", config.Providers[0].Body)
	}
	metadata := config.Providers[0].ModelMetadata["test-model"]
	reasoning := metadata.Capabilities.Reasoning
	if metadata.ContextLength != 123456 || metadata.MaxOutputTokens != 8192 ||
		reasoning.DefaultEffort != "high" ||
		!slices.Equal(reasoning.SupportedEfforts, []string{"low", "high"}) {
		t.Fatalf("model metadata = %#v", metadata)
	}
}

func TestLoadConfigDecodesCodexMinimalOverrides(t *testing.T) {
	t.Setenv("TEST_CODEX_CWD", `C:\workspace`)
	file, err := os.CreateTemp(t.TempDir(), "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, `{"providers":[{"id":"codex-minimal","type":"codex-app-server","enabled":true,"codex":{"minimal":true,"thread_start":{"cwd":"${TEST_CODEX_CWD}","config":{"include_environment_context":true}}}}]}`)
	_ = file.Close()
	config, err := LoadConfig(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	codexConfig := config.Providers[0].Codex
	if codexConfig.Minimal == nil || !*codexConfig.Minimal {
		t.Fatal("minimal was not decoded")
	}
	if codexConfig.ThreadStart["cwd"] != `C:\workspace` {
		t.Fatalf("thread_start cwd = %#v", codexConfig.ThreadStart["cwd"])
	}
	threadConfig := codexConfig.ThreadStart["config"].(map[string]any)
	if threadConfig["include_environment_context"] != true {
		t.Fatalf("thread_start config = %#v", threadConfig)
	}
}

func TestCodexConversationCacheConfigValidation(t *testing.T) {
	base := ProviderConfig{ID: "codex", Type: "codex", Enabled: true}
	if err := base.validate(); err != nil {
		t.Fatalf("default in-memory cache config: %v", err)
	}

	redisConfig := base
	redisConfig.Codex.ConversationCache = CodexConversationCacheConfig{
		Type: "redis", TTL: "30m",
		Redis: CodexRedisConfig{Addresses: []string{"127.0.0.1:6379"}},
	}
	if err := redisConfig.validate(); err != nil {
		t.Fatalf("Redis cache config: %v", err)
	}

	invalid := base
	invalid.Codex.ConversationCache.Type = "disk"
	if err := invalid.validate(); err == nil {
		t.Fatal("expected unsupported cache type error")
	}
	invalid = base
	invalid.Codex.ConversationCache.Type = "redis"
	if err := invalid.validate(); err == nil {
		t.Fatal("expected missing Redis addresses error")
	}
	invalid = base
	invalid.Codex.ConversationCache.TTL = "never"
	if err := invalid.validate(); err == nil {
		t.Fatal("expected invalid cache ttl error")
	}
}

func TestReasoningCapabilityConfigValidation(t *testing.T) {
	valid := ProviderConfig{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: "http://localhost",
		ModelMetadata: map[string]llmprovider.ModelMetadata{"model": {
			Capabilities: &llmprovider.ModelCapabilities{Reasoning: &llmprovider.ReasoningCapabilities{
				Supported: true, Control: llmprovider.ReasoningControlEffort,
				SupportedEfforts: []string{"low", "high"}, DefaultEffort: "high",
			}},
		}},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid reasoning metadata: %v", err)
	}

	invalid := valid
	invalid.ModelMetadata = cloneModelMetadata(valid.ModelMetadata)
	invalid.ModelMetadata["model"].Capabilities.Reasoning.Control = llmprovider.ReasoningControlToggle
	if err := invalid.validate(); err == nil {
		t.Fatal("expected toggle-with-efforts validation error")
	}

	invalid = valid
	invalid.ModelMetadata = cloneModelMetadata(valid.ModelMetadata)
	invalid.ModelMetadata["model"].Capabilities.Reasoning.Control = llmprovider.ReasoningControlTokenBudget
	invalid.ModelMetadata["model"].Capabilities.Reasoning.SupportedEfforts = nil
	invalid.ModelMetadata["model"].Capabilities.Reasoning.DefaultEffort = ""
	if err := invalid.validate(); err == nil {
		t.Fatal("expected token-budget-without-max-tokens validation error")
	}
}

func TestRejectsUnknownPrefix(t *testing.T) {
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: "http://localhost",
		Models: []string{"model"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"missing/model","messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestLocalEmbeddingModelDiscoveryAndRouting(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"chat-model"},{"id":"text-embedding-local"}]}`)
		case "/v1/embeddings":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "text-embedding-local" || body["encoding_format"] != "base64" || body["dimensions"] != float64(768) {
				t.Errorf("embedding body = %#v", body)
			}
			input, ok := body["input"].([]any)
			if !ok || len(input) != 2 || input[0] != "hello" {
				t.Errorf("embedding input = %#v", body["input"])
			}
			_, _ = io.WriteString(writer, `{"object":"list","data":[{"object":"embedding","embedding":"AAAA","index":0}],"model":"text-embedding-local","usage":{"prompt_tokens":2,"total_tokens":2},"backend_extension":{"cached":true}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "lmstudio", Type: "openai-compatible", Prefix: "local", Enabled: true,
		BaseURL: upstream.URL + "/v1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	modelResponse, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer modelResponse.Body.Close()
	var listed struct {
		Data []llmprovider.Model `json:"data"`
	}
	if err := json.NewDecoder(modelResponse.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 2 || listed.Data[1].ID != "local/text-embedding-local" {
		t.Fatalf("models = %#v", listed.Data)
	}

	embeddingResponse, err := http.Post(server.URL+"/v1/embeddings", "application/json", strings.NewReader(
		`{"model":"local/text-embedding-local","input":["hello","world"],"encoding_format":"base64","dimensions":768}`))
	if err != nil {
		t.Fatal(err)
	}
	defer embeddingResponse.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(embeddingResponse.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if embeddingResponse.StatusCode != http.StatusOK || result["model"] != "local/text-embedding-local" {
		t.Fatalf("status=%d result=%#v", embeddingResponse.StatusCode, result)
	}
	if result["backend_extension"].(map[string]any)["cached"] != true {
		t.Fatalf("extension was not preserved: %#v", result)
	}
}

func TestNativeResponsesRoutingAndChatWirePreservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(writer, `{"data":[{"id":"model"}]}`)
		case "/v1/responses":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "model" || body["input"] != "hello" {
				t.Errorf("responses body = %#v", body)
			}
			if body["stream"] == true {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"model\",\"status\":\"in_progress\"}}\n\n")
				_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"model\",\"status\":\"completed\"}}\n\n")
				return
			}
			_, _ = io.WriteString(writer, `{"id":"resp_1","object":"response","model":"model","status":"completed","output":[],"vendor_field":"kept"}`)
		case "/v1/chat/completions":
			_, _ = io.WriteString(writer, `{"id":"chat_1","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"ok","refusal":null,"annotations":[{"type":"url_citation"}]},"finish_reason":"stop","logprobs":{"content":[]}}],"system_fingerprint":"fp_test","service_tier":"default"}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL + "/v1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"local/model","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(data), `"model":"local/model"`) || !strings.Contains(string(data), `"vendor_field":"kept"`) {
		t.Fatalf("responses status=%d body=%s", response.StatusCode, data)
	}

	streamResponse, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"local/model","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	streamData, _ := io.ReadAll(streamResponse.Body)
	_ = streamResponse.Body.Close()
	if !strings.Contains(string(streamData), "event: response.created") || strings.Count(string(streamData), `"model":"local/model"`) != 2 {
		t.Fatalf("responses stream = %s", streamData)
	}

	chatResponse, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"local/model","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chatData, _ := io.ReadAll(chatResponse.Body)
	_ = chatResponse.Body.Close()
	for _, expected := range []string{`"system_fingerprint":"fp_test"`, `"service_tier":"default"`, `"annotations"`, `"logprobs"`, `"model":"local/model"`} {
		if !strings.Contains(string(chatData), expected) {
			t.Fatalf("chat response lost %s: %s", expected, chatData)
		}
	}
}

func TestResponsesChatAdapterCommonSubset(t *testing.T) {
	decoded := responseRequest{
		Model: "codex/model", Instructions: "be concise", Input: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
		Tools:      []map[string]any{{"type": "function", "name": "lookup", "description": "Lookup", "parameters": map[string]any{"type": "object"}}},
		ToolChoice: map[string]any{"type": "function", "name": "lookup"},
		Reasoning:  &responseReasoning{Effort: "high"},
	}
	chat, err := responseToChat(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 || chat.Messages[0].Role != llmprovider.RoleDeveloper || chat.Messages[1].Content != "hello" || chat.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("messages = %#v", chat.Messages)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tools = %#v", chat.Tools)
	}
	if chat.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q", chat.ReasoningEffort)
	}
	choice := chat.ToolChoice.(map[string]any)
	if choice["function"].(map[string]string)["name"] != "lookup" {
		t.Fatalf("tool choice = %#v", chat.ToolChoice)
	}
}

func TestDecodeChatRequestReasoningEffort(t *testing.T) {
	request, stream, err := decodeChatRequest(strings.NewReader(
		`{"model":"codex/model","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high","reasoning":{"enabled":true}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if stream || request.ReasoningEffort != "high" {
		t.Fatalf("stream=%v reasoning effort=%q", stream, request.ReasoningEffort)
	}
	if _, duplicate := request.Extra["reasoning_effort"]; duplicate {
		t.Fatalf("reasoning_effort was also retained as an extension: %#v", request.Extra)
	}
	if request.Extra["reasoning"].(map[string]any)["enabled"] != true {
		t.Fatalf("reasoning extension = %#v", request.Extra["reasoning"])
	}
}

func TestGatewayPreservesOpenAIErrorEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true, BaseURL: upstream.URL, Models: []string{"model"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"local/model","messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusTooManyRequests || envelope.Error.Message != "slow down" ||
		envelope.Error.Type != "rate_limit_error" || envelope.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("status=%d envelope=%#v", response.Code, envelope)
	}
}
