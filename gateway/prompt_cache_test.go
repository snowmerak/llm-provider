package gateway

import (
	"net/http"
	"strings"
	"testing"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestPreparePromptCacheMapsConversationAffinity(t *testing.T) {
	tests := []struct {
		name       string
		route      *route
		extraField string
		header     string
	}{
		{name: "OpenAI compatible", route: &route{providerType: "openai-compatible"}, extraField: "prompt_cache_key"},
		{name: "OpenRouter", route: &route{providerType: "openrouter"}, extraField: "session_id"},
		{name: "Grok", route: &route{providerType: "grok"}, header: "X-Grok-Conv-Id"},
		{name: "xAI compatible", route: &route{providerType: "openai-compatible", modelCapabilityProfile: "xai"}, header: "X-Grok-Conv-Id"},
		{name: "explicit Grok kind", route: &route{providerType: "openai-compatible", providerKind: "grok"}, header: "X-Grok-Conv-Id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, conversationID, err := preparePromptCache(test.route, llmprovider.ChatRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(conversationID, gatewayCacheConversationPrefix) {
				t.Fatalf("conversation ID = %q", conversationID)
			}
			if test.extraField != "" && prepared.Extra[test.extraField] != conversationID {
				t.Fatalf("extra = %#v", prepared.Extra)
			}
			if test.header != "" && prepared.Headers.Get(test.header) != conversationID {
				t.Fatalf("headers = %#v", prepared.Headers)
			}

			prepared, secondID, err := preparePromptCache(test.route, llmprovider.ChatRequest{ConversationID: conversationID})
			if err != nil {
				t.Fatal(err)
			}
			if secondID != conversationID || prepared.ConversationID != "" {
				t.Fatalf("second request ID = %q, upstream conversation ID = %q", secondID, prepared.ConversationID)
			}
			if test.extraField != "" && prepared.Extra[test.extraField] != conversationID {
				t.Fatalf("second extra = %#v", prepared.Extra)
			}
			if test.header != "" && prepared.Headers.Get(test.header) != conversationID {
				t.Fatalf("second headers = %#v", prepared.Headers)
			}
		})
	}
}

