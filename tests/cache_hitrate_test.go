package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
	"github.com/snowmerak/llm-provider/gateway"
	openai "github.com/snowmerak/llm-provider/providers/openai"
)

const defaultMinimumCacheHitRate = 0.50

func TestCacheHitRate(t *testing.T) {
	t.Run("Codex", testCodexCacheHitRate)
	t.Run("Grok", testGrokCacheHitRate)
	t.Run("OpenRouter", testOpenRouterCacheHitRate)
	t.Run("OpenAICompatible", testOpenAICompatibleCacheHitRate)
}

func testCodexCacheHitRate(t *testing.T) {
	requireRegressionEnabled(t, "CODEX")
	client, model, closeGateway := configuredGatewayClient(t, "codex", "CODEX_CACHE_MODEL", "gpt-5.6-luna")
	defer closeGateway()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	request := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{
			{Role: llmprovider.RoleSystem, Content: stablePrefix("codex")},
			{Role: llmprovider.RoleUser, Content: "Call cache_regression_lookup with key demo before answering."},
		},
		Tools: []llmprovider.Tool{{
			Type: llmprovider.ToolTypeFunction,
			Function: llmprovider.FunctionDefinition{
				Name: "cache_regression_lookup", Description: "Returns a cache regression marker.",
				Parameters: map[string]any{
					"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}},
					"required": []string{"key"}, "additionalProperties": false,
				},
			},
		}},
		ToolChoice: llmprovider.NamedToolChoice("cache_regression_lookup"),
	}
	warmup, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(warmup.Choices) == 0 || len(warmup.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("Codex returned no delegated tool call: %#v", warmup)
	}
	call := warmup.Choices[0].Message.ToolCalls[0]
	request.ConversationID = warmup.ConversationID
	request.ToolChoice = llmprovider.ToolChoiceNone
	request.Messages = append(request.Messages, warmup.Choices[0].Message, llmprovider.Message{
		Role: llmprovider.RoleTool, ToolCallID: call.ID,
		Content: `{"value":"CODEX_CACHE_REGRESSION_OK"}`,
	})
	probe, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if probe.ConversationID != warmup.ConversationID || probe.ID != warmup.ID {
		t.Fatalf("Codex delegated tool changed thread/turn: warmup=%q/%q probe=%q/%q",
			warmup.ConversationID, warmup.ID, probe.ConversationID, probe.ID)
	}
	requireCacheHitRate(t, "CODEX", probe.Usage)
}

func testGrokCacheHitRate(t *testing.T) {
	requireRegressionEnabled(t, "GROK")
	client, model, closeGateway := configuredGatewayClient(t, "grok", "GROK_CACHE_MODEL", "grok-4.5")
	defer closeGateway()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conversationID := uniqueID("grok")
	request := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{
			{Role: llmprovider.RoleSystem, Content: stablePrefix("grok")},
			{Role: llmprovider.RoleUser, Content: "Reply with exactly GROK_CACHE_WARMUP."},
		},
		Headers: http.Header{"X-Grok-Conv-Id": []string{conversationID}},
	}
	warmup, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages, warmup.Choices[0].Message, llmprovider.Message{
		Role: llmprovider.RoleUser, Content: "Reply with exactly GROK_CACHE_PROBE.",
	})
	probe, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	requireCacheHitRate(t, "GROK", probe.Usage)
}

func testOpenRouterCacheHitRate(t *testing.T) {
	requireRegressionEnabled(t, "OPENROUTER")
	client, model, closeGateway := configuredGatewayClient(t, "openrouter", "OPENROUTER_CACHE_MODEL", "openai/gpt-4.1-mini")
	defer closeGateway()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	request := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{
			{Role: llmprovider.RoleSystem, Content: stablePrefix("openrouter")},
			{Role: llmprovider.RoleUser, Content: "Reply with exactly OPENROUTER_CACHE_WARMUP."},
		},
		Extra: map[string]any{"session_id": uniqueID("openrouter-prompt")},
	}
	warmup, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages, warmup.Choices[0].Message, llmprovider.Message{
		Role: llmprovider.RoleUser, Content: "Reply with exactly OPENROUTER_CACHE_PROBE.",
	})
	probe, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	requireCacheHitRate(t, "OPENROUTER", probe.Usage)

	marker := uniqueID("openrouter-response")
	responseCacheRequest := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{{
			Role: llmprovider.RoleUser, Content: "Reply with exactly " + marker,
		}},
		Headers: http.Header{
			"X-OpenRouter-Cache":     []string{"true"},
			"X-OpenRouter-Cache-TTL": []string{"600"},
		},
	}
	miss, err := client.Chat(ctx, responseCacheRequest)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := client.Chat(ctx, responseCacheRequest)
	if err != nil {
		t.Fatal(err)
	}
	if status := miss.Headers.Get("X-OpenRouter-Cache-Status"); status != "MISS" {
		t.Fatalf("OpenRouter first response-cache status = %q, want MISS", status)
	}
	if status := hit.Headers.Get("X-OpenRouter-Cache-Status"); status != "HIT" {
		t.Fatalf("OpenRouter second response-cache status = %q, want HIT", status)
	}
	if hit.Usage.TotalTokens != 0 {
		t.Fatalf("OpenRouter response-cache hit consumed tokens: %#v", hit.Usage)
	}
	t.Logf("OPENROUTER response-cache status=HIT age=%ss ttl=%ss",
		hit.Headers.Get("X-OpenRouter-Cache-Age"), hit.Headers.Get("X-OpenRouter-Cache-TTL"))
}

