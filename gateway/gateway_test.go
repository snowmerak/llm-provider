package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
			_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"vendor/model-a","object":"model","owned_by":"vendor","max_model_len":262144}]}`)
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
		ID: "router", Type: "openai-compatible", Prefix: "openrouter", Enabled: true,
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
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(modelResponse.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "openrouter/vendor/model-a" ||
		models.Data[0].ContextLength != 262144 {
		t.Fatalf("models = %#v", models.Data)
	}
	detailResponse, err := http.Get(server.URL + "/v1/models/openrouter/vendor/model-a")
	if err != nil {
		t.Fatal(err)
	}
	defer detailResponse.Body.Close()
	var detail struct {
		ID            string `json:"id"`
		ContextLength int64  `json:"context_length"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detailResponse.StatusCode != http.StatusOK || detail.ID != "openrouter/vendor/model-a" ||
		detail.ContextLength != 262144 {
		t.Fatalf("model detail status=%d body=%#v", detailResponse.StatusCode, detail)
	}

	body := `{"model":"openrouter/vendor/model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"prompt_cache_key":"cache-key-1"}`
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
			`{"id":"allowed","context_window":131072,"max_tokens":4096},`+
			`{"id":"not-allowed","context_window":262144}]}`)
	}))
	defer upstream.Close()

	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Enabled: true,
		BaseURL: upstream.URL + "/v1", Models: []string{"allowed"},
		ModelMetadata: map[string]llmprovider.ModelMetadata{
			"allowed": {ContextLength: 200000, MaxOutputTokens: 8192},
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
	if len(models) != 1 || models[0].ID != "local/allowed" ||
		models[0].ContextLength != 200000 || models[0].MaxOutputTokens != 8192 {
		t.Fatalf("models = %#v", models)
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
	_, _ = io.WriteString(file, `{"listen":":8080","providers":[{"id":"local","type":"openai-compatible","enabled":true,"base_url":"http://localhost","api_key":"${TEST_GATEWAY_API_KEY}","body":{"prompt_cache_key":"${TEST_CACHE_KEY}","nested":{"value":"${TEST_CACHE_KEY}"}},"model_metadata":{"test-model":{"context_length":123456,"max_output_tokens":8192}}}]}`)
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
	if metadata.ContextLength != 123456 || metadata.MaxOutputTokens != 8192 {
		t.Fatalf("model metadata = %#v", metadata)
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