func TestPreparePromptCachePreservesExplicitSettings(t *testing.T) {
	extra := map[string]any{"prompt_cache_key": "explicit"}
	request := llmprovider.ChatRequest{ConversationID: "provider-thread", Extra: extra}
	prepared, conversationID, err := preparePromptCache(&route{providerType: "openai-compatible"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if conversationID != "provider-thread" || prepared.ConversationID != "provider-thread" ||
		prepared.Extra["prompt_cache_key"] != "explicit" {
		t.Fatalf("prepared = %#v, conversation ID = %q", prepared, conversationID)
	}
	headers := http.Header{"X-Grok-Conv-Id": []string{"explicit-grok"}}
	prepared, _, err = preparePromptCache(&route{providerType: "grok"}, llmprovider.ChatRequest{
		ConversationID: "provider-thread", Headers: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Headers.Get("X-Grok-Conv-Id") != "explicit-grok" {
		t.Fatalf("headers = %#v", prepared.Headers)
	}
}

func TestPreparePromptCacheStripsSyntheticConversationWithExplicitSettings(t *testing.T) {
	for _, test := range []struct {
		name  string
		route *route
		req   llmprovider.ChatRequest
	}{
		{
			name:  "request setting",
			route: &route{providerType: "openai-compatible"},
			req: llmprovider.ChatRequest{
				ConversationID: gatewayCacheConversationPrefix + "request",
				Extra:          map[string]any{"prompt_cache_key": "explicit"},
			},
		},
		{
			name:  "provider setting",
			route: &route{providerType: "openai-compatible", cacheAffinityConfigured: true},
			req:   llmprovider.ChatRequest{ConversationID: gatewayCacheConversationPrefix + "provider"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, conversationID, err := preparePromptCache(test.route, test.req)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.ConversationID != "" || conversationID != test.req.ConversationID {
				t.Fatalf("prepared conversation ID = %q, lifecycle ID = %q", prepared.ConversationID, conversationID)
			}
		})
	}
}

func TestPreparePromptCacheDropsSyntheticConversationForGenericRoute(t *testing.T) {
	prepared, conversationID, err := preparePromptCache(
		&route{providerType: "openai-compatible", providerKind: "generic"},
		llmprovider.ChatRequest{ConversationID: gatewayCacheConversationPrefix + "old"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ConversationID != "" || conversationID != "" || prepared.Extra != nil {
		t.Fatalf("prepared = %#v, lifecycle ID = %q", prepared, conversationID)
	}
}

func TestPreparePromptCacheOpenRouterClaudeUsesOnlySessionAffinity(t *testing.T) {
	prepared, conversationID, err := preparePromptCache(&route{providerType: "openrouter"}, llmprovider.ChatRequest{
		Model: "anthropic/claude-sonnet-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Extra["session_id"] != conversationID {
		t.Fatalf("session affinity = %#v, lifecycle ID = %q", prepared.Extra["session_id"], conversationID)
	}
	if _, configured := prepared.Extra["cache_control"]; configured {
		t.Fatalf("OpenRouter Claude unexpectedly enabled Anthropic caching: %#v", prepared.Extra)
	}
}

func TestPreparePromptCacheDoesNotOverrideExplicitAffinity(t *testing.T) {
	tests := []struct {
		name    string
		route   *route
		request llmprovider.ChatRequest
	}{
		{name: "request OpenAI key", route: &route{providerType: "openai-compatible"}, request: llmprovider.ChatRequest{Extra: map[string]any{"prompt_cache_key": "explicit"}}},
		{name: "request OpenRouter session", route: &route{providerType: "openrouter"}, request: llmprovider.ChatRequest{Extra: map[string]any{"session_id": "explicit"}}},
		{name: "request Grok header", route: &route{providerType: "grok"}, request: llmprovider.ChatRequest{Headers: http.Header{"X-Grok-Conv-Id": []string{"explicit"}}}},
		{name: "provider default", route: &route{providerType: "openai-compatible", cacheAffinityConfigured: true}, request: llmprovider.ChatRequest{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, conversationID, err := preparePromptCache(test.route, test.request)
			if err != nil {
				t.Fatal(err)
			}
			if conversationID != "" || prepared.ConversationID != "" {
				t.Fatalf("prepared = %#v, conversation ID = %q", prepared, conversationID)
			}
		})
	}
}

func TestPreparePromptCacheEnablesAnthropicAutomaticCaching(t *testing.T) {
	for _, testRoute := range []*route{
		{providerType: "anthropic"},
		{providerType: "claude"},
		{providerType: "openai-compatible", providerKind: "anthropic"},
	} {
		prepared, conversationID, err := preparePromptCache(testRoute, llmprovider.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(conversationID, gatewayCacheConversationPrefix) {
			t.Fatalf("conversation ID = %q", conversationID)
		}
		cacheControl, ok := prepared.Extra["cache_control"].(map[string]any)
		if !ok || cacheControl["type"] != "ephemeral" {
			t.Fatalf("cache control = %#v", prepared.Extra["cache_control"])
		}

		prepared, secondID, err := preparePromptCache(testRoute, llmprovider.ChatRequest{ConversationID: conversationID})
		if err != nil {
			t.Fatal(err)
		}
		if secondID != conversationID || prepared.ConversationID != "" {
			t.Fatalf("second request ID = %q, upstream conversation ID = %q", secondID, prepared.ConversationID)
		}
		cacheControl, ok = prepared.Extra["cache_control"].(map[string]any)
		if !ok || cacheControl["type"] != "ephemeral" {
			t.Fatalf("second cache control = %#v", prepared.Extra["cache_control"])
		}
	}
}

func TestPreparePromptCachePreservesExplicitAnthropicCacheControl(t *testing.T) {
	explicit := map[string]any{"type": "ephemeral", "ttl": "1h"}
	prepared, conversationID, err := preparePromptCache(&route{providerType: "anthropic"}, llmprovider.ChatRequest{
		Extra: map[string]any{"cache_control": explicit},
	})
	if err != nil {
		t.Fatal(err)
	}
	cacheControl, ok := prepared.Extra["cache_control"].(map[string]any)
	if !strings.HasPrefix(conversationID, gatewayCacheConversationPrefix) || !ok ||
		cacheControl["type"] != "ephemeral" || cacheControl["ttl"] != "1h" {
		t.Fatalf("prepared = %#v, conversation ID = %q", prepared, conversationID)
	}

	prepared, conversationID, err = preparePromptCache(&route{
		providerType: "anthropic", cacheAffinityConfigured: true,
	}, llmprovider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(conversationID, gatewayCacheConversationPrefix) || prepared.Extra != nil {
		t.Fatalf("provider default was overridden: %#v, conversation ID = %q", prepared, conversationID)
	}
}

func TestPreparePromptCacheLeavesCodexAlone(t *testing.T) {
	for _, providerType := range []string{"codex", "codex-app-server"} {
		request := llmprovider.ChatRequest{ConversationID: "existing"}
		prepared, conversationID, err := preparePromptCache(&route{providerType: providerType}, request)
		if err != nil {
			t.Fatal(err)
		}
		if conversationID != "existing" || prepared.ConversationID != "existing" ||
			prepared.Extra != nil || prepared.Headers != nil {
			t.Fatalf("%s prepared = %#v, conversation ID = %q", providerType, prepared, conversationID)
		}
	}

	generated, err := newCacheConversationID()
	if err != nil {
		t.Fatal(err)
	}
	for _, providerType := range []string{"codex", "codex-app-server"} {
		prepared, conversationID, err := preparePromptCache(&route{providerType: providerType}, llmprovider.ChatRequest{ConversationID: generated})
		if err != nil {
			t.Fatal(err)
		}
		if conversationID != "" || prepared.ConversationID != "" {
			t.Fatalf("%s treated cache affinity as a Codex thread: %#v", providerType, prepared)
		}
	}
}