func testOpenAICompatibleCacheHitRate(t *testing.T) {
	requireRegressionEnabled(t, "OPENAI_COMPATIBLE")
	client, model, closeGateway := configuredGatewayClient(t, "macmini", "OPENAI_COMPAT_CACHE_MODEL", "gpt-5.6-luna")
	defer closeGateway()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cacheKey := uniqueID("openai-compatible")
	request := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{
			{
				Role: llmprovider.RoleSystem,
				ContentParts: []llmprovider.MessageContentPart{{
					"type": "text", "text": stablePrefix("openai-compatible"),
					"prompt_cache_breakpoint": map[string]any{"mode": "explicit"},
				}},
			},
			{Role: llmprovider.RoleUser, Content: "Reply with exactly OPENAI_COMPAT_CACHE_WARMUP."},
		},
		Extra: map[string]any{
			"prompt_cache_key":     cacheKey,
			"prompt_cache_options": map[string]any{"mode": "explicit", "ttl": "30m"},
		},
	}
	warmup, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages, warmup.Choices[0].Message, llmprovider.Message{
		Role: llmprovider.RoleUser, Content: "Reply with exactly OPENAI_COMPAT_CACHE_PROBE.",
	})
	probe, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	requireCacheHitRate(t, "OPENAI_COMPATIBLE", probe.Usage)
}

func configuredGatewayClient(t *testing.T, providerID, modelEnv, defaultModel string) (*llmprovider.Client, string, func()) {
	t.Helper()
	configPath := os.Getenv("GATEWAY_CONFIG_PATH")
	if configPath == "" {
		configPath = filepath.Join("..", "llm-provider.json")
	}
	config, err := gateway.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv(modelEnv)
	if model == "" {
		model = defaultModel
	}
	found := false
	var prefix string
	for index := range config.Providers {
		provider := &config.Providers[index]
		provider.Enabled = provider.ID == providerID
		if !provider.Enabled {
			continue
		}
		found = true
		prefix = provider.Prefix
		if prefix == "" {
			prefix = provider.ID
		}
		provider.Models = []string{model}
	}
	if !found {
		t.Fatalf("provider %q is missing from %s", providerID, configPath)
	}
	instance, err := gateway.New(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(instance.Handler())
	client := llmprovider.New(openai.New(openai.WithBaseURL(server.URL + "/v1")))
	closeGateway := func() {
		server.Close()
		_ = instance.Close()
	}
	return client, prefix + "/" + model, closeGateway
}

func requireRegressionEnabled(t *testing.T, provider string) {
	t.Helper()
	if os.Getenv("CACHE_REGRESSION_ALL") == "1" || os.Getenv("CACHE_REGRESSION_"+provider) == "1" {
		return
	}
	t.Skipf("set CACHE_REGRESSION_%s=1 or CACHE_REGRESSION_ALL=1 to run this external cache regression", provider)
}

func requireCacheHitRate(t *testing.T, provider string, usage llmprovider.Usage) {
	t.Helper()
	if usage.PromptDetails == nil {
		t.Fatalf("%s did not expose prompt_tokens_details: %#v", provider, usage)
	}
	if usage.PromptTokens <= 0 {
		t.Fatalf("%s reported no prompt tokens: %#v", provider, usage)
	}
	rate := float64(usage.PromptDetails.CachedTokens) / float64(usage.PromptTokens)
	minimum := minimumHitRate(t, provider)
	t.Logf("%s cache read=%d write=%d prompt=%d hit_rate=%.2f%% minimum=%.2f%%",
		provider, usage.PromptDetails.CachedTokens, usage.PromptDetails.CacheWriteTokens,
		usage.PromptTokens, rate*100, minimum*100)
	if rate < minimum {
		t.Fatalf("%s cache hit rate %.2f%% is below %.2f%%", provider, rate*100, minimum*100)
	}
}

func minimumHitRate(t *testing.T, provider string) float64 {
	t.Helper()
	value := os.Getenv("CACHE_MIN_HIT_RATE_" + provider)
	if value == "" {
		value = os.Getenv("CACHE_MIN_HIT_RATE")
	}
	if value == "" {
		return defaultMinimumCacheHitRate
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || parsed > 1 {
		t.Fatalf("invalid cache hit-rate threshold %q for %s; use a value from 0 to 1", value, provider)
	}
	return parsed
}

func stablePrefix(provider string) string {
	return strings.Repeat(fmt.Sprintf("Stable %s cache regression reference context. ", provider), 256)
}

func uniqueID(prefix string) string {
	return fmt.Sprintf("llm-provider-%s-%d", prefix, time.Now().UnixNano())
}
