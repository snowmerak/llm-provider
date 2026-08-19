package tests

import (
	"context"
	"os"
	"testing"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

// TestAutomaticGatewayPromptCacheHitRate exercises the Gateway policy rather
// than provider-specific cache fields supplied by the caller. It remains an
// opt-in external regression because each subtest makes paid provider calls.
func TestAutomaticGatewayPromptCacheHitRate(t *testing.T) {
	t.Run("Claude", func(t *testing.T) {
		testAutomaticGatewayPromptCacheHitRate(t, "CLAUDE", cacheProvider("CLAUDE", "claude"), "CLAUDE_CACHE_MODEL", "claude-sonnet-5")
	})
	t.Run("OpenAICompatible", func(t *testing.T) {
		testAutomaticGatewayPromptCacheHitRate(t, "OPENAI_COMPATIBLE", cacheProvider("OPENAI_COMPATIBLE", "macmini"), "OPENAI_COMPAT_CACHE_MODEL", "gpt-5.6-luna")
	})
	t.Run("OpenRouter", func(t *testing.T) {
		testAutomaticGatewayPromptCacheHitRate(t, "OPENROUTER", cacheProvider("OPENROUTER", "openrouter"), "OPENROUTER_CACHE_MODEL", "openai/gpt-4.1-mini")
	})
	t.Run("Grok", func(t *testing.T) {
		testAutomaticGatewayPromptCacheHitRate(t, "GROK", cacheProvider("GROK", "grok"), "GROK_CACHE_MODEL", "grok-4.5")
	})
}

func cacheProvider(regressionName, fallback string) string {
	if configured := os.Getenv("CACHE_REGRESSION_" + regressionName + "_PROVIDER"); configured != "" {
		return configured
	}
	return fallback
}

func testAutomaticGatewayPromptCacheHitRate(
	t *testing.T,
	regressionName string,
	providerID string,
	modelEnv string,
	defaultModel string,
) {
	t.Helper()
	requireRegressionEnabled(t, regressionName)
	client, model, closeGateway := configuredGatewayClient(t, providerID, modelEnv, defaultModel)
	defer closeGateway()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	request := llmprovider.ChatRequest{
		Model: model,
		Messages: []llmprovider.Message{
			{Role: llmprovider.RoleSystem, Content: stablePrefix("automatic-" + regressionName)},
			{Role: llmprovider.RoleUser, Content: "Reply with exactly AUTOMATIC_CACHE_WARMUP."},
		},
	}
	warmup, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if warmup.ConversationID == "" {
		t.Fatalf("%s Gateway returned no cache-affinity conversation_id", regressionName)
	}
	if len(warmup.Choices) == 0 {
		t.Fatalf("%s returned no choices: %#v", regressionName, warmup)
	}

	request.ConversationID = warmup.ConversationID
	request.Messages = append(request.Messages, warmup.Choices[0].Message, llmprovider.Message{
		Role: llmprovider.RoleUser, Content: "Reply with exactly AUTOMATIC_CACHE_PROBE.",
	})
	probe, err := client.Chat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if probe.ConversationID != warmup.ConversationID {
		t.Fatalf("%s cache-affinity ID changed: warmup=%q probe=%q",
			regressionName, warmup.ConversationID, probe.ConversationID)
	}
	requireCacheHitRate(t, regressionName, probe.Usage)
}
